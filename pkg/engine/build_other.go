// SPDX-License-Identifier: Apache-2.0
//go:build !linux || !cgo

package engine

import "context"

// Build is unsupported without the Linux container lifecycle: RUN executes
// in a container.
func (e *Engine) Build(context.Context, BuildOptions, IO) (*Image, error) {
	return nil, ErrUnsupported
}
