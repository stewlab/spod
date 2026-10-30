// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

// Package engine's Linux runtime executes OCI images as rootless containers
// using github.com/opencontainers/runc/libcontainer. It requires an
// unprivileged user namespace (CLONE_NEWUSER) and does not use cgroups or a
// container network namespace: containers share the host network by default
// (no bridge). This file is only built with cgo enabled, because libcontainer's
// namespace bootstrap (nsenter) and its userns helpers are cgo-only.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/console"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/specconv"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

const (
	statusCreated = "created"
	statusRunning = "running"
	statusExited  = "exited"
)

// runtimeMeta is the engine-private, fully resolved execution spec persisted
// alongside the shared Container metadata. It lets Start re-launch a stopped
// container even if its source image has since been removed.
type runtimeMeta struct {
	Argv     []string          `json:"argv"`
	Env      []string          `json:"env"`
	Cwd      string            `json:"cwd"`
	UID      int               `json:"uid"`
	GID      int               `json:"gid"`
	Hostname string            `json:"hostname"`
	Mounts   []Mount           `json:"mounts"`
	TTY      bool              `json:"tty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// perContainerLocks guards create/state mutations within this process; a flock
// on the container directory guards across processes.
var perContainerLocks sync.Map

func (e *Engine) containerLock(name string) *sync.Mutex {
	m, _ := perContainerLocks.LoadOrStore(e.Root+"\x00"+name, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (e *Engine) containerDir(name string) string { return filepath.Join(e.Root, "containers", name) }
func (e *Engine) rootfsDir(name string) string    { return filepath.Join(e.containerDir(name), "rootfs") }
func (e *Engine) stateDir(name string) string     { return filepath.Join(e.containerDir(name), "state") }
func (e *Engine) metaPath(name string) string {
	return filepath.Join(e.containerDir(name), "container.json")
}
func (e *Engine) runtimePath(name string) string {
	return filepath.Join(e.containerDir(name), "runtime.json")
}

// lockContainer takes the in-process mutex and an exclusive flock on the
// container directory, serializing concurrent create/state mutations.
func (e *Engine) lockContainer(ctx context.Context, name string) (func(), error) {
	mu := e.containerLock(name)
	mu.Lock()
	if err := os.MkdirAll(e.containerDir(name), 0700); err != nil {
		mu.Unlock()
		return nil, err
	}
	unlockFile, err := lockImageProcesses(ctx, filepath.Join(e.containerDir(name), ".lock"))
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	return func() {
		unlockFile()
		mu.Unlock()
	}, nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Engine) loadContainer(name string) (*Container, error) {
	data, err := os.ReadFile(e.metaPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no such container: %s", name)
		}
		return nil, err
	}
	var c Container
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("corrupt metadata for %s: %w", name, err)
	}
	return &c, nil
}

func (e *Engine) loadRuntimeMeta(name string) (*runtimeMeta, error) {
	data, err := os.ReadFile(e.runtimePath(name))
	if err != nil {
		return nil, err
	}
	var m runtimeMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// refreshStatus reconciles persisted metadata with the live libcontainer state.
func (e *Engine) refreshStatus(c *Container) {
	cont, err := libcontainer.Load(e.stateDir(c.Name), c.Name)
	if err != nil {
		if c.Status == statusRunning {
			c.Status = statusExited
			c.PID = 0
		}
		return
	}
	st, err := cont.Status()
	if err != nil {
		return
	}
	switch st {
	case libcontainer.Running, libcontainer.Created, libcontainer.Paused:
		c.Status = statusRunning
		if pids, err := cont.Processes(); err == nil && len(pids) > 0 {
			c.PID = pids[0]
		}
	default:
		c.Status = statusExited
		c.PID = 0
	}
}

// parseUser parses a numeric "uid", "uid:gid", image "user" value. Only numeric
// IDs are supported in rootless mode; named users would require resolving the
// image's /etc/passwd which spod does not do. An empty value means 0:0.
func parseUser(user string) (int, int, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(user, ":", 2)
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("unsupported non-numeric user %q: spod requires a numeric uid[:gid]", user)
	}
	gid := uid
	if len(parts) == 2 {
		gid, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, fmt.Errorf("unsupported non-numeric group in user %q", user)
		}
	}
	return uid, gid, nil
}

// idMappings returns the uid/gid mappings for the container's user namespace.
// The current user is always mapped to container root. When newuidmap/newgidmap
// and /etc/subuid+/etc/subgid ranges are available, additional ranges are added
// so files owned by non-root uids inside the image map to delegated subids.
// Otherwise only the single mapping is used.
func idMappings() ([]specs.LinuxIDMapping, []specs.LinuxIDMapping) {
	euid, egid := os.Geteuid(), os.Getegid()
	uidMaps := []specs.LinuxIDMapping{{HostID: uint32(euid), ContainerID: 0, Size: 1}}
	gidMaps := []specs.LinuxIDMapping{{HostID: uint32(egid), ContainerID: 0, Size: 1}}

	haveUmap := lookPath("newuidmap") && lookPath("newgidmap")
	if !haveUmap {
		return uidMaps, gidMaps
	}
	if base, size, ok := subIDRange("/etc/subuid"); ok {
		uidMaps = append(uidMaps, specs.LinuxIDMapping{HostID: base, ContainerID: 1, Size: size})
	}
	if base, size, ok := subIDRange("/etc/subgid"); ok {
		gidMaps = append(gidMaps, specs.LinuxIDMapping{HostID: base, ContainerID: 1, Size: size})
	}
	return uidMaps, gidMaps
}

func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// subIDRange reads the first range for the current user from /etc/subuid or
// /etc/subgid. The container id 1 maps to the first delegated host id (the
// range base), covering Size ids.
func subIDRange(path string) (base uint32, size uint32, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	uname := ""
	if u := os.Getenv("USER"); u != "" {
		uname = u
	}
	uidStr := strconv.Itoa(os.Getuid())
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) != 3 {
			continue
		}
		if fields[0] != uname && fields[0] != uidStr {
			continue
		}
		b, err1 := strconv.ParseUint(fields[1], 10, 32)
		s, err2 := strconv.ParseUint(fields[2], 10, 32)
		if err1 != nil || err2 != nil || s == 0 {
			continue
		}
		return uint32(b), uint32(s), true
	}
	return 0, 0, false
}

// resolveProcess merges image config with run options into a concrete process
// and the engine-private runtimeMeta.
func resolveProcess(opts RunOptions, img *Image) (*runtimeMeta, error) {
	entrypoint := img.Config.Entrypoint
	if opts.Entrypoint != nil {
		entrypoint = opts.Entrypoint
	}
	cmd := img.Config.Cmd
	if len(opts.Command) > 0 {
		cmd = opts.Command
	}
	argv := append(append([]string{}, entrypoint...), cmd...)
	if len(argv) == 0 {
		return nil, errors.New("no command specified and image has no entrypoint or cmd")
	}

	env := append([]string{}, img.Config.Env...)
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	env = append(env, opts.Env...)

	cwd := opts.WorkDir
	if cwd == "" {
		cwd = img.Config.WorkingDir
	}
	if cwd == "" {
		cwd = "/"
	}

	userStr := opts.User
	if userStr == "" {
		userStr = img.Config.User
	}
	uid, gid, err := parseUser(userStr)
	if err != nil {
		return nil, err
	}

	return &runtimeMeta{
		Argv:     argv,
		Env:      env,
		Cwd:      cwd,
		UID:      uid,
		GID:      gid,
		Hostname: opts.Hostname,
		Mounts:   opts.Mounts,
		TTY:      opts.TTY,
		Labels:   opts.Labels,
	}, nil
}

func hasEnv(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

// buildConfig constructs a rootless libcontainer configuration. Networking uses
// the host namespace (no NEWNET); cgroups are disabled.
func (e *Engine) buildConfig(name, rootfs string, rm *runtimeMeta) (*configs.Config, error) {
	if rm.Hostname == "" {
		rm.Hostname = name
	}
	spec := specconv.Example()
	spec.Root.Path = rootfs
	spec.Root.Readonly = false
	spec.Hostname = rm.Hostname
	spec.Process.Terminal = rm.TTY
	spec.Process.Args = rm.Argv
	spec.Process.Env = rm.Env
	spec.Process.Cwd = rm.Cwd
	spec.Process.User = specs.User{UID: uint32(rm.UID), GID: uint32(rm.GID)}

	for _, m := range rm.Mounts {
		opt := []string{"rbind"}
		if m.ReadOnly {
			opt = append(opt, "ro")
		} else {
			opt = append(opt, "rw")
		}
		spec.Mounts = append(spec.Mounts, specs.Mount{
			Source:      m.Source,
			Destination: m.Target,
			Type:        "none",
			Options:     opt,
		})
	}

	// Rootless conversion drops the network namespace (host networking) and the
	// cgroup namespace, and adds the user namespace.
	specconv.ToRootless(spec)
	uidMaps, gidMaps := idMappings()
	spec.Linux.UIDMappings = uidMaps
	spec.Linux.GIDMappings = gidMaps

	cfg, err := specconv.CreateLibcontainerConfig(&specconv.CreateOpts{
		CgroupName:      name,
		Spec:            spec,
		RootlessEUID:    true,
		RootlessCgroups: true,
	})
	if err != nil {
		return nil, err
	}
	// No cgroup controller is available for rootless users without delegation;
	// clear device rules so libcontainer does not try to program them.
	cfg.Cgroups.Rootless = true
	if cfg.Cgroups.Resources != nil {
		cfg.Cgroups.Resources.Devices = nil
	}
	return cfg, nil
}

// ttyHandler forwards a container's pty master to the host terminal in raw mode.
type ttyHandler struct {
	hostConsole console.Console
	console     console.Console
	epoller     *console.Epoller
	closers     []interface{ Close() error }
}

func newTTYHandler() (*ttyHandler, error) {
	t := &ttyHandler{}
	for _, s := range []*os.File{os.Stderr, os.Stdout, os.Stdin} {
		if c, err := console.ConsoleFromFile(s); err == nil {
			t.hostConsole = c
			return t, nil
		} else if !errors.Is(err, console.ErrNotAConsole) {
			return nil, err
		}
	}
	return nil, errors.New("TTY requested but stdio is not a terminal; run without -t or attach a terminal")
}

// recv receives the pty master over socket and wires it to the host terminal.
func (t *ttyHandler) recv(socket *os.File) error {
	f, err := utils.RecvFile(socket)
	if err != nil {
		return err
	}
	cons, err := console.ConsoleFromFile(f)
	if err != nil {
		return err
	}
	_ = console.ClearONLCR(cons.Fd())
	epoller, err := console.NewEpoller()
	if err != nil {
		return err
	}
	epollConsole, err := epoller.Add(cons)
	if err != nil {
		return err
	}
	go func() { _ = epoller.Wait() }()
	go func() { _, _ = ioCopy(epollConsole, os.Stdin) }()
	go func() { _, _ = ioCopy(os.Stdout, epollConsole) }()
	if err := t.hostConsole.SetRaw(); err != nil {
		return fmt.Errorf("failed to set terminal raw mode: %w", err)
	}
	t.epoller = epoller
	t.console = epollConsole
	t.closers = []interface{ Close() error }{epollConsole}
	t.resize()
	return nil
}

func (t *ttyHandler) resize() {
	if t.console != nil && t.hostConsole != nil {
		_ = t.console.ResizeFrom(t.hostConsole)
	}
}

func (t *ttyHandler) Close() {
	if t.hostConsole != nil {
		_ = t.hostConsole.Reset()
	}
	for _, c := range t.closers {
		_ = c.Close()
	}
	if t.epoller != nil {
		_ = t.epoller.Close()
	}
}

// startProcess configures stdio or a pty for process p and starts it inside
// cont. When the process wants a TTY, a console socket pair is used to receive
// the pty master; the returned ttyHandler (possibly nil) must be closed by the
// caller and watched for SIGWINCH resizes.
func startProcess(cont *libcontainer.Container, p *libcontainer.Process, io IO, tty bool) (*ttyHandler, error) {
	if tty {
		p.Stdin, p.Stdout, p.Stderr = nil, nil, nil
		th, err := newTTYHandler()
		if err != nil {
			return nil, err
		}
		parent, child, err := utils.NewSockPair("console")
		if err != nil {
			th.Close()
			return nil, err
		}
		p.ConsoleSocket = child
		if err := cont.Run(p); err != nil {
			parent.Close()
			child.Close()
			th.Close()
			return nil, err
		}
		child.Close()
		if err := th.recv(parent); err != nil {
			parent.Close()
			th.Close()
			return nil, err
		}
		parent.Close()
		return th, nil
	}
	p.Stdin, p.Stdout, p.Stderr = io.Stdin, io.Stdout, io.Stderr
	if err := cont.Run(p); err != nil {
		return nil, err
	}
	return nil, nil
}

func ioCopy(dst interface{ Write([]byte) (int, error) }, src interface{ Read([]byte) (int, error) }) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// watchResize forwards host SIGWINCH to the container pty until stop is closed.
func watchResize(th *ttyHandler, stop <-chan struct{}) {
	if th == nil {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, unix.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-ch:
			th.resize()
		case <-stop:
			return
		}
	}
}

// Run creates a container with its own persistent rootfs and metadata, starts
// it, and blocks until the init process exits, returning its exit status. The
// caller (typically a detached monitor re-exec) owns stdio. Metadata reaches
// the running state (with PID) before Run begins waiting, so other processes
// can observe and signal it.
func (e *Engine) Run(ctx context.Context, opts RunOptions, io IO) (int, error) {
	if err := ValidateName(opts.Name); err != nil {
		return -1, err
	}
	if opts.Network != "" && opts.Network != "host" {
		return -1, fmt.Errorf("unsupported network %q: spod only supports host networking", opts.Network)
	}
	if opts.TTY && opts.Remove {
		// permitted; handled below
	}

	unlock, err := e.lockContainer(ctx, opts.Name)
	if err != nil {
		return -1, err
	}
	locked := true
	releaseLock := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer releaseLock()

	if _, err := os.Stat(e.metaPath(opts.Name)); err == nil {
		return -1, fmt.Errorf("container name already in use: %s", opts.Name)
	}

	// PrepareRootfs atomically renames the populated rootfs into place and
	// requires the destination to be absent, so do not pre-create it. Once
	// the destination exists, a container that never reaches running is
	// discarded so a failed run does not leave it registered under its name.
	neverStarted := func() {
		_ = e.Discard(e.containerDir(opts.Name))
	}
	rootfs := e.rootfsDir(opts.Name)
	img, err := e.PrepareRootfs(ctx, opts.Image, rootfs)
	if err != nil {
		return -1, err
	}
	rm, err := resolveProcess(opts, img)
	if err != nil {
		neverStarted()
		return -1, err
	}
	if rm.Hostname == "" {
		rm.Hostname = opts.Name
	}

	meta := &Container{
		Name:       opts.Name,
		Image:      opts.Image,
		Status:     statusCreated,
		Entrypoint: rm.Argv,
		Created:    time.Now(),
		Options:    opts,
	}
	// Once metadata exists on disk, a container that never reaches running is
	// discarded so a failed run does not leave it registered under its name.
	if err := writeJSONAtomic(e.runtimePath(opts.Name), rm); err != nil {
		neverStarted()
		return -1, err
	}
	if err := writeJSONAtomic(e.metaPath(opts.Name), meta); err != nil {
		neverStarted()
		return -1, err
	}

	cfg, err := e.buildConfig(opts.Name, rootfs, rm)
	if err != nil {
		neverStarted()
		return -1, err
	}
	if err := os.MkdirAll(e.stateDir(opts.Name), 0700); err != nil {
		neverStarted()
		return -1, err
	}
	cont, err := libcontainer.Create(e.stateDir(opts.Name), opts.Name, cfg)
	if err != nil {
		neverStarted()
		return -1, err
	}

	proc := &libcontainer.Process{
		Args:          rm.Argv,
		Env:           rm.Env,
		Cwd:           rm.Cwd,
		UID:           rm.UID,
		GID:           rm.GID,
		Init:          true,
		ConsoleWidth:  0,
		ConsoleHeight: 0,
	}
	th, err := startProcess(cont, proc, io, rm.TTY)
	if err != nil {
		cont.Destroy()
		neverStarted()
		return -1, err
	}

	pid, _ := proc.Pid()
	meta.Status = statusRunning
	meta.PID = pid
	_ = writeJSONAtomic(e.metaPath(opts.Name), meta)

	// Release the create lock so Stop/Exec from other processes can proceed
	// while we wait for the container to exit.
	releaseLock()

	stopResize := make(chan struct{})
	go watchResize(th, stopResize)

	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cont.Signal(unix.SIGKILL)
		case <-ctxDone:
		}
	}()

	state, waitErr := proc.Wait()
	close(ctxDone)
	close(stopResize)
	if th != nil {
		th.Close()
	}

	exitCode := 0
	if state != nil {
		exitCode = state.ExitCode()
	} else if waitErr != nil {
		exitCode = -1
	}

	// Finalize state under the lock.
	relock, lerr := e.lockContainer(context.Background(), opts.Name)
	if lerr == nil {
		if c, err := e.loadContainer(opts.Name); err == nil {
			c.Status = statusExited
			c.PID = 0
			_ = writeJSONAtomic(e.metaPath(opts.Name), c)
		}
		_ = cont.Destroy()
		if opts.Remove {
			_ = e.Discard(e.containerDir(opts.Name))
		}
		relock()
	}
	return exitCode, nil
}

// Start re-launches a previously created, stopped container using its persisted
// resolved configuration and existing rootfs, blocking until it exits.
func (e *Engine) Start(ctx context.Context, name string, io IO) (int, error) {
	if err := ValidateName(name); err != nil {
		return -1, err
	}
	unlock, err := e.lockContainer(ctx, name)
	if err != nil {
		return -1, err
	}
	locked := true
	releaseLock := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer releaseLock()

	meta, err := e.loadContainer(name)
	if err != nil {
		return -1, err
	}
	e.refreshStatus(meta)
	if meta.Status == statusRunning {
		return -1, fmt.Errorf("container already running: %s", name)
	}
	rm, err := e.loadRuntimeMeta(name)
	if err != nil {
		return -1, fmt.Errorf("cannot start %s: missing runtime metadata: %w", name, err)
	}

	// Remove any stale libcontainer state from a prior run.
	if old, err := libcontainer.Load(e.stateDir(name), name); err == nil {
		_ = old.Destroy()
	}

	cfg, err := e.buildConfig(name, e.rootfsDir(name), rm)
	if err != nil {
		return -1, err
	}
	if err := os.MkdirAll(e.stateDir(name), 0700); err != nil {
		return -1, err
	}
	cont, err := libcontainer.Create(e.stateDir(name), name, cfg)
	if err != nil {
		return -1, err
	}
	proc := &libcontainer.Process{
		Args: rm.Argv,
		Env:  rm.Env,
		Cwd:  rm.Cwd,
		UID:  rm.UID,
		GID:  rm.GID,
		Init: true,
	}
	th, err := startProcess(cont, proc, io, rm.TTY)
	if err != nil {
		cont.Destroy()
		return -1, err
	}
	pid, _ := proc.Pid()
	meta.Status = statusRunning
	meta.PID = pid
	_ = writeJSONAtomic(e.metaPath(name), meta)
	releaseLock()

	stopResize := make(chan struct{})
	go watchResize(th, stopResize)
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cont.Signal(unix.SIGKILL)
		case <-ctxDone:
		}
	}()

	state, _ := proc.Wait()
	close(ctxDone)
	close(stopResize)
	if th != nil {
		th.Close()
	}
	exitCode := 0
	if state != nil {
		exitCode = state.ExitCode()
	}
	relock, lerr := e.lockContainer(context.Background(), name)
	if lerr == nil {
		if c, err := e.loadContainer(name); err == nil {
			c.Status = statusExited
			c.PID = 0
			_ = writeJSONAtomic(e.metaPath(name), c)
		}
		_ = cont.Destroy()
		relock()
	}
	return exitCode, nil
}

// Exec runs an additional process inside a running container.
func (e *Engine) Exec(ctx context.Context, name string, opts ExecOptions, io IO) (int, error) {
	if err := ValidateName(name); err != nil {
		return -1, err
	}
	meta, err := e.loadContainer(name)
	if err != nil {
		return -1, err
	}
	e.refreshStatus(meta)
	if meta.Status != statusRunning {
		return -1, fmt.Errorf("container is not running: %s", name)
	}
	cont, err := libcontainer.Load(e.stateDir(name), name)
	if err != nil {
		return -1, fmt.Errorf("cannot load running container %s: %w", name, err)
	}
	if len(opts.Command) == 0 {
		return -1, errors.New("exec requires a command")
	}
	uid, gid := 0, 0
	if opts.User != "" {
		uid, gid, err = parseUser(opts.User)
		if err != nil {
			return -1, err
		}
	}
	rm, _ := e.loadRuntimeMeta(name)
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if rm != nil {
		env = rm.Env
	}
	env = append(env, opts.Env...)
	cwd := opts.WorkDir
	if cwd == "" && rm != nil {
		cwd = rm.Cwd
	}
	if cwd == "" {
		cwd = "/"
	}

	proc := &libcontainer.Process{
		Args: opts.Command,
		Env:  env,
		Cwd:  cwd,
		UID:  uid,
		GID:  gid,
		Init: false,
	}
	th, err := startProcess(cont, proc, io, opts.TTY)
	if err != nil {
		return -1, err
	}
	stopResize := make(chan struct{})
	go watchResize(th, stopResize)
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = proc.Signal(unix.SIGKILL)
		case <-ctxDone:
		}
	}()
	state, _ := proc.Wait()
	close(ctxDone)
	close(stopResize)
	if th != nil {
		th.Close()
	}
	if state != nil {
		return state.ExitCode(), nil
	}
	return -1, nil
}

// Stop sends SIGTERM to the container's init process and, if it does not exit
// within timeout, SIGKILL.
func (e *Engine) Stop(ctx context.Context, name string, timeout time.Duration) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock, err := e.lockContainer(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()

	meta, err := e.loadContainer(name)
	if err != nil {
		return err
	}
	cont, err := libcontainer.Load(e.stateDir(name), name)
	if err != nil {
		// Not running; just mark exited.
		meta.Status = statusExited
		meta.PID = 0
		return writeJSONAtomic(e.metaPath(name), meta)
	}
	st, _ := cont.Status()
	if st == libcontainer.Stopped {
		meta.Status = statusExited
		meta.PID = 0
		return writeJSONAtomic(e.metaPath(name), meta)
	}

	if err := cont.Signal(unix.SIGTERM); err != nil && !errors.Is(err, libcontainer.ErrNotRunning) {
		return err
	}
	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s, err := cont.Status(); err != nil || s == libcontainer.Stopped {
				goto done
			}
		case <-deadline:
			_ = cont.Signal(unix.SIGKILL)
			goto done
		case <-ctx.Done():
			_ = cont.Signal(unix.SIGKILL)
			goto done
		}
	}
done:
	// Give KILL a moment to take effect.
	for i := 0; i < 40; i++ {
		if s, err := cont.Status(); err != nil || s == libcontainer.Stopped {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	meta.Status = statusExited
	meta.PID = 0
	return writeJSONAtomic(e.metaPath(name), meta)
}

// Remove deletes a container's metadata and rootfs. A running container is only
// removed when force is true (it is killed first). Removed resources are moved
// to the store trash, never deleted outright.
func (e *Engine) Remove(ctx context.Context, name string, force bool) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock, err := e.lockContainer(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := os.Stat(e.metaPath(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no such container: %s", name)
		}
		return err
	}
	if cont, err := libcontainer.Load(e.stateDir(name), name); err == nil {
		st, _ := cont.Status()
		if st != libcontainer.Stopped {
			if !force {
				return fmt.Errorf("container is running (use force to remove): %s", name)
			}
			_ = cont.Signal(unix.SIGKILL)
			for i := 0; i < 80; i++ {
				if s, err := cont.Status(); err != nil || s == libcontainer.Stopped {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
		}
		_ = cont.Destroy()
	}
	return e.Discard(e.containerDir(name))
}

// Inspect returns the current metadata for a container, reconciled with its
// live runtime state.
func (e *Engine) Inspect(ctx context.Context, name string) (*Container, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	meta, err := e.loadContainer(name)
	if err != nil {
		return nil, err
	}
	e.refreshStatus(meta)
	return meta, nil
}

// List returns all containers in the store with reconciled status.
func (e *Engine) List(ctx context.Context) ([]Container, error) {
	entries, err := os.ReadDir(filepath.Join(e.Root, "containers"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Container
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		meta, err := e.loadContainer(ent.Name())
		if err != nil {
			continue
		}
		e.refreshStatus(meta)
		out = append(out, *meta)
	}
	return out, nil
}
