// SPDX-License-Identifier: Apache-2.0
//go:build !linux || !cgo

// On non-Linux platforms, and on Linux built with CGO_ENABLED=0 (which cannot
// link libcontainer's namespace bootstrap), the container lifecycle is
// unsupported. These stubs let the package compile and cross-compile for all
// targets; every method reports ErrUnsupported.
package engine

import (
	"context"
	"time"
)

func (e *Engine) Run(context.Context, RunOptions, IO) (int, error) { return -1, ErrUnsupported }

func (e *Engine) Start(context.Context, string, IO) (int, error) { return -1, ErrUnsupported }

func (e *Engine) Exec(context.Context, string, ExecOptions, IO) (int, error) {
	return -1, ErrUnsupported
}

func (e *Engine) Stop(context.Context, string, time.Duration) error { return ErrUnsupported }

func (e *Engine) Remove(context.Context, string, bool) error { return ErrUnsupported }

func (e *Engine) Inspect(context.Context, string) (*Container, error) { return nil, ErrUnsupported }

func (e *Engine) List(context.Context) ([]Container, error) { return nil, ErrUnsupported }

func (e *Engine) Rename(context.Context, string, string) error { return ErrUnsupported }
