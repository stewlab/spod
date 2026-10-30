// SPDX-License-Identifier: Apache-2.0
//go:build !linux || !cgo

package engine

// Init is a no-op on platforms and builds that cannot execute containers
// (non-Linux, or Linux built with CGO_ENABLED=0, which cannot link the
// libcontainer nsenter bootstrap). Calling it first in main() remains safe.
func Init() {}
