// SPDX-License-Identifier: Apache-2.0
//go:build !linux || !cgo

package engine

import (
	"context"
	"errors"
	"io"
)

// Export requires the Linux container runtime, which records containers and
// their root filesystems.
func (e *Engine) Export(ctx context.Context, name string, out io.Writer) error {
	return errors.New("spod container export requires the Linux runtime")
}
