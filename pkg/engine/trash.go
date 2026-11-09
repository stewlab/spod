// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
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

// dirSize calculates the recursive size in logical bytes of all files within a directory without following symlinks.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if fi, lerr := os.Lstat(p); lerr == nil {
				total += fi.Size()
			}
			return nil
		}
		if !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

// ListTrash returns all discarded items currently held in the storage trash.
// It never mutates trash contents.
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
			if nano, parseErr := strconv.ParseInt(name[:idx], 10, 64); parseErr == nil && nano > 0 {
				item.DiscardedAt = time.Unix(0, nano)
				item.OriginalName = name[idx+1:]
			}
		}
		if item.OriginalName == "" {
			item.OriginalName = name
		}

		// Classify item type and recover metadata
		switch {
		case strings.HasSuffix(item.OriginalName, ".attach"):
			item.ItemType = "socket"
		case strings.HasSuffix(item.OriginalName, ".log"):
			item.ItemType = "log"
		case entry.IsDir():
			if _, cErr := os.Stat(filepath.Join(fullPath, "container.json")); cErr == nil {
				item.ItemType = "container"
			} else if _, mErr := os.Stat(filepath.Join(fullPath, "metadata.json")); mErr == nil {
				item.ItemType = "container"
			} else if imgBytes, iErr := os.ReadFile(filepath.Join(fullPath, "image.json")); iErr == nil {
				item.ItemType = "image"
				var img Image
				if jErr := json.Unmarshal(imgBytes, &img); jErr == nil && img.Reference != "" {
					item.OriginalName = img.Reference
				}
			} else if _, rErr := os.Stat(filepath.Join(fullPath, "rootfs")); rErr == nil {
				item.ItemType = "image"
			} else if _, manErr := os.Stat(filepath.Join(fullPath, "manifest.json")); manErr == nil {
				item.ItemType = "image"
			} else {
				item.ItemType = "directory"
			}
		default:
			item.ItemType = "file"
		}

		if entry.IsDir() {
			item.Size, _ = dirSize(fullPath)
		} else if info, sErr := entry.Info(); sErr == nil {
			item.Size = info.Size()
		}
		items = append(items, item)
	}

	return items, nil
}

func safeRemoveAllCore(root string) error {
	fi, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Never traverse out of symlinks; remove the link itself.
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(root)
	}

	_ = os.Chmod(root, 0700)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		subPath := filepath.Join(root, entry.Name())
		if err := safeRemoveAllCore(subPath); err != nil {
			return err
		}
	}
	return os.Remove(root)
}

func safeRemoveAll(root string) error {
	err := safeRemoveAllCore(root)
	if err != nil && os.IsPermission(err) {
		if uErr := usernsRemove(root); uErr == nil {
			err = safeRemoveAllCore(root)
		}
	}
	return err
}

// CleanTrash purges items in the storage trash directory older than olderThan (or all if olderThan == 0).
// Returns the number of items removed and total bytes freed.
func (e *Engine) CleanTrash(olderThan time.Duration) (int, int64, error) {
	if olderThan < 0 {
		return 0, 0, fmt.Errorf("older-than duration cannot be negative: %v", olderThan)
	}

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
	var errs []error

	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." || strings.Contains(name, "/") || strings.Contains(name, "\\") {
			continue
		}
		fullPath := filepath.Join(trashDir, name)

		if olderThan > 0 {
			idx := strings.Index(name, "-")
			if idx <= 0 {
				// Malformed or unknown timestamp: fail closed for age-filtered purge (keep item)
				continue
			}
			nano, parseErr := strconv.ParseInt(name[:idx], 10, 64)
			if parseErr != nil || nano <= 0 {
				// Malformed timestamp: fail closed (keep item)
				continue
			}
			discarded := time.Unix(0, nano)
			if now.Sub(discarded) < olderThan {
				continue
			}
		}

		var itemSize int64
		if entry.IsDir() {
			itemSize, _ = dirSize(fullPath)
		} else if info, sErr := entry.Info(); sErr == nil {
			itemSize = info.Size()
		}

		if err := safeRemoveAll(fullPath); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", name, err))
		} else {
			count++
			freed += itemSize
		}
	}

	if len(errs) > 0 {
		return count, freed, errors.Join(errs...)
	}
	return count, freed, nil
}
