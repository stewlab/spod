// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// runInitPayload is the run-init helper request: the container to create and
// run inside the storage user namespace.
type runInitPayload struct {
	Root    string       `json:"root"`
	Name    string       `json:"name"`
	Rootfs  string       `json:"rootfs"`
	Runtime *runtimeMeta `json:"runtime"`
}

// runInitOp creates, runs and destroys a keep-id container as root of the
// storage user namespace, reporting "pid N", then "exit N" or "error MSG".
func runInitOp(req usernsRequest, _ io.Writer) error {
	var p runInitPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return err
	}
	// The caller handles interruption by killing the container; the helper
	// stays to destroy the libcontainer state.
	signal.Ignore(unix.SIGINT, unix.SIGTERM, unix.SIGHUP)
	e := &Engine{Root: p.Root}
	code, err := e.runInit(context.Background(), e.stateDir(p.Name), p.Name, p.Rootfs, p.Runtime,
		IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, true,
		func(pid int) { fmt.Fprintf(usernsReport, "pid %d\n", pid) })
	if err != nil {
		fmt.Fprintf(usernsReport, "error %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return nil
	}
	fmt.Fprintf(usernsReport, "exit %d\n", code)
	return nil
}

// launchInitUserns runs runInit in a storage user namespace helper. The
// caller's stdio is the helper's, so terminals pass through unchanged.
func (e *Engine) launchInitUserns(ctx context.Context, name, rootfs string, rm *runtimeMeta, stdio IO, started func(pid int)) (int, error) {
	payload, err := json.Marshal(runInitPayload{Root: e.Root, Name: name, Rootfs: rootfs, Runtime: rm})
	if err != nil {
		return -1, err
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	defer reportR.Close()
	cmd, err := startUserns(usernsRequest{Op: "run-init", Payload: payload}, stdio, reportW)
	reportW.Close()
	if err != nil {
		return -1, err
	}

	pid, code := 0, -1
	var runErr error
	ctxDone := make(chan struct{})
	defer close(ctxDone)
	sc := bufio.NewScanner(reportR)
	for sc.Scan() {
		kind, val, _ := strings.Cut(sc.Text(), " ")
		switch kind {
		case "pid":
			if pid, err = strconv.Atoi(val); err != nil {
				continue
			}
			started(pid)
			go func(pid int) {
				select {
				case <-ctx.Done():
					// The caller owns the helper namespace, so it can
					// signal the container nested in it.
					_ = unix.Kill(pid, unix.SIGKILL)
				case <-ctxDone:
				}
			}(pid)
		case "exit":
			code, _ = strconv.Atoi(val)
		case "error":
			runErr = errors.New(val)
		}
	}
	waitErr := cmd.Wait()
	if runErr != nil {
		return -1, runErr
	}
	if pid == 0 {
		if waitErr == nil {
			waitErr = errors.New("no container started")
		}
		return -1, fmt.Errorf("--userns=keep-id helper: %w", waitErr)
	}
	return code, nil
}
