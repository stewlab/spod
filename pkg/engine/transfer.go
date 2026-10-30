// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Export writes a container's root filesystem as a plain uncompressed tar
// stream. The archive holds the filesystem as the container sees it: entries
// are owned by root, with modes and symlinks preserved. Runtime bind mounts
// and device nodes are host resources and are not part of the archive. The
// container keeps running while it is exported; files written concurrently
// may be captured mid-write.
func (e *Engine) Export(ctx context.Context, name string, out io.Writer) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock, err := e.lockContainer(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := e.loadContainer(name); err != nil {
		return err
	}
	rootfs := e.rootfsDir(name)
	tw := tar.NewWriter(out)
	defer tw.Close()
	return filepath.WalkDir(rootfs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(rootfs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    rel,
			Mode:    int64(info.Mode().Perm()),
			ModTime: info.ModTime(),
			Uid:     0, Gid: 0,
			Uname:  "root",
			Gname:  "root",
			Format: tar.FormatPAX,
		}
		switch {
		case info.IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
		case info.Mode()&fs.ModeSymlink != 0:
			hdr.Typeflag = tar.TypeSymlink
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Linkname = target
		case info.Mode().IsRegular():
			hdr.Typeflag = tar.TypeReg
			hdr.Size = info.Size()
		default:
			// Sockets, fifos, and device nodes have no archive
			// representation for a rootless container.
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		return errors.Join(copyErr, f.Close())
	})
}

// Import builds a stored image from a plain filesystem tar, such as one
// produced by Export, so it can serve as the base image of other containers.
// The archive becomes the image's single layer; no entrypoint, environment,
// or history is inferred beyond a default PATH. The archive is buffered
// while the layer digest is computed.
func (e *Engine) Import(ctx context.Context, ref string, archive io.Reader) (*Image, error) {
	unlock, err := e.lockImages(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	r, err := imageReference(ref)
	if err != nil {
		return nil, err
	}
	layer, err := tarball.LayerFromReader(archive)
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	cfg := &v1.ConfigFile{}
	cfg.OS, cfg.Architecture = "linux", runtime.GOARCH
	cfg.Config = v1.Config{
		Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, err
	}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return nil, err
	}
	return e.storeImage(ctx, r, img)
}
