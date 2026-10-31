// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

// Build-time filesystem operations. RUN steps leave files owned by
// subordinate IDs the caller cannot read or chown, so COPY, ADD, WORKDIR,
// stage cloning, and layer commits run in the storage user namespace
// helper, where every container ID is the namespace ID of the same number.

package engine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/moby/patternmatcher"
	"golang.org/x/sys/unix"
)

func init() {
	usernsOps["build-copy"] = func(req usernsRequest, _ io.Writer) error {
		var p buildCopyPayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return err
		}
		return buildCopy(p)
	}
	usernsOps["build-clone"] = func(req usernsRequest, _ io.Writer) error {
		var p buildClonePayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return err
		}
		c := &copier{rootfs: p.Dest, links: map[imageFileIdentity]string{}}
		return c.copyInto(p.Source, "/")
	}
	usernsOps["build-mkdir"] = func(req usernsRequest, _ io.Writer) error {
		var p buildMkdirPayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return err
		}
		c := &copier{rootfs: p.Rootfs, chown: true, uid: p.UID, gid: p.GID}
		_, err := c.ensureDir(p.Path, 0o755)
		return err
	}
	usernsOps["build-commit"] = func(req usernsRequest, _ io.Writer) error {
		var p buildCommitPayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return err
		}
		return commitTar(p.Rootfs, p.Out)
	}
}

// buildCopyPayload is a COPY or ADD: Sources are paths or patterns
// relative to SrcRoot (the build context or another stage's rootfs).
type buildCopyPayload struct {
	Rootfs  string   `json:"rootfs"`
	Dest    string   `json:"dest"`
	SrcRoot string   `json:"src_root"`
	Sources []string `json:"sources"`
	// Files are downloaded ADD URLs, copied as single files.
	Files   []string `json:"files,omitempty"`
	Ignore  []string `json:"ignore,omitempty"`
	UID     int      `json:"uid"`
	GID     int      `json:"gid"`
	Chmod   *uint32  `json:"chmod,omitempty"`
	Extract bool     `json:"extract,omitempty"`
}

type buildClonePayload struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

type buildMkdirPayload struct {
	Rootfs string `json:"rootfs"`
	Path   string `json:"path"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
}

type buildCommitPayload struct {
	Rootfs string `json:"rootfs"`
	Out    string `json:"out"`
}

// copier writes into a rootfs by container path, resolving every parent
// through the rootfs's own symlinks so nothing lands outside it.
type copier struct {
	rootfs string
	// chown forces uid/gid; otherwise source ownership is kept.
	chown    bool
	uid, gid int
	chmod    *fs.FileMode
	filter   func(rel string) (skip bool)
	links    map[imageFileIdentity]string
}

func (c *copier) resolve(ctrPath string) (string, error) {
	return securejoin.SecureJoin(c.rootfs, ctrPath)
}

// resolveParent resolves ctrPath's parent and appends its final component
// unresolved, so the entry itself is never followed.
func (c *copier) resolveParent(ctrPath string) (string, error) {
	ctrPath = path.Clean("/" + ctrPath)
	if ctrPath == "/" {
		return c.rootfs, nil
	}
	parent, err := c.resolve(path.Dir(ctrPath))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, path.Base(ctrPath)), nil
}

func (c *copier) owner(st *syscall.Stat_t) (int, int) {
	if c.chown || st == nil {
		return c.uid, c.gid
	}
	return int(st.Uid), int(st.Gid)
}

func (c *copier) mode(m fs.FileMode) fs.FileMode {
	if c.chmod != nil {
		return *c.chmod
	}
	return m & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// ensureDir makes ctrPath a directory, creating missing parents owned by
// the copy's owner, and returns its host path. Existing directories are
// left as they are.
func (c *copier) ensureDir(ctrPath string, mode fs.FileMode) (string, error) {
	ctrPath = path.Clean("/" + ctrPath)
	abs, err := c.resolve(ctrPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("%s exists and is not a directory", ctrPath)
		}
		return abs, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := c.ensureDir(path.Dir(ctrPath), 0o755); err != nil {
		return "", err
	}
	abs, err = c.resolve(ctrPath)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(abs, 0o700); err != nil {
		return "", err
	}
	uid, gid := c.owner(nil)
	if err := os.Lchown(abs, uid, gid); err != nil {
		return "", err
	}
	return abs, os.Chmod(abs, mode)
}

// copyInto copies src (a file, or a directory's contents) to the container
// path dst.
func (c *copier) copyInto(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return c.copyEntry(src, info, dst)
	}
	if _, err := c.ensureDir(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if c.filter != nil && c.filter(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return c.copyEntry(p, info, path.Join(dst, filepath.ToSlash(rel)))
	})
}

func (c *copier) copyEntry(src string, info fs.FileInfo, dst string) error {
	st, _ := info.Sys().(*syscall.Stat_t)
	uid, gid := c.owner(st)
	if info.IsDir() {
		abs, err := c.resolve(dst)
		if err != nil {
			return err
		}
		existing, err := os.Stat(abs)
		switch {
		case err == nil && !existing.IsDir():
			return fmt.Errorf("cannot copy directory over %s", dst)
		case errors.Is(err, os.ErrNotExist):
			if _, err := c.ensureDir(path.Dir(dst), 0o755); err != nil {
				return err
			}
			if abs, err = c.resolve(dst); err != nil {
				return err
			}
			if err := os.Mkdir(abs, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if err := os.Lchown(abs, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(abs, c.mode(info.Mode())); err != nil {
			return err
		}
		return os.Chtimes(abs, info.ModTime(), info.ModTime())
	}
	if _, err := c.ensureDir(path.Dir(path.Clean("/"+dst)), 0o755); err != nil {
		return err
	}
	abs, err := c.resolveParent(dst)
	if err != nil {
		return err
	}
	if existing, err := os.Lstat(abs); err == nil {
		if existing.IsDir() {
			return fmt.Errorf("cannot copy over directory %s", dst)
		}
		if err := os.Remove(abs); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, abs); err != nil {
			return err
		}
		return os.Lchown(abs, uid, gid)
	case info.Mode().IsRegular():
		if st != nil && st.Nlink > 1 && c.links != nil {
			id := imageFileIdentity{uint64(st.Dev), st.Ino}
			if first, ok := c.links[id]; ok {
				return os.Link(first, abs)
			}
			c.links[id] = abs
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		if err := errors.Join(copyErr, out.Close()); err != nil {
			return err
		}
		if err := os.Lchown(abs, uid, gid); err != nil {
			return err
		}
		// chmod after chown: chown clears setuid/setgid.
		if err := os.Chmod(abs, c.mode(info.Mode())); err != nil {
			return err
		}
		return os.Chtimes(abs, info.ModTime(), info.ModTime())
	default:
		// Sockets, fifos, and devices have no place in a rootless image.
		return nil
	}
}

func hasGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

func buildCopy(p buildCopyPayload) error {
	c := &copier{rootfs: p.Rootfs, chown: true, uid: p.UID, gid: p.GID, links: map[imageFileIdentity]string{}}
	if p.Chmod != nil {
		m := fs.FileMode(*p.Chmod) & fs.ModePerm
		if *p.Chmod&0o4000 != 0 {
			m |= fs.ModeSetuid
		}
		if *p.Chmod&0o2000 != 0 {
			m |= fs.ModeSetgid
		}
		if *p.Chmod&0o1000 != 0 {
			m |= fs.ModeSticky
		}
		c.chmod = &m
	}
	var pm *patternmatcher.PatternMatcher
	if len(p.Ignore) > 0 {
		var err error
		if pm, err = patternmatcher.New(p.Ignore); err != nil {
			return fmt.Errorf("ignore file: %w", err)
		}
	}
	ignored := func(abs string) bool {
		if pm == nil {
			return false
		}
		rel, err := filepath.Rel(p.SrcRoot, abs)
		if err != nil || rel == "." {
			return false
		}
		match, _ := pm.MatchesOrParentMatches(rel)
		return match
	}
	if pm != nil {
		c.filter = ignored
	}

	var matches []string
	for _, pattern := range p.Sources {
		clean := path.Clean("/" + pattern)
		dir, base := path.Split(clean)
		if hasGlob(dir) {
			return fmt.Errorf("wildcards are only supported in the last path component: %s", pattern)
		}
		if !hasGlob(base) {
			abs, err := securejoin.SecureJoin(p.SrcRoot, clean)
			if err != nil {
				return err
			}
			if _, err := os.Stat(abs); err != nil || clean != "/" && ignored(abs) {
				return fmt.Errorf("source %s not found in the build context", pattern)
			}
			matches = append(matches, abs)
			continue
		}
		dirAbs, err := securejoin.SecureJoin(p.SrcRoot, dir)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dirAbs)
		if err != nil {
			return fmt.Errorf("source %s: %w", pattern, err)
		}
		n := 0
		for _, e := range entries {
			if ok, _ := path.Match(base, e.Name()); !ok {
				continue
			}
			abs, err := securejoin.SecureJoin(p.SrcRoot, path.Join(dir, e.Name()))
			if err != nil {
				return err
			}
			if ignored(filepath.Join(dirAbs, e.Name())) {
				continue
			}
			matches = append(matches, abs)
			n++
		}
		if n == 0 {
			return fmt.Errorf("no source files match %s", pattern)
		}
	}
	matches = append(matches, p.Files...)

	dirDest := strings.HasSuffix(p.Dest, "/")
	if len(matches) > 1 && !dirDest {
		return fmt.Errorf("copying more than one source requires a destination ending in /: %s", p.Dest)
	}
	dest := path.Clean(p.Dest)
	for _, src := range matches {
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := c.copyInto(src, dest); err != nil {
				return err
			}
			continue
		}
		if p.Extract && !contains(p.Files, src) {
			if ok, err := extractArchive(c, src, dest); err != nil {
				return err
			} else if ok {
				continue
			}
		}
		target := dest
		if dirDest {
			target = path.Join(dest, filepath.Base(src))
		} else if abs, err := c.resolve(dest); err == nil {
			if st, err := os.Stat(abs); err == nil && st.IsDir() {
				target = path.Join(dest, filepath.Base(src))
			}
		}
		if err := c.copyInto(src, target); err != nil {
			return err
		}
	}
	return nil
}

// extractArchive unpacks a local tar archive (plain, gzip, or bzip2) for
// ADD into the container directory dest. ok is false when src is not an
// archive, so it is copied as a file instead.
func extractArchive(c *copier, src, dest string) (ok bool, err error) {
	f, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	magic, _ := br.Peek(512)
	var r io.Reader = br
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			return false, nil
		}
		defer gz.Close()
		r = gz
	case bytes.HasPrefix(magic, []byte("BZh")):
		r = bzip2.NewReader(br)
	case len(magic) >= 262 && string(magic[257:262]) == "ustar":
	default:
		return false, nil
	}
	if _, err := c.ensureDir(dest, 0o755); err != nil {
		return true, err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return true, fmt.Errorf("extract %s: %w", filepath.Base(src), err)
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return true, err
		}
		ctr := path.Join(dest, filepath.ToSlash(name))
		uid, gid := h.Uid, h.Gid
		if c.chown {
			uid, gid = c.uid, c.gid
		}
		mode := tarMode(h.Mode)
		if c.chmod != nil {
			mode = *c.chmod
		}
		if _, err := c.ensureDir(path.Dir(ctr), 0o755); err != nil {
			return true, err
		}
		abs, err := c.resolveParent(ctr)
		if err != nil {
			return true, err
		}
		if h.Typeflag == tar.TypeDir {
			if abs, err = c.ensureDir(ctr, mode); err != nil {
				return true, err
			}
		} else {
			if existing, err := os.Lstat(abs); err == nil && !existing.IsDir() {
				if err := os.Remove(abs); err != nil {
					return true, err
				}
			}
			switch h.Typeflag {
			case tar.TypeReg, tar.TypeRegA:
				out, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
				if err != nil {
					return true, err
				}
				_, copyErr := io.Copy(out, tr)
				if err := errors.Join(copyErr, out.Close()); err != nil {
					return true, err
				}
			case tar.TypeSymlink:
				if err := os.Symlink(h.Linkname, abs); err != nil {
					return true, err
				}
			case tar.TypeLink:
				target, err := archivePath(h.Linkname)
				if err != nil {
					return true, err
				}
				first, err := c.resolveParent(path.Join(dest, filepath.ToSlash(target)))
				if err != nil {
					return true, err
				}
				if err := os.Link(first, abs); err != nil {
					return true, err
				}
				continue
			default:
				continue
			}
		}
		if err := os.Lchown(abs, uid, gid); err != nil {
			return true, err
		}
		if h.Typeflag != tar.TypeSymlink {
			if err := os.Chmod(abs, mode); err != nil {
				return true, err
			}
			_ = os.Chtimes(abs, h.ModTime, h.ModTime)
		}
	}
}

// commitTar writes rootfs as an uncompressed layer tar at out, keeping
// each entry's owner, mode, and hardlinks. Sockets and devices are skipped;
// extended attributes, including file capabilities, are not recorded.
func commitTar(rootfs, out string) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	tw := tar.NewWriter(bw)
	links := map[imageFileIdentity]string{}
	walkErr := filepath.WalkDir(rootfs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(rootfs, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uname, h.Gname = "", ""
		h.Format = tar.FormatPAX
		st := info.Sys().(*syscall.Stat_t)
		h.Uid, h.Gid = int(st.Uid), int(st.Gid)
		if info.Mode().IsRegular() && st.Nlink > 1 {
			id := imageFileIdentity{uint64(st.Dev), st.Ino}
			if first, ok := links[id]; ok {
				h.Typeflag, h.Linkname, h.Size = tar.TypeLink, first, 0
				return tw.WriteHeader(h)
			}
			links[id] = h.Name
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, in)
		return errors.Join(copyErr, in.Close())
	})
	return errors.Join(walkErr, tw.Close(), bw.Flush(), f.Close())
}
