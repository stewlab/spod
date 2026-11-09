// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestTrashReviewFindings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storage")
	e, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	trashDir := filepath.Join(root, "trash")
	if err := os.MkdirAll(trashDir, 0700); err != nil {
		t.Fatal(err)
	}

	// 1. Negative olderThan must return error on engine level
	if _, _, err := e.CleanTrash(-10 * time.Minute); err == nil {
		t.Fatal("expected error for negative duration in CleanTrash, got nil")
	}

	// 2. Real container with container.json and a known 4096-byte file
	contDir := filepath.Join(trashDir, "1700000000000000000-real-container")
	if err := os.MkdirAll(contDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contDir, "container.json"), []byte(`{"Name":"real-container"}`), 0600); err != nil {
		t.Fatal(err)
	}
	knownPayload := make([]byte, 4096)
	if err := os.WriteFile(filepath.Join(contDir, "data.bin"), knownPayload, 0600); err != nil {
		t.Fatal(err)
	}

	// 3. Real image with image.json
	imgDir := filepath.Join(trashDir, "1700000000000000001-abc123hash")
	if err := os.MkdirAll(imgDir, 0700); err != nil {
		t.Fatal(err)
	}
	imgJSON := `{"reference":"docker.io/library/alpine:3.22","digest":"sha256:abc123hash"}`
	if err := os.WriteFile(filepath.Join(imgDir, "image.json"), []byte(imgJSON), 0600); err != nil {
		t.Fatal(err)
	}

	// 4. Malformed timestamp entry
	malformedDir := filepath.Join(trashDir, "malformed-name")
	if err := os.MkdirAll(malformedDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformedDir, "note.txt"), []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}

	// 5. Read-only 0555 directory inside a discarded item
	roDir := filepath.Join(trashDir, "1700000000000000002-ro-item")
	if err := os.MkdirAll(filepath.Join(roDir, "subdir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "subdir", "file.txt"), []byte("ro content"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(roDir, "subdir"), 0555)
	_ = os.Chmod(roDir, 0555)

	// List trash and verify classification and recursive sizes
	items, err := e.ListTrash()
	if err != nil {
		t.Fatalf("ListTrash failed: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items in trash, got %d", len(items))
	}

	foundMap := make(map[string]TrashItem)
	for _, it := range items {
		foundMap[it.Name] = it
	}

	// Check container
	contItem, ok := foundMap["1700000000000000000-real-container"]
	if !ok {
		t.Fatal("missing container item in trash")
	}
	if contItem.ItemType != "container" {
		t.Errorf("expected container item type, got %q", contItem.ItemType)
	}
	if contItem.Size < 4096 {
		t.Errorf("expected recursive size >= 4096, got %d", contItem.Size)
	}

	// Check image
	imgItem, ok := foundMap["1700000000000000001-abc123hash"]
	if !ok {
		t.Fatal("missing image item in trash")
	}
	if imgItem.ItemType != "image" {
		t.Errorf("expected image item type, got %q", imgItem.ItemType)
	}
	if imgItem.OriginalName != "docker.io/library/alpine:3.22" {
		t.Errorf("expected recovered reference 'docker.io/library/alpine:3.22', got %q", imgItem.OriginalName)
	}

	// Clean with age filter (e.g. olderThan 1 hour)
	// Since 1700000000 is far in the past (Nov 2023), it's older than 1h.
	// But malformed-name has no valid timestamp and must NOT be purged!
	count, freed, err := e.CleanTrash(1 * time.Hour)
	if err != nil {
		t.Fatalf("CleanTrash(1h) failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 items purged, got %d", count)
	}
	if freed < 4096 {
		t.Errorf("expected freed bytes >= 4096, got %d", freed)
	}

	// Verify malformed item was preserved
	rem, err := e.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(rem) != 1 || rem[0].Name != "malformed-name" {
		t.Fatalf("expected only malformed-name to survive age filter, got: %+v", rem)
	}
}
