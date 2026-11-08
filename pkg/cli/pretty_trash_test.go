// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrettyPSAndContainerCommands(t *testing.T) {
	root := t.TempDir()

	// 1. ps empty
	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", root, "ps"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("expected empty output for empty ps, got %q", got)
	}

	// 2. container list empty
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "container", "list"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("expected empty output for empty container list, got %q", got)
	}

	// 3. container list --json
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "container", "list", "--json"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("expected '[]' for container list --json, got %q", out.String())
	}
}

func TestPrettyImagesAndImageCommands(t *testing.T) {
	root := t.TempDir()

	// 1. images empty
	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", root, "images"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("expected empty output for empty images, got %q", got)
	}

	// 2. image list empty
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "image", "list"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("expected empty output for empty image list, got %q", got)
	}

	// 3. image list --json
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "image", "list", "--json"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("expected '[]' for image list --json, got %q", out.String())
	}
}

func TestTrashCLI(t *testing.T) {
	root := t.TempDir()

	// 1. trash list when empty
	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", root, "trash", "list"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Storage trash is empty") {
		t.Fatalf("expected 'Storage trash is empty', got %q", out.String())
	}

	// 2. create dummy discarded item
	trashDir := filepath.Join(root, "trash")
	if err := os.MkdirAll(trashDir, 0700); err != nil {
		t.Fatal(err)
	}
	dummyItem := filepath.Join(trashDir, "1790000000000000000-dummy-cont")
	if err := os.MkdirAll(dummyItem, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dummyItem, "metadata.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	// 3. trash list should now display item
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "trash", "list"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dummy-cont") || !strings.Contains(out.String(), "container") {
		t.Fatalf("expected dummy-cont in trash list, got:\n%s", out.String())
	}

	// 4. trash clean --force
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "trash", "clean", "--force"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Purged 1 item(s)") {
		t.Fatalf("expected purged message, got:\n%s", out.String())
	}

	// 5. trash list is now empty again
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "trash", "list"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Storage trash is empty") {
		t.Fatalf("expected empty trash, got:\n%s", out.String())
	}
}
