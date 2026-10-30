// SPDX-License-Identifier: Apache-2.0
//go:build linux

package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err := e.Remove(ctx, "real-long", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
}
