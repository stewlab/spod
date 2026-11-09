// SPDX-License-Identifier: Apache-2.0
//go:build !linux || !cgo

package engine

import "errors"

func usernsRemove(path string) error {
	return errors.New("userns helper not supported on this platform")
}
