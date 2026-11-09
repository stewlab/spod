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

func TestCLIContractsAndFormatting(t *testing.T) {
	root := t.TempDir()

	// 1. Negative older-than must fail before mutations
	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", root, "trash", "clean", "--force", "--older-than", "-1h"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error on negative older-than duration, got nil")
	}

	// 2. Container prune on empty store
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "container", "prune"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No containers to prune.") {
		t.Fatalf("expected 'No containers to prune.', got %q", out.String())
	}

	// 3. Create dummy container and test ps --format
	cDir := filepath.Join(root, "containers", "test-format-cont")
	if err := os.MkdirAll(cDir, 0700); err != nil {
		t.Fatal(err)
	}
	contData := `{"Name":"test-format-cont","Image":"alpine:3.22","Status":"exited","Entrypoint":["/bin/sh"],"Options":{"Command":["-c","echo hi"]}}`
	if err := os.WriteFile(filepath.Join(cDir, "container.json"), []byte(contData), 0600); err != nil {
		t.Fatal(err)
	}

	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "ps", "--format", "{{.Name}}={{.Status}}"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "test-format-cont=exited" {
		t.Fatalf("expected 'test-format-cont=exited', got %q", out.String())
	}

	// 4. Test ps table output includes entrypoint + command
	cmd = NewCommand()
	out.Reset()
	cmd.SetArgs([]string{"--root", root, "ps"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/bin/sh -c echo hi") {
		t.Fatalf("expected combined entrypoint and command in table, got:\n%s", out.String())
	}
}
