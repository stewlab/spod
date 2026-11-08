// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrashListAndClean(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storage")
	e, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Initial list should be empty
	items, err := e.ListTrash()
	if err != nil {
		t.Fatalf("ListTrash failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items in trash, got %d", len(items))
	}

	// 2. Discard dummy files
	dummyDir := filepath.Join(root, "containers", "test-cont")
	if err := os.MkdirAll(dummyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dummyDir, "metadata.json"), []byte(`{"Name":"test-cont"}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := e.Discard(dummyDir); err != nil {
		t.Fatalf("Discard failed: %v", err)
	}

	// 3. List trash should now have 1 item
	items, err = e.ListTrash()
	if err != nil {
		t.Fatalf("ListTrash failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item in trash, got %d", len(items))
	}
	if items[0].OriginalName != "test-cont" {
		t.Errorf("expected original name 'test-cont', got %q", items[0].OriginalName)
	}
	if items[0].ItemType != "container" {
		t.Errorf("expected item type 'container', got %q", items[0].ItemType)
	}
	if items[0].Size <= 0 {
		t.Errorf("expected positive size, got %d", items[0].Size)
	}

	// 4. Clean trash
	count, freed, err := e.CleanTrash(0)
	if err != nil {
		t.Fatalf("CleanTrash failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 cleaned item, got %d", count)
	}
	if freed <= 0 {
		t.Errorf("expected positive bytes freed, got %d", freed)
	}

	// 5. Verify trash is now empty
	items, err = e.ListTrash()
	if err != nil {
		t.Fatalf("ListTrash after clean failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items in trash after clean, got %d", len(items))
	}
}
