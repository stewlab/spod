// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type TrashItem struct {
	Name         string    `json:"name"`
	OriginalName string    `json:"original_name"`
	ItemType     string    `json:"item_type"` // "container", "image", "socket", "log", "directory", "file"
	DiscardedAt  time.Time `json:"discarded_at"`
	Size         int64     `json:"size"`
	Path         string    `json:"path"`
}

// ListTrash returns all discarded items currently held in the storage trash.
func (e *Engine) ListTrash() ([]TrashItem, error) {
	trashDir := filepath.Join(e.Root, "trash")
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var items []TrashItem
	for _, entry := range entries {
		name := entry.Name()
		fullPath := filepath.Join(trashDir, name)

		item := TrashItem{
			Name: name,
			Path: fullPath,
		}

		// Discarded files are named: <unixnano>-<basename>
		if idx := strings.Index(name, "-"); idx > 0 {
			if nano, parseErr := strconv.ParseInt(name[:idx], 10, 64); parseErr == nil {
				item.DiscardedAt = time.Unix(0, nano)
				item.OriginalName = name[idx+1:]
			}
		}
		if item.OriginalName == "" {
			item.OriginalName = name
		}

		// Classify item type
		switch {
		case strings.HasSuffix(item.OriginalName, ".attach"):
			item.ItemType = "socket"
		case strings.HasSuffix(item.OriginalName, ".log"):
			item.ItemType = "log"
		case entry.IsDir():
			if _, mErr := os.Stat(filepath.Join(fullPath, "metadata.json")); mErr == nil {
				item.ItemType = "container"
			} else if _, rErr := os.Stat(filepath.Join(fullPath, "rootfs")); rErr == nil {
				item.ItemType = "image"
			} else {
				item.ItemType = "directory"
			}
		default:
			item.ItemType = "file"
		}

		if info, sErr := entry.Info(); sErr == nil {
			item.Size = info.Size()
		}
		items = append(items, item)
	}

	return items, nil
}

func safeRemoveAll(path string) error {
	_ = filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() {
			_ = os.Chmod(p, 0700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// CleanTrash purges items in the storage trash directory older than olderThan (or all if olderThan == 0).
// Returns the number of items removed and total bytes freed.
func (e *Engine) CleanTrash(olderThan time.Duration) (int, int64, error) {
	trashDir := filepath.Join(e.Root, "trash")
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	now := time.Now()
	var count int
	var freed int64

	for _, entry := range entries {
		name := entry.Name()
		fullPath := filepath.Join(trashDir, name)

		if olderThan > 0 {
			if idx := strings.Index(name, "-"); idx > 0 {
				if nano, parseErr := strconv.ParseInt(name[:idx], 10, 64); parseErr == nil {
					discarded := time.Unix(0, nano)
					if now.Sub(discarded) < olderThan {
						continue
					}
				}
			}
		}

		if info, sErr := entry.Info(); sErr == nil {
			freed += info.Size()
		}
		if err := safeRemoveAll(fullPath); err == nil {
			count++
		}
	}

	return count, freed, nil
}
