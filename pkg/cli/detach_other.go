// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package cli

import "os/exec"

func detachProcess(*exec.Cmd) {}
