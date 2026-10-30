// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"io"
	"runtime"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
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
