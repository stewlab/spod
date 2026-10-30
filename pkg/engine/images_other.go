// SPDX-License-Identifier: Apache-2.0
//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package engine

import (
	"context"
	"io/fs"
)

func imageFileID(fs.FileInfo) (imageFileIdentity, bool) {
	return imageFileIdentity{}, false
}

// Other builds support metadata tooling and process-local serialization.
// Cross-process store locking is available on Linux and BSD-family hosts.
func lockImageProcesses(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}
