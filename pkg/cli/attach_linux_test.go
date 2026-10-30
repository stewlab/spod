// SPDX-License-Identifier: Apache-2.0
//go:build linux

package cli

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stewlab/spod/pkg/engine"
)

func TestMonitorAttachStreamsWithLongStoragePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("long-storage-", 8))
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewCommand()
	var log bytes.Buffer
	cmd.SetOut(&log)
	cmd.SetErr(&log)
	stdio, closeIO, err := monitorStreams(cmd, e, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIO()
	address, dir, err := attachAddress(e, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	conn, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("input\n"))
		written <- err
	}()
	input := make([]byte, len("input\n"))
	if _, err := io.ReadFull(stdio.Stdin, input); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if string(input) != "input\n" {
		t.Fatalf("stdin: %q", input)
	}
	if _, err := stdio.Stdout.Write([]byte("output\n")); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, len("output\n"))
	if _, err := io.ReadFull(conn, output); err != nil {
		t.Fatal(err)
	}
	if string(output) != "output\n" || log.String() != "output\n" {
		t.Fatalf("output %q, log %q", output, log.String())
	}
}

// A stale attach socket (file present, no listener) must be discarded and
// rebound, not treated as a live monitor.
func TestMonitorAttachStreamsDiscardsStaleSocket(t *testing.T) {
	e, err := engine.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(e.Root, "run", "stale.attach")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	dead.SetUnlinkOnClose(false)
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := NewCommand()
	_, closeIO, err := monitorStreams(cmd, e, "stale", true)
	if err != nil {
		t.Fatalf("stale socket must be discarded and rebound: %v", err)
	}
	defer closeIO()
}

// While a previous monitor still holds the socket — the recreate race —
// binding waits for the listener to release instead of failing outright.
func TestMonitorAttachStreamsWaitsForDyingMonitor(t *testing.T) {
	e, err := engine.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(e.Root, "run", "dying.attach")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	previous, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := previous.Close(); err != nil {
			t.Errorf("closing the previous monitor: %v", err)
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, closeIO, err := monitorStreams(NewCommand(), e, "dying", false)
		if err == nil {
			closeIO()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bind must wait out the dying monitor: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bind never succeeded after the previous monitor released")
	}
}
