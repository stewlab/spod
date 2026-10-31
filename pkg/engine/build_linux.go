// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/moby/patternmatcher/ignorefile"
)

// buildStage is a stage's working state.
type buildStage struct {
	name   string
	rootfs string
	cfg    v1.Config
	author string
	// args holds the stage's declared ARGs; unset ones are absent.
	args map[string]string
	// declared lists every declared ARG, set or not.
	declared map[string]bool
}

func (s *buildStage) lookup(name string) (string, bool) {
	for i := len(s.cfg.Env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(s.cfg.Env[i], "="); ok && k == name {
			return v, true
		}
	}
	v, ok := s.args[name]
	return v, ok
}

// runEnv is the environment of RUN: the image environment plus build
// arguments, which are not persisted in the image.
func (s *buildStage) runEnv() []string {
	env := append([]string(nil), s.cfg.Env...)
	names := make([]string, 0, len(s.args))
	for k := range s.args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !hasEnv(env, k) {
			env = append(env, k+"="+s.args[k])
		}
	}
	return env
}

func setEnv(env []string, key, value string) []string {
	for i, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

// builder holds one build's inputs and stages.
type builder struct {
	e         *Engine
	opts      BuildOptions
	stdio     IO
	cf        *containerfile
	dir       string
	ignore    []string
	globals   map[string]string
	usedArgs  map[string]bool
	stages    []*buildStage
	step      int
	steps     int
	runCount  int
	escape    rune
	buildID   string
	stageByNm map[string]int
}

// Build builds an image from a Containerfile and stores it under
// opts.Tag. Each RUN executes in a container on the stage's rootfs under
// the default mapping (container root is the caller) with host networking.
// The result is a single squashed layer over the stage's filesystem; file
// ownership is recorded in the layer, and extended attributes, including
// file capabilities, are not. Stages the target does not need are skipped.
func (e *Engine) Build(ctx context.Context, opts BuildOptions, stdio IO) (_ *Image, retErr error) {
	if stdio.Stdout == nil {
		stdio.Stdout = io.Discard
	}
	if stdio.Stderr == nil {
		stdio.Stderr = io.Discard
	}
	if opts.Tag == "" {
		return nil, errors.New("build needs a tag")
	}
	ref, err := imageReference(opts.Tag)
	if err != nil {
		return nil, err
	}
	if opts.Context == "" {
		opts.Context = "."
	}
	if opts.Context, err = filepath.Abs(opts.Context); err != nil {
		return nil, err
	}
	if info, err := os.Stat(opts.Context); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("build context %s is not a directory", opts.Context)
	}
	file := opts.File
	if file == "" {
		for _, name := range []string{"Containerfile", "Dockerfile"} {
			if _, err := os.Stat(filepath.Join(opts.Context, name)); err == nil {
				file = filepath.Join(opts.Context, name)
				break
			}
		}
		if file == "" {
			return nil, fmt.Errorf("no Containerfile or Dockerfile in %s", opts.Context)
		}
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	cf, err := parseContainerfile(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	b := &builder{e: e, opts: opts, stdio: stdio, cf: cf, escape: cf.Escape, usedArgs: map[string]bool{}, stageByNm: map[string]int{}}
	if b.ignore, err = readIgnore(opts.Context); err != nil {
		return nil, err
	}
	if err := b.resolveGlobals(); err != nil {
		return nil, err
	}
	target := len(cf.Stages) - 1
	for i, s := range cf.Stages {
		if s.Name != "" {
			b.stageByNm[s.Name] = i
		}
	}
	if opts.Target != "" {
		i, ok := b.stageByNm[strings.ToLower(opts.Target)]
		if !ok {
			return nil, fmt.Errorf("target stage %q not found", opts.Target)
		}
		target = i
	}
	needed := b.neededStages(target)
	for i := range cf.Stages {
		if needed[i] {
			b.steps += 1 + len(cf.Stages[i].Instructions)
		}
	}

	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	b.buildID = hex.EncodeToString(id[:])
	b.dir = filepath.Join(e.Root, "builds", b.buildID)
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err := e.Discard(b.dir); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	b.stages = make([]*buildStage, len(cf.Stages))
	for i := 0; i <= target; i++ {
		if !needed[i] {
			continue
		}
		if err := b.runStage(ctx, i); err != nil {
			return nil, err
		}
	}
	for name := range opts.BuildArgs {
		if !b.usedArgs[name] {
			fmt.Fprintf(stdio.Stderr, "[Warning] build argument %s was not consumed\n", name)
		}
	}
	return b.commit(ctx, ref, b.stages[target])
}

func readIgnore(contextDir string) ([]string, error) {
	for _, name := range []string{".containerignore", ".dockerignore"} {
		f, err := os.Open(filepath.Join(contextDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		patterns, err := ignorefile.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return patterns, nil
	}
	return nil, nil
}

func (b *builder) resolveGlobals() error {
	b.globals = map[string]string{}
	lookup := func(k string) (string, bool) { v, ok := b.globals[k]; return v, ok }
	for _, ins := range b.cf.GlobalArgs {
		kvs, err := keyValues(ins, lookup, b.escape)
		if err != nil {
			return err
		}
		for _, kv := range kvs {
			if v, ok := b.opts.BuildArgs[kv[0]]; ok {
				b.usedArgs[kv[0]] = true
				b.globals[kv[0]] = v
			} else if kv[1] != "\x00" {
				b.globals[kv[0]] = kv[1]
			}
		}
	}
	return nil
}

func (b *builder) globalLookup(k string) (string, bool) {
	v, ok := b.globals[k]
	return v, ok
}

// stageRef resolves a FROM base or COPY --from value to a stage index.
func (b *builder) stageRef(ref string, before int) (int, bool) {
	if i, ok := b.stageByNm[strings.ToLower(ref)]; ok && i < before {
		return i, true
	}
	if n, err := strconv.Atoi(ref); err == nil && n >= 0 && n < before {
		return n, true
	}
	return 0, false
}

// neededStages marks target and the stages it depends on through FROM and
// COPY --from.
func (b *builder) neededStages(target int) map[int]bool {
	needed := map[int]bool{}
	var visit func(int)
	visit = func(i int) {
		if needed[i] {
			return
		}
		needed[i] = true
		s := b.cf.Stages[i]
		base, _ := expandWord(strings.Fields(s.From.Rest)[0], b.globalLookup, b.escape)
		if j, ok := b.stageRef(base, i); ok {
			visit(j)
		}
		for _, ins := range s.Instructions {
			if from, ok := ins.Flags["from"]; ok && ins.Cmd == "copy" {
				from, _ = expandWord(from, b.globalLookup, b.escape)
				if j, ok := b.stageRef(from, i); ok {
					visit(j)
				}
			}
		}
	}
	visit(target)
	return needed
}

func (b *builder) logStep(ins cfInstruction) {
	b.step++
	fmt.Fprintf(b.stdio.Stderr, "STEP %d/%d: %s\n", b.step, b.steps, ins.Original)
}

func (b *builder) userns(ctx context.Context, op string, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return inUserns(usernsRequest{Op: op, Payload: data}, nil)
}

func (b *builder) runStage(ctx context.Context, i int) error {
	def := b.cf.Stages[i]
	b.logStep(def.From)
	s := &buildStage{name: def.Name, args: map[string]string{}, declared: map[string]bool{}}
	b.stages[i] = s
	base, err := expandWord(strings.Fields(def.From.Rest)[0], b.globalLookup, b.escape)
	if err != nil {
		return fmt.Errorf("line %d: %w", def.From.Line, err)
	}
	stageDir := filepath.Join(b.dir, "stage-"+strconv.Itoa(i))
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return err
	}
	s.rootfs = filepath.Join(stageDir, "rootfs")
	switch j, isStage := b.stageRef(base, i); {
	case isStage:
		if err := os.Mkdir(s.rootfs, 0o755); err != nil {
			return err
		}
		if err := b.userns(ctx, "build-clone", buildClonePayload{Source: b.stages[j].rootfs, Dest: s.rootfs}); err != nil {
			return err
		}
		s.cfg = cloneConfig(b.stages[j].cfg)
	case strings.EqualFold(base, "scratch"):
		if err := os.Mkdir(s.rootfs, 0o755); err != nil {
			return err
		}
	default:
		img, err := b.e.PrepareRootfs(ctx, base, s.rootfs)
		if err != nil {
			return err
		}
		s.cfg = cloneConfig(img.Config)
	}
	cmdSet := false
	for _, ins := range def.Instructions {
		b.logStep(ins)
		if err := b.execute(ctx, s, ins, &cmdSet); err != nil {
			if ins.Line > 0 {
				return fmt.Errorf("line %d: %s: %w", ins.Line, strings.ToUpper(ins.Cmd), err)
			}
			return err
		}
	}
	return nil
}

func cloneConfig(c v1.Config) v1.Config {
	data, _ := json.Marshal(c)
	var out v1.Config
	_ = json.Unmarshal(data, &out)
	return out
}

func (b *builder) execute(ctx context.Context, s *buildStage, ins cfInstruction, cmdSet *bool) error {
	expand := func(w string) (string, error) { return expandWord(w, s.lookup, b.escape) }
	words := func() ([]string, error) {
		if ins.IsJSON {
			return ins.JSON, nil
		}
		return expandWords(ins.Rest, s.lookup, b.escape, true)
	}
	command := func() []string {
		if ins.IsJSON {
			return ins.JSON
		}
		shell := s.cfg.Shell
		if len(shell) == 0 {
			shell = []string{"/bin/sh", "-c"}
		}
		return append(append([]string(nil), shell...), ins.Rest)
	}
	switch ins.Cmd {
	case "arg":
		kvs, err := keyValues(ins, s.lookup, b.escape)
		if err != nil {
			return err
		}
		for _, kv := range kvs {
			s.declared[kv[0]] = true
			if v, ok := b.opts.BuildArgs[kv[0]]; ok {
				b.usedArgs[kv[0]] = true
				s.args[kv[0]] = v
			} else if kv[1] != "\x00" {
				s.args[kv[0]] = kv[1]
			} else if v, ok := b.globals[kv[0]]; ok {
				s.args[kv[0]] = v
			}
		}
	case "env":
		kvs, err := keyValues(ins, s.lookup, b.escape)
		if err != nil {
			return err
		}
		for _, kv := range kvs {
			s.cfg.Env = setEnv(s.cfg.Env, kv[0], kv[1])
		}
	case "label":
		kvs, err := keyValues(ins, s.lookup, b.escape)
		if err != nil {
			return err
		}
		if s.cfg.Labels == nil {
			s.cfg.Labels = map[string]string{}
		}
		for _, kv := range kvs {
			s.cfg.Labels[kv[0]] = kv[1]
		}
	case "maintainer":
		s.author = ins.Rest
	case "workdir":
		dir, err := expand(ins.Rest)
		if err != nil {
			return err
		}
		if !path.IsAbs(dir) {
			dir = path.Join("/", s.cfg.WorkingDir, dir)
		}
		s.cfg.WorkingDir = path.Clean(dir)
		uid, gid, err := resolveUser(s.rootfs, s.cfg.User)
		if err != nil {
			uid, gid = 0, 0
		}
		return b.userns(ctx, "build-mkdir", buildMkdirPayload{Rootfs: s.rootfs, Path: s.cfg.WorkingDir, UID: uid, GID: gid})
	case "user":
		u, err := expand(ins.Rest)
		if err != nil {
			return err
		}
		s.cfg.User = u
	case "expose":
		ports, err := words()
		if err != nil {
			return err
		}
		if s.cfg.ExposedPorts == nil {
			s.cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range ports {
			if !strings.Contains(p, "/") {
				p += "/tcp"
			}
			s.cfg.ExposedPorts[p] = struct{}{}
		}
	case "volume":
		vols, err := words()
		if err != nil {
			return err
		}
		if s.cfg.Volumes == nil {
			s.cfg.Volumes = map[string]struct{}{}
		}
		for _, v := range vols {
			s.cfg.Volumes[v] = struct{}{}
		}
	case "stopsignal":
		sig, err := expand(ins.Rest)
		if err != nil {
			return err
		}
		s.cfg.StopSignal = sig
	case "cmd":
		s.cfg.Cmd = command()
		*cmdSet = true
	case "entrypoint":
		s.cfg.Entrypoint = command()
		if !*cmdSet {
			s.cfg.Cmd = nil
		}
	case "shell":
		s.cfg.Shell = ins.JSON
	case "onbuild":
		s.cfg.OnBuild = append(s.cfg.OnBuild, ins.Rest)
	case "healthcheck":
		hc, err := parseHealthcheck(ins)
		if err != nil {
			return err
		}
		s.cfg.Healthcheck = hc
	case "run":
		if n := ins.Flags["network"]; n != "" && n != "host" && n != "default" {
			return fmt.Errorf("unsupported network %q: spod only supports host networking", n)
		}
		return b.run(ctx, s, command())
	case "copy", "add":
		args, err := words()
		if err != nil {
			return err
		}
		return b.copy(ctx, s, ins, args)
	}
	return nil
}

func parseHealthcheck(ins cfInstruction) (*v1.HealthConfig, error) {
	fields := strings.Fields(ins.Rest)
	if len(fields) == 0 {
		return nil, errors.New("HEALTHCHECK needs NONE or CMD")
	}
	if strings.EqualFold(fields[0], "none") {
		return &v1.HealthConfig{Test: []string{"NONE"}}, nil
	}
	if !strings.EqualFold(fields[0], "cmd") {
		return nil, errors.New("HEALTHCHECK needs NONE or CMD")
	}
	hc := &v1.HealthConfig{}
	rest := strings.TrimSpace(ins.Rest[len(fields[0]):])
	var args []string
	if strings.HasPrefix(rest, "[") && json.Unmarshal([]byte(rest), &args) == nil {
		hc.Test = append([]string{"CMD"}, args...)
	} else {
		hc.Test = []string{"CMD-SHELL", rest}
	}
	for name, dst := range map[string]*time.Duration{
		"interval": &hc.Interval, "timeout": &hc.Timeout,
		"start-period": &hc.StartPeriod, "start-interval": nil,
	} {
		if v, ok := ins.Flags[name]; ok && dst != nil {
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("--%s: %w", name, err)
			}
			*dst = d
		}
	}
	if v, ok := ins.Flags["retries"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("--retries: %w", err)
		}
		hc.Retries = n
	}
	return hc, nil
}

func (b *builder) run(ctx context.Context, s *buildStage, argv []string) error {
	uid, gid, err := resolveUser(s.rootfs, s.cfg.User)
	if err != nil {
		return err
	}
	b.runCount++
	name := fmt.Sprintf("build-%s-%d", b.buildID, b.runCount)
	cwd := s.cfg.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	rm := &runtimeMeta{Argv: argv, Env: s.runEnv(), Cwd: cwd, UID: uid, GID: gid, Hostname: "spod-build"}
	if !hasEnv(rm.Env, "PATH") {
		rm.Env = append(rm.Env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	stateDir := filepath.Join(b.dir, "state", strconv.Itoa(b.runCount))
	code, err := b.e.runInit(ctx, stateDir, name, s.rootfs, rm,
		IO{Stdout: b.stdio.Stdout, Stderr: b.stdio.Stderr}, false, func(int) {})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("command %q exited with status %d", strings.Join(argv, " "), code)
	}
	return nil
}

func (b *builder) copy(ctx context.Context, s *buildStage, ins cfInstruction, args []string) error {
	if len(args) < 2 {
		return errors.New("needs at least one source and a destination")
	}
	lookup := s.lookup
	p := buildCopyPayload{Rootfs: s.rootfs, SrcRoot: b.opts.Context, Ignore: b.ignore, Extract: ins.Cmd == "add"}
	if from, ok := ins.Flags["from"]; ok {
		from, err := expandWord(from, lookup, b.escape)
		if err != nil {
			return err
		}
		if j, ok := b.stageRef(from, len(b.stages)); ok && b.stages[j] != nil {
			p.SrcRoot = b.stages[j].rootfs
		} else {
			img, err := b.e.Pull(ctx, from)
			if err != nil {
				return fmt.Errorf("--from=%s: %w", from, err)
			}
			p.SrcRoot = img.Rootfs
		}
		p.Ignore = nil
	}
	if chown, ok := ins.Flags["chown"]; ok {
		chown, err := expandWord(chown, lookup, b.escape)
		if err != nil {
			return err
		}
		if p.UID, p.GID, err = resolveUser(s.rootfs, chown); err != nil {
			return fmt.Errorf("--chown: %w", err)
		}
	}
	if chmod, ok := ins.Flags["chmod"]; ok {
		m, err := strconv.ParseUint(chmod, 8, 32)
		if err != nil || m > 0o7777 {
			return fmt.Errorf("--chmod must be an octal mode: %s", chmod)
		}
		mode := uint32(m)
		p.Chmod = &mode
	}
	dest := args[len(args)-1]
	dirDest := strings.HasSuffix(dest, "/") || dest == "." || strings.HasSuffix(dest, "/.")
	if !path.IsAbs(dest) {
		dest = path.Join("/", s.cfg.WorkingDir, dest)
	}
	dest = path.Clean(dest)
	if dirDest && dest != "/" {
		dest += "/"
	}
	p.Dest = dest
	for _, src := range args[:len(args)-1] {
		u, err := url.Parse(src)
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			if ins.Cmd != "add" {
				return errors.New("URL sources need ADD")
			}
			file, err := b.download(ctx, u)
			if err != nil {
				return err
			}
			p.Files = append(p.Files, file)
			continue
		}
		if err == nil && u.Scheme != "" && len(u.Scheme) > 1 {
			return fmt.Errorf("unsupported source %s", src)
		}
		p.Sources = append(p.Sources, src)
	}
	return b.userns(ctx, "build-copy", p)
}

// download fetches an ADD URL into the build directory; like other engines
// it is stored with mode 0600 under the URL's base name.
func (b *builder) download(ctx context.Context, u *url.URL) (string, error) {
	name := path.Base(u.Path)
	if name == "/" || name == "." || name == "" {
		return "", fmt.Errorf("cannot derive a file name from %s", u)
	}
	dir, err := os.MkdirTemp(b.dir, "download-")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", u, resp.Status)
	}
	file := filepath.Join(dir, name)
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(f, resp.Body)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", err
	}
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		_ = os.Chtimes(file, lm, lm)
	}
	return file, nil
}

// resolveUser resolves a USER or --chown value (name or ID, with an
// optional :group) against the rootfs's /etc/passwd and /etc/group.
func resolveUser(rootfs, spec string) (int, int, error) {
	if spec == "" {
		return 0, 0, nil
	}
	user, group, hasGroup := strings.Cut(spec, ":")
	lookup := func(file, name string) ([]string, error) {
		p, err := securejoin.SecureJoin(rootfs, file)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) >= 3 && fields[0] == name {
				return fields, nil
			}
		}
		return nil, fmt.Errorf("%s not found in %s", name, file)
	}
	uid, err := strconv.Atoi(user)
	gid := -1
	if err != nil {
		fields, err := lookup("/etc/passwd", user)
		if err != nil {
			return 0, 0, err
		}
		if uid, err = strconv.Atoi(fields[2]); err != nil {
			return 0, 0, fmt.Errorf("invalid UID for %s", user)
		}
		if len(fields) >= 4 {
			gid, _ = strconv.Atoi(fields[3])
		}
	} else if !hasGroup {
		// A numeric user takes its primary group from passwd if listed.
		if data, err := securejoin.SecureJoin(rootfs, "/etc/passwd"); err == nil {
			if content, err := os.ReadFile(data); err == nil {
				for _, line := range strings.Split(string(content), "\n") {
					fields := strings.Split(line, ":")
					if len(fields) >= 4 && fields[2] == user {
						gid, _ = strconv.Atoi(fields[3])
						break
					}
				}
			}
		}
	}
	if hasGroup {
		if gid, err = strconv.Atoi(group); err != nil {
			fields, err := lookup("/etc/group", group)
			if err != nil {
				return 0, 0, err
			}
			if gid, err = strconv.Atoi(fields[2]); err != nil {
				return 0, 0, fmt.Errorf("invalid GID for %s", group)
			}
		}
	}
	if gid < 0 {
		gid = uid
	}
	return uid, gid, nil
}

// commit squashes the stage's rootfs into one layer and stores the image.
func (b *builder) commit(ctx context.Context, ref interface{ Name() string }, s *buildStage) (*Image, error) {
	fmt.Fprintf(b.stdio.Stderr, "COMMIT %s\n", ref.Name())
	layerPath := filepath.Join(b.dir, "layer.tar")
	if err := b.userns(ctx, "build-commit", buildCommitPayload{Rootfs: s.rootfs, Out: layerPath}); err != nil {
		return nil, err
	}
	layer, err := tarball.LayerFromFile(layerPath)
	if err != nil {
		return nil, err
	}
	cfg := &v1.ConfigFile{
		Architecture: runtime.GOARCH,
		OS:           "linux",
		Author:       s.author,
		Created:      v1.Time{Time: time.Now().UTC()},
		Config:       s.cfg,
	}
	img, err := mutate.ConfigFile(empty.Image, cfg)
	if err != nil {
		return nil, err
	}
	img, err = mutate.Append(img, mutate.Addendum{
		Layer:   layer,
		History: v1.History{Created: cfg.Created, CreatedBy: "spod build", Comment: "squashed"},
	})
	if err != nil {
		return nil, err
	}
	r, err := imageReference(b.opts.Tag)
	if err != nil {
		return nil, err
	}
	unlock, err := b.e.lockImages(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return b.e.storeImage(ctx, r, img)
}
