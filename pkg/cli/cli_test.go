// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stewlab/spod/pkg/engine"
)

func TestRunRejectsUnsupportedFeatures(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--network=bridge", "alpine", "true"},
		{"run", "--device", "nvidia.com/gpu=all", "alpine", "true"},
		{"run", "--name", "../escape", "alpine", "true"},
		{"run", "--detach", "-it", "alpine", "true"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := NewCommand()
			cmd.SetArgs(append([]string{"--root", t.TempDir()}, args...))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err == nil {
				t.Fatal("unsupported configuration succeeded")
			}
		})
	}
}

func TestMountParsing(t *testing.T) {
	e, err := engine.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	mount, err := parseMount(e, host+":/workspace:ro,Z")
	if err != nil || !mount.ReadOnly || mount.Source != host || mount.Target != "/workspace" {
		t.Fatalf("read-only bind: %+v, %v", mount, err)
	}
	mount, err = parseMount(e, "home:/home/dev")
	if err != nil {
		t.Fatal(err)
	}
	if mount.Source != filepath.Join(e.Root, "volumes", "home", "_data") {
		t.Fatalf("wrong volume location: %+v", mount)
	}
	if _, err := os.Stat(mount.Source); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"../escape:/data", "data:relative", host + ":/data:unknown", ":/data"} {
		if _, err := parseMount(e, spec); err == nil {
			t.Errorf("accepted invalid mount %q", spec)
		}
	}
}

func TestCommandFlagsAreNotParsedAsRuntimeFlags(t *testing.T) {
	cmd := NewCommand()
	cmd.SetArgs([]string{"--root", t.TempDir(), "run", "--network=bridge", "alpine", "sh", "-c", "--name command-argument"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--network=host only") {
		t.Fatalf("command arguments were parsed as runtime flags: %v", err)
	}
}

func TestExitCodeSurvivesWrapping(t *testing.T) {
	if code := ExitCode(errors.Join(errors.New("command failed"), result(42, nil))); code != 42 {
		t.Fatalf("exit status lost: %d", code)
	}
}

func TestLogsTail(t *testing.T) {
	root := t.TempDir()
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Root, "logs", "test.log"), []byte("first\nsecond\nthird\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := NewCommand()
	cmd.SetArgs([]string{"--root", root, "logs", "--tail", "2", "test"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "second\nthird\n" {
		t.Fatalf("tail output %q", got)
	}
}

func TestPSEncodesEmptyList(t *testing.T) {
	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", t.TempDir(), "ps"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("empty ps output: %q", got)
	}
}
