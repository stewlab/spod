// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Image describes an image and its unpacked filesystem. Pull and InspectImage
// return an immutable shared Rootfs; PrepareRootfs returns the writable copy.
type Image struct {
	Reference string
	Digest    string
	Rootfs    string
	Config    v1.Config
	Created   time.Time
	Platform  v1.Platform
}

// Serialize local store operations, including across Engine instances.
var imageStoreLocks sync.Map

func (e *Engine) imageLock() chan struct{} {
	lock, _ := imageStoreLocks.LoadOrStore(filepath.Clean(e.Root), make(chan struct{}, 1))
	return lock.(chan struct{})
}

func (e *Engine) lockImages(ctx context.Context) (func(), error) {
	lock := e.imageLock()
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	unlock, err := lockImageProcesses(ctx, filepath.Join(e.Root, "images", ".lock"))
	if err != nil {
		<-lock
		return nil, err
	}
	return func() {
		unlock()
		<-lock
	}, nil
}

func imageReference(ref string) (name.Reference, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	return r, nil
}

func (e *Engine) imageDir(ref name.Reference) string {
	sum := sha256.Sum256([]byte(ref.Name()))
	return filepath.Join(e.Root, "images", hex.EncodeToString(sum[:]))
}

// Pull downloads a Linux image for the host architecture without a daemon.
// A cached reference is reused; remove it explicitly to refresh a mutable tag.
func (e *Engine) Pull(ctx context.Context, ref string) (*Image, error) {
	unlock, err := e.lockImages(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return e.pullImage(ctx, ref)
}

func (e *Engine) pullImage(ctx context.Context, ref string) (_ *Image, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := imageReference(ref)
	if err != nil {
		return nil, err
	}
	dir := e.imageDir(r)
	if img, err := readImage(dir); err == nil {
		return img, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// An incomplete entry is never accepted as a cache hit.
	if _, err := os.Lstat(dir); err == nil {
		if err := e.Discard(dir); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	src, err := remote.Image(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithPlatform(platform))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", r.Name(), err)
	}
	return e.storeImage(ctx, r, src)
}

// storeImage extracts src into the store and publishes it under r. The
// destination is staged inside images/ and renamed into place atomically; a
// concurrent writer that wins the rename is returned instead.
func (e *Engine) storeImage(ctx context.Context, r name.Reference, src v1.Image) (_ *Image, retErr error) {
	digest, err := src.Digest()
	if err != nil {
		return nil, err
	}
	cfg, err := src.ConfigFile()
	if err != nil {
		return nil, err
	}
	if cfg.OS != "linux" || cfg.Architecture != runtime.GOARCH {
		return nil, fmt.Errorf("image platform %s/%s does not match linux/%s", cfg.OS, cfg.Architecture, runtime.GOARCH)
	}
	manifest, err := src.RawManifest()
	if err != nil {
		return nil, err
	}
	config, err := src.RawConfigFile()
	if err != nil {
		return nil, err
	}
	dir := e.imageDir(r)
	stage, err := os.MkdirTemp(filepath.Join(e.Root, "images"), ".stage-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if stage != "" {
			if err := e.Discard(stage); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("retain unfinished image: %w", err))
			}
		}
	}()
	rootfs := filepath.Join(stage, "rootfs")
	if err := os.Mkdir(rootfs, 0700); err != nil {
		return nil, err
	}
	stream := mutate.Extract(src)
	err = extractRootfs(ctx, rootfs, stream)
	closeErr := stream.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return nil, fmt.Errorf("extract %s: %w", r.Name(), err)
	}
	img := &Image{
		Reference: r.Name(), Digest: digest.String(), Rootfs: filepath.Join(dir, "rootfs"),
		Config: cfg.Config, Created: time.Now().UTC(),
		Platform: v1.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant},
	}
	metadata, err := json.Marshal(img)
	if err != nil {
		return nil, err
	}
	for file, data := range map[string][]byte{"manifest.json": manifest, "config.json": config, "image.json": metadata} {
		if err := os.WriteFile(filepath.Join(stage, file), data, 0600); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The staged rootfs extracted cleanly, so it is a valid replacement;
	// any previous image under this reference is retained in trash now.
	if _, err := os.Lstat(dir); err == nil {
		if err := e.Discard(dir); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(stage, dir); err != nil {
		if winner, readErr := readImage(dir); readErr == nil {
			return winner, nil
		}
		return nil, fmt.Errorf("publish image: %w", err)
	}
	stage = ""
	return img, nil
}

func readImage(dir string) (*Image, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile("image.json")
	if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal(data, &img); err != nil {
		return nil, fmt.Errorf("invalid image metadata: %w", err)
	}
	if img.Reference == "" || img.Digest == "" {
		return nil, errors.New("incomplete image metadata")
	}
	for _, file := range []string{"manifest.json", "config.json", "rootfs"} {
		info, err := root.Lstat(file)
		if err != nil {
			return nil, err
		}
		if file == "rootfs" && !info.IsDir() || file != "rootfs" && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid cached image %s", file)
		}
	}
	img.Rootfs = filepath.Join(dir, "rootfs")
	return &img, nil
}

// InspectImage reads local metadata without contacting a registry.
func (e *Engine) InspectImage(ref string) (*Image, error) {
	lock := e.imageLock()
	lock <- struct{}{}
	defer func() { <-lock }()
	r, err := imageReference(ref)
	if err != nil {
		return nil, err
	}
	return readImage(e.imageDir(r))
}

// Images lists only complete published images.
func (e *Engine) Images() ([]Image, error) {
	lock := e.imageLock()
	lock <- struct{}{}
	defer func() { <-lock }()
	entries, err := os.ReadDir(filepath.Join(e.Root, "images"))
	if err != nil {
		return nil, err
	}
	images := []Image{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		img, err := readImage(filepath.Join(e.Root, "images", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("inspect cached image %s: %w", entry.Name(), err)
		}
		images = append(images, *img)
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Reference < images[j].Reference })
	return images, nil
}

// RemoveImage retains the image in trash and refuses references used by a
// container. Unreadable container metadata fails closed.
func (e *Engine) RemoveImage(ref string) error {
	unlock, err := e.lockImages(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	r, err := imageReference(ref)
	if err != nil {
		return err
	}
	img, err := readImage(e.imageDir(r))
	if err != nil {
		return err
	}
	err = filepath.WalkDir(filepath.Join(e.Root, "containers"), func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "rootfs" || entry.Name() == "state" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "container.json" {
			return nil
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var container Container
		if err := json.Unmarshal(data, &container); err != nil {
			return err
		}
		for _, used := range []string{container.Image, container.Options.Image} {
			candidate, err := imageReference(used)
			usedDigest := ""
			if digest, ok := candidate.(name.Digest); ok {
				usedDigest = digest.DigestStr()
			}
			if used != "" && (used == img.Digest || usedDigest == img.Digest || err == nil && candidate.Name() == img.Reference) {
				return fmt.Errorf("image %s is used by container %s", img.Reference, container.Name)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return e.Discard(e.imageDir(r))
}

// PrepareRootfs creates an independent writable filesystem, never hardlinking
// files to the shared cache. dest must not exist, or must be an empty directory
// on the storage filesystem. The returned Image.Rootfs points to dest.
func (e *Engine) PrepareRootfs(ctx context.Context, ref, dest string) (_ *Image, retErr error) {
	unlock, err := e.lockImages(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	img, err := e.pullImage(ctx, ref)
	if err != nil {
		return nil, err
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	imagesPath := filepath.Join(e.Root, "images")
	if rel, err := filepath.Rel(imagesPath, dest); err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("rootfs destination must be outside the image store")
	}
	if info, err := os.Lstat(dest); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("rootfs destination is not a directory: %s", dest)
		}
		entries, err := os.ReadDir(dest)
		if err != nil {
			return nil, err
		}
		if len(entries) != 0 {
			return nil, fmt.Errorf("rootfs destination is not empty: %s", dest)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return nil, err
	}
	// Staging stays inside the store so a failed copy can use Discard.
	stage, err := os.MkdirTemp(filepath.Join(e.Root, "images"), ".copy-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if stage != "" {
			if err := e.Discard(stage); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	if err := copyRootfs(ctx, img.Rootfs, stage); err != nil {
		return nil, err
	}
	if err := os.Rename(stage, dest); err != nil {
		return nil, fmt.Errorf("publish writable rootfs (destination must share the store filesystem): %w", err)
	}
	stage = ""
	prepared := *img
	prepared.Rootfs = dest
	return &prepared, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

type imageFileIdentity struct {
	device, inode uint64
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func archivePath(name string) (string, error) {
	// A NUL byte or an absolute path can never be a safe relative member.
	// Backslashes are ordinary filename characters on Linux and appear in
	// real images: systemd escapes unit names on disk, e.g.
	// system-systemd\x2dcryptsetup.slice.
	if strings.ContainsRune(name, '\x00') || path.IsAbs(name) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return "", fmt.Errorf("unsafe archive path %q", name)
		}
	}
	name = path.Clean(name)
	if name == "." {
		return ".", nil
	}
	if !filepath.IsLocal(filepath.FromSlash(name)) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return filepath.FromSlash(name), nil
}

// Reject symlink parents even when the link is internal. Absolute container
// symlinks are valid entries, but must never redirect subsequent archive writes.
func safeParents(root *os.Root, name string, directories map[string]fs.FileMode) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(parent, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(current, 0700); err != nil {
				return err
			}
			directories[current] = 0755
		} else if err != nil {
			return err
		} else if !info.IsDir() {
			return fmt.Errorf("archive parent is not a directory: %s", current)
		}
	}
	return nil
}

func tarMode(mode int64) fs.FileMode {
	perm := fs.FileMode(mode) & fs.ModePerm
	if mode&04000 != 0 {
		perm |= fs.ModeSetuid
	}
	if mode&02000 != 0 {
		perm |= fs.ModeSetgid
	}
	if mode&01000 != 0 {
		perm |= fs.ModeSticky
	}
	return perm
}

func extractRootfs(ctx context.Context, dest string, reader io.Reader) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	reader = contextReader{ctx, reader}
	tr := tar.NewReader(reader)
	directories := map[string]fs.FileMode{".": 0755}
	type hardlink struct{ name, target string }
	var links []hardlink
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := archivePath(header.Name)
		if err != nil {
			return err
		}
		if name == "." && header.Typeflag != tar.TypeDir {
			return errors.New("non-directory archive root")
		}
		if err := safeParents(root, name, directories); err != nil {
			return err
		}
		// mutate.Extract emits the merged filesystem; duplicates must not be
		// allowed to reopen an existing symlink or truncate a hardlink target.
		if info, err := root.Lstat(name); err == nil {
			if header.Typeflag != tar.TypeDir || !info.IsDir() {
				return fmt.Errorf("duplicate archive entry: %s", name)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
			directories[name] = tarMode(header.Mode)
		case tar.TypeReg, tar.TypeRegA:
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			modeErr := f.Chmod(tarMode(header.Mode))
			closeErr := f.Close()
			if err := errors.Join(copyErr, modeErr, closeErr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if strings.ContainsRune(header.Linkname, '\x00') {
				return errors.New("NUL in symlink target")
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				return err
			}
		case tar.TypeLink:
			target, err := archivePath(header.Linkname)
			if err != nil {
				return err
			}
			links = append(links, hardlink{name, target})
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// A rootless image cache cannot create devices. Runtime supplies /dev.
			continue
		default:
			return fmt.Errorf("unsupported tar entry type %d: %s", header.Typeflag, name)
		}
	}
	// A tar EOF can precede the pipe's final error (including a failed layer
	// digest check). Drain the stream before accepting the merged filesystem.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return err
	}
	for len(links) != 0 {
		var pending []hardlink
		for _, link := range links {
			if err := safeParents(root, link.target, directories); err != nil {
				return err
			}
			info, err := root.Lstat(link.target)
			if errors.Is(err, os.ErrNotExist) {
				pending = append(pending, link)
				continue
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("hardlink target is not a regular file: %s", link.target)
			}
			if err := root.Link(link.target, link.name); err != nil {
				return err
			}
		}
		if len(pending) == len(links) {
			return errors.New("unresolved or cyclic archive hardlinks")
		}
		links = pending
	}
	return finishDirectories(root, directories)
}

func finishDirectories(root *os.Root, directories map[string]fs.FileMode) error {
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, name := range names {
		if err := root.Chmod(name, directories[name]); err != nil {
			return err
		}
	}
	return nil
}

func copyRootfs(ctx context.Context, source, dest string) error {
	src, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer dst.Close()
	directories := map[string]fs.FileMode{}
	links := map[imageFileIdentity]string{}
	err = fs.WalkDir(src.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := dst.MkdirAll(name, 0700); err != nil {
				return err
			}
			directories[name] = mode
		case mode&fs.ModeSymlink != 0:
			target, err := src.Readlink(name)
			if err != nil {
				return err
			}
			return dst.Symlink(target, name)
		case mode.IsRegular():
			identity, linked := imageFileID(info)
			if previous, ok := links[identity]; linked && ok {
				return dst.Link(previous, name)
			}
			in, err := src.Open(name)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := dst.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, contextReader{ctx, in})
			modeErr := out.Chmod(mode)
			if err := errors.Join(copyErr, modeErr, out.Close()); err != nil {
				return err
			}
			if linked {
				links[identity] = name
			}
		default:
			return fmt.Errorf("unsupported cached file: %s", name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return finishDirectories(dst, directories)
}
