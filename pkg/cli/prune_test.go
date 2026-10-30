// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/stewlab/spod/pkg/engine"
)

func TestNormalizeImageRef(t *testing.T) {
	cases := map[string]string{
		"alpine":                                   "alpine:latest",
		"alpine:3.22":                              "alpine:3.22",
		"docker.io/library/alpine:3.22":            "alpine:3.22",
		"index.docker.io/library/alpine:3.22":      "alpine:3.22",
		"docker.io/archlinux/archlinux:base-devel": "archlinux/archlinux:base-devel",
		"Alpine:3.22":                              "alpine:3.22",
		"alpine@sha256:abc":                        "alpine@sha256:abc",
	}
	for in, want := range cases {
		if got := normalizeImageRef(in); got != want {
			t.Errorf("normalizeImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefsMatch(t *testing.T) {
	same := [][2]string{
		{"alpine:3.22", "docker.io/library/alpine:3.22"},
		{"alpine", "index.docker.io/library/alpine:latest"},
		{"archlinux/archlinux:base-devel", "docker.io/archlinux/archlinux:base-devel"},
	}
	for _, pair := range same {
		if !refsMatch(pair[0], pair[1]) {
			t.Errorf("refsMatch(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"alpine:3.21", "alpine:3.22"},
		{"alpine:3.22", "archlinux:base-devel"},
	}
	for _, pair := range different {
		if refsMatch(pair[0], pair[1]) {
			t.Errorf("refsMatch(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

func TestImageUsers(t *testing.T) {
	containers := []engine.Container{
		{Name: "ws-alpha", Image: "docker.io/library/alpine:3.22"},
		{Name: "ws-beta", Image: "archlinux/archlinux:base-devel"},
	}
	if got := imageUsers("alpine:3.22", containers); len(got) != 1 || got[0] != "ws-alpha" {
		t.Errorf("imageUsers(alpine) = %v, want [ws-alpha]", got)
	}
	if got := imageUsers("docker.io/archlinux/archlinux:base-devel", containers); len(got) != 1 || got[0] != "ws-beta" {
		t.Errorf("imageUsers(arch) = %v, want [ws-beta]", got)
	}
	if got := imageUsers("fedora:41", containers); len(got) != 0 {
		t.Errorf("imageUsers(fedora) = %v, want none", got)
	}
}

func TestHumanTime(t *testing.T) {
	now := time.Now()
	cases := map[string]time.Time{
		"just now": now.Add(-10 * time.Second),
		"5m ago":   now.Add(-5 * time.Minute),
		"3h ago":   now.Add(-3 * time.Hour),
		"2d ago":   now.Add(-48 * time.Hour),
	}
	for want, ts := range cases {
		if got := humanTime(ts); got != want {
			t.Errorf("humanTime(%v) = %q, want %q", ts, got, want)
		}
	}
}

func TestPromptYesNo(t *testing.T) {
	out := &bytes.Buffer{}
	confirmed, err := promptYesNo(strings.NewReader("y\n"), out, "go? ")
	if err != nil || !confirmed {
		t.Errorf("promptYesNo(y) = %v, %v; want true, nil", confirmed, err)
	}
	confirmed, err = promptYesNo(strings.NewReader("YES\n"), out, "go? ")
	if err != nil || !confirmed {
		t.Errorf("promptYesNo(YES) = %v, %v; want true, nil", confirmed, err)
	}
	// The default and every non-affirmative answer keep the current state.
	for _, answer := range []string{"\n", "n\n", "no\n", "later\n"} {
		confirmed, err = promptYesNo(strings.NewReader(answer), out, "go? ")
		if err != nil || confirmed {
			t.Errorf("promptYesNo(%q) = %v, %v; want false, nil", answer, confirmed, err)
		}
	}
}

// writeTestImage fabricates a complete store entry without contacting a
// registry, the way storeImage would publish one.
func writeTestImage(t *testing.T, root, ref string) {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(r.Name()))
	dir := filepath.Join(root, "images", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"Reference":%q,"Digest":"sha256:%s","Created":"2026-01-01T00:00:00Z"}`, r.Name(), strings.Repeat("a", 64))
	for file, data := range map[string]string{
		"image.json":    metadata,
		"manifest.json": "{}",
		"config.json":   "{}",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func imageDirName(t *testing.T, root, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(r.Name()))
	return filepath.Join(root, "images", hex.EncodeToString(sum[:]))
}

func TestPruneForceRemovesOnlyUnused(t *testing.T) {
	root := t.TempDir()
	if _, err := engine.New(root); err != nil {
		t.Fatal(err)
	}
	writeTestImage(t, root, "alpine:3.21")
	writeTestImage(t, root, "fedora:41")
	// A container holds alpine:3.21 under a different spelling of the same
	// reference; fedora:41 has no users.
	if err := os.MkdirAll(filepath.Join(root, "containers", "user"), 0700); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"user","Image":"docker.io/library/alpine:3.21","Status":"exited"}`
	if err := os.WriteFile(filepath.Join(root, "containers", "user", "container.json"), []byte(meta), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", root, "prune", "--force"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(imageDirName(t, root, "alpine:3.21")); err != nil {
		t.Errorf("in-use alpine:3.21 was pruned: %v", err)
	}
	if _, err := os.Stat(imageDirName(t, root, "fedora:41")); !os.IsNotExist(err) {
		t.Errorf("unused fedora:41 survived prune: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Removed fedora:41") || !strings.Contains(got, "storage trash") {
		t.Errorf("prune output %q", got)
	}
}

func TestPruneNonTerminalRefusesWithoutForce(t *testing.T) {
	root := t.TempDir()
	if _, err := engine.New(root); err != nil {
		t.Fatal(err)
	}
	writeTestImage(t, root, "alpine:3.21")

	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", root, "prune"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("prune without a terminal or --force: %v, want a refusal naming --force", err)
	}
	if _, err := os.Stat(imageDirName(t, root, "alpine:3.21")); err != nil {
		t.Errorf("image was pruned despite the refusal: %v", err)
	}
}

func TestPruneNothingToDo(t *testing.T) {
	root := t.TempDir()
	if _, err := engine.New(root); err != nil {
		t.Fatal(err)
	}
	writeTestImage(t, root, "alpine:3.21")
	if err := os.MkdirAll(filepath.Join(root, "containers", "user"), 0700); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"user","Image":"alpine:3.21","Status":"exited"}`
	if err := os.WriteFile(filepath.Join(root, "containers", "user", "container.json"), []byte(meta), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := NewCommand()
	out := &bytes.Buffer{}
	cmd.SetArgs([]string{"--root", root, "prune", "--force"})
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Nothing to prune") {
		t.Errorf("fully-used store output %q", got)
	}
	if _, err := os.Stat(imageDirName(t, root, "alpine:3.21")); err != nil {
		t.Errorf("used image was pruned: %v", err)
	}
}
