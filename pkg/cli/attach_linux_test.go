// SPDX-License-Identifier: Apache-2.0
//go:build linux

package cli

import (
	"bytes"
	"io"
	"net"
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
