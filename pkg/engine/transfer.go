// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"runtime"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Import builds a stored image from a plain filesystem tar, such as one
// produced by Export, so it can serve as the base image of other containers.
// The archive becomes the image's single layer; no entrypoint, environment,
// or history is inferred beyond a default PATH. The archive is buffered
// while the layer digest is computed. Import works on every platform; only
// running the resulting containers requires Linux.
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

// ImportOCI stores an image tarball under ref. Both common save formats
// are accepted: the OCI layout (podman save --format oci-archive) and the
// docker-save layout (docker save). Unlike Import, the archive carries the
// image's real layers and configuration, so the stored image behaves
// exactly like the source: entrypoint, environment, and history included.
// The archive must contain a single-platform image matching the host.
func (e *Engine) ImportOCI(ctx context.Context, ref, path string) (*Image, error) {
	unlock, err := e.lockImages(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	r, err := imageReference(ref)
	if err != nil {
		return nil, err
	}
	img, cleanup, err := imageFromArchive(path, e.Root)
	if err != nil {
		return nil, fmt.Errorf("read image archive %s: %w", path, err)
	}
	// The layout path reads blobs lazily during storeImage, so the
	// extraction directory lives until the store has finished consuming it.
	defer cleanup()
	return e.storeImage(ctx, r, img)
}

// imageFromArchive loads a v1.Image from either an OCI-layout tarball or a
// docker-save tarball. The OCI layout needs the tarball unpacked first, so
// the archive is extracted to a temporary directory that stays alive until
// the returned cleanup runs — the image reads blobs lazily.
func imageFromArchive(path, tmpBase string) (v1.Image, func(), error) {
	if img, err := tarball.ImageFromPath(path, nil); err == nil {
		return img, func() {}, nil
	}
	// Extract beside the store: an unpacked image can be several gigabytes
	// and /tmp is frequently a small tmpfs.
	tmp, err := os.MkdirTemp(tmpBase, ".import-oci-*.tmp")
	if err != nil {
		return nil, nil, err
	}
	if err := untarTo(path, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, nil, err
	}
	// layout.Path has no manifest listing; the index is small JSON.
	raw, err := os.ReadFile(filepath.Join(tmp, "index.json"))
	if err != nil {
		os.RemoveAll(tmp)
		return nil, nil, fmt.Errorf("not a docker-save or OCI-layout tarball: %w", err)
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(raw, &index); err != nil {
		os.RemoveAll(tmp)
		return nil, nil, err
	}
	if len(index.Manifests) != 1 {
		os.RemoveAll(tmp)
		return nil, nil, fmt.Errorf("archive holds %d images; want exactly one", len(index.Manifests))
	}
	layoutIndex, err := layout.FromPath(tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, nil, err
	}
	img, err := layoutIndex.Image(index.Manifests[0].Digest)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, nil, err
	}
	return img, func() { os.RemoveAll(tmp) }, nil
}

// untarTo extracts a tar archive into dir, refusing paths that escape it.
func untarTo(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(header.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("archive entry escapes the extraction directory: %s", header.Name)
		}
		target := filepath.Join(dir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, header.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, header.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}
