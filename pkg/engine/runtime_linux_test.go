// SPDX-License-Identifier: Apache-2.0
//go:build linux

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMain hands control to libcontainer when the test binary is re-executed
// as a container's init process. In ordinary runs the guard is absent and
// this is a no-op, mirroring the engine.Init() call in cmd/spod's main.
func TestMain(m *testing.M) {
	Init()
	os.Exit(m.Run())
}

// A run that fails before the container process starts must leave no state
// under the container name, so the name stays immediately reusable.
func TestRunNeverStartedContainerIsDiscarded(t *testing.T) {
	ref, _ := publishTestImage(t, testImage(t, []imageEntry{{name: "usr/bin/tool", content: "x", mode: 0751}}))
	e := testEngine(t)
	opts := RunOptions{Name: "probe", Image: ref, Command: []string{"-l"}, TTY: true}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := e.Run(context.Background(), opts, IO{}); err == nil {
			t.Fatal("a TTY run without a console must fail")
		}
		if _, err := os.Stat(filepath.Join(e.Root, "containers", "probe")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed run left container state behind: %v", err)
		}
	}
}

func realLifecycleEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SPOD_REAL_LIFECYCLE_TEST") != "1" {
		t.Skip("set SPOD_REAL_LIFECYCLE_TEST=1 to run a real rootless lifecycle against docker.io/library/alpine:3.22")
	}
}

func waitRunning(t *testing.T, e *Engine, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		meta, err := e.Inspect(context.Background(), name)
		if err == nil && meta.Status == statusRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s never reached running: %+v, %v", name, meta, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRealLifecycleAlpine exercises the exported engine against a real
// registry image: pull, foreground exit status, output capture, exec, stop.
func TestRealLifecycleAlpine(t *testing.T) {
	realLifecycleEnabled(t)
	const ref = "docker.io/library/alpine:3.22"
	e := testEngine(t)
	ctx := context.Background()
	if _, err := e.Pull(ctx, ref); err != nil {
		t.Fatalf("pull: %v", err)
	}

	var out bytes.Buffer
	code, err := e.Run(ctx, RunOptions{
		Name:    "real-exit",
		Image:   ref,
		Command: []string{"/bin/sh", "-c", "echo lifecycle-ok; exit 42"},
		Remove:  true,
	}, IO{Stdout: &out, Stderr: &out})
	if err != nil || code != 42 {
		t.Fatalf("exit status: code=%d err=%v output=%q", code, err, out.String())
	}
	if !strings.Contains(out.String(), "lifecycle-ok") {
		t.Fatalf("missing output: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(e.Root, "containers", "real-exit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--rm left state behind: %v", err)
	}

	// A long-running container must accept exec and stop while its init
	// process runs in a separate goroutine, as the detached CLI does.
	done := make(chan int, 1)
	go func() {
		code, _ := e.Run(ctx, RunOptions{
			Name:    "real-long",
			Image:   ref,
			Command: []string{"/bin/sh", "-c", "sleep 60"},
		}, IO{})
		done <- code
	}()
	waitRunning(t, e, "real-long")

	var execOut bytes.Buffer
	code, err = e.Exec(ctx, "real-long", ExecOptions{
		Command: []string{"/bin/sh", "-c", "echo exec-ok; exit 7"},
	}, IO{Stdout: &execOut, Stderr: &execOut})
	if err != nil || code != 7 {
		t.Fatalf("exec: code=%d err=%v output=%q", code, err, execOut.String())
	}
	if !strings.Contains(execOut.String(), "exec-ok") {
		t.Fatalf("missing exec output: %q", execOut.String())
	}
	if err := e.Stop(ctx, "real-long", 5*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if code := <-done; code == 0 {
		t.Fatal("stopped container reported a clean exit")
	}
	meta, err := e.Inspect(ctx, "real-long")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != statusExited || meta.PID != 0 {
		t.Fatalf("post-stop metadata: %+v", meta)
	}

	// The store listing must see the stopped container with its image.
	list, err := e.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, c := range list {
		if c.Name == "real-long" {
			found = true
			if c.Image != ref {
				t.Fatalf("listed image %q, want %q", c.Image, ref)
			}
		}
	}
	if !found {
		t.Fatalf("stopped container missing from list: %+v", list)
	}

	// A stopped container restarts under a fresh process; Start blocks
	// until exit, so it runs in a goroutine the way the detached monitor
	// invokes it.
	restarted := make(chan int, 1)
	go func() {
		code, err := e.Start(ctx, "real-long", IO{})
		if err != nil {
			t.Errorf("start: %v", err)
		}
		restarted <- code
	}()
	waitRunning(t, e, "real-long")
	if err := e.Stop(ctx, "real-long", 5*time.Second); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if code := <-restarted; code == 0 {
		t.Fatal("restarted container reported a clean exit")
	}

	// A running container refuses removal without force; the restarted
	// container needs one more stop first.
	restarted2 := make(chan int, 1)
	go func() {
		code, _ := e.Start(ctx, "real-long", IO{})
		restarted2 <- code
	}()
	waitRunning(t, e, "real-long")
	if err := e.Remove(ctx, "real-long", false); err == nil {
		t.Fatal("remove of a running container without force must fail")
	}
	if err := e.Stop(ctx, "real-long", 5*time.Second); err != nil {
		t.Fatalf("third stop: %v", err)
	}
	<-restarted2

	// Remove discards the attach socket with the container, so the next
	// run of the same name never waits out a stale bind. The socket is
	// bound through a /proc/self/fd path like the engine does, since the
	// test store's path exceeds the unix socket name limit.
	socketPath := filepath.Join(e.Root, "run", "real-long.attach")
	runDir, err := os.OpenFile(filepath.Join(e.Root, "run"), unix.O_RDONLY|unix.O_PATH, 0)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: fmt.Sprintf("/proc/self/fd/%d/real-long.attach", runDir.Fd()),
		Net:  "unix",
	})
	if err != nil {
		runDir.Close()
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	runDir.Close()
	if err := e.Remove(ctx, "real-long", true); err != nil {
		t.Fatalf("force remove: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove left the attach socket behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "containers", "real-long")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove left container state behind: %v", err)
	}
}

func TestParseUser(t *testing.T) {
	cases := []struct {
		in          string
		uid, gid    int
		expectError bool
	}{
		{"", 0, 0, false},
		{"0", 0, 0, false},
		{"1000", 1000, 1000, false},
		{"1000:100", 1000, 100, false},
		{" 1000:100 ", 1000, 100, false},
		{"root", 0, 0, true},
		{"1000:users", 0, 0, true},
		{":100", 0, 0, true},
	}
	for _, tc := range cases {
		uid, gid, err := parseUser(tc.in)
		if tc.expectError {
			if err == nil {
				t.Errorf("parseUser(%q): expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUser(%q): %v", tc.in, err)
			continue
		}
		if uid != tc.uid || gid != tc.gid {
			t.Errorf("parseUser(%q) = %d:%d, want %d:%d", tc.in, uid, gid, tc.uid, tc.gid)
		}
	}
}
