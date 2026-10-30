// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A relative storage root would materialize the store relative to the
// caller's working directory, so New refuses it.
// TestDefaultRootPerPlatform pins the platform-standard default locations
// and the absolute-path requirement for SPOD_ROOT.
func TestDefaultRootPerPlatform(t *testing.T) {
	t.Setenv("SPOD_ROOT", "")
	base := t.TempDir()
	var want string
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LOCALAPPDATA", base)
		want = filepath.Join(base, "spod", "storage")
	case "darwin":
		t.Setenv("HOME", base)
		want = filepath.Join(base, "Library", "Application Support", "spod", "storage")
	default:
		t.Setenv("XDG_DATA_HOME", base)
		want = filepath.Join(base, "spod", "storage")
	}
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != want {
		t.Fatalf("default root: got %q, want %q", root, want)
	}

	t.Setenv("SPOD_ROOT", "relative-root")
	if _, err := DefaultRoot(); err == nil {
		t.Fatal("accepted a relative SPOD_ROOT")
	}
}

func TestNewRejectsRelativeRoot(t *testing.T) {
	if _, err := New("spod-root"); err == nil {
		t.Fatal("accepted a relative storage root")
	}
	if _, err := New("./spod-root"); err == nil {
		t.Fatal("accepted a relative storage root")
	}
	accepted := filepath.Join(t.TempDir(), "accepted")
	if _, err := New(accepted); err != nil {
		t.Fatalf("rejected an absolute root: %v", err)
	}
}

func TestDiscardRetainsDataAndRejectsEscapes(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Root, "images", "old")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "data"), []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Discard(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("resource still published: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "trash"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash contents: %v, %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "trash", entries[0].Name(), "data"))
	if err != nil || string(data) != "retained" {
		t.Fatalf("discard lost data: %q, %v", data, err)
	}
	outside := t.TempDir()
	for _, path := range []string{e.Root, filepath.Dir(e.Root), outside, filepath.Join(e.Root, "..", filepath.Base(outside))} {
		if err := e.Discard(path); err == nil {
			t.Errorf("discard accepted unsafe path %q", path)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("outside directory was moved")
	}
}
