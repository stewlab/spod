// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

package engine

func usernsRemove(path string) error {
	return inUserns(usernsRequest{Op: "trash-remove", Path: path}, nil)
}
