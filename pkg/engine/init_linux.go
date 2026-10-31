// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

package engine

import (
	"os"

	"github.com/opencontainers/runc/libcontainer"
	// Importing nsenter registers a cgo constructor that runs before the Go
	// runtime starts. It performs the clone(2)/unshare(2) and ID-map setup for
	// the container's namespaces from a single-threaded context, which Go code
	// cannot do safely after the scheduler spawns threads.
	_ "github.com/opencontainers/runc/libcontainer/nsenter"
)

// Init must be the very first call in main() for every binary that links this
// package. When the current process is a libcontainer re-exec (the init of a
// new container), it hands control to libcontainer and never returns.
// Otherwise it returns immediately and normal startup continues.
//
// Detection uses the _LIBCONTAINER_INITPIPE environment marker that
// libcontainer sets on its re-exec child, so ordinary subcommands (e.g. a
// user-facing "init" wizard) are never mistaken for container init.
func Init() {
	if os.Getenv(usernsHelperEnv) != "" {
		runUsernsHelper()
	}
	if os.Getenv("_LIBCONTAINER_INITPIPE") == "" {
		return
	}
	libcontainer.Init()
}
