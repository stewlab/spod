// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/stewlab/spod/pkg/engine"
)

// humanBytes converts bytes to human-readable units.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// renderContainerTable formats a slice of containers as a clean aligned table.
func renderContainerTable(out io.Writer, containers []engine.Container) {
	if len(containers) == 0 {
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tIMAGE\tSTATUS\tCREATED\tCOMMAND")
	for _, c := range containers {
		cmdStr := strings.Join(c.Options.Command, " ")
		if cmdStr == "" && len(c.Entrypoint) > 0 {
			cmdStr = strings.Join(c.Entrypoint, " ")
		}
		if cmdStr == "" && len(c.Options.Entrypoint) > 0 {
			cmdStr = strings.Join(c.Options.Entrypoint, " ")
		}
		if len(cmdStr) > 42 {
			cmdStr = cmdStr[:39] + "..."
		}
		if cmdStr == "" {
			cmdStr = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			c.Name,
			shortImageRef(c.Image),
			c.Status,
			humanTime(c.Created),
			cmdStr,
		)
	}
	_ = w.Flush()
}

// renderImageTable formats a slice of images as a clean aligned table.
func renderImageTable(out io.Writer, images []engine.Image) {
	if len(images) == 0 {
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "REPOSITORY\tTAG\tIMAGE ID\tCREATED")
	for _, img := range images {
		ref := shortImageRef(img.Reference)
		repo := ref
		tag := "latest"
		if idx := strings.LastIndex(ref, ":"); idx != -1 {
			repo = ref[:idx]
			tag = ref[idx+1:]
		}

		imgID := img.Digest
		if strings.HasPrefix(imgID, "sha256:") {
			imgID = strings.TrimPrefix(imgID, "sha256:")
		}
		if len(imgID) > 12 {
			imgID = imgID[:12]
		}
		if imgID == "" {
			imgID = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			repo,
			tag,
			imgID,
			humanTime(img.Created),
		)
	}
	_ = w.Flush()
}

// renderTrashTable formats a slice of trash items as a clean aligned table.
func renderTrashTable(out io.Writer, items []engine.TrashItem) {
	if len(items) == 0 {
		fmt.Fprintln(out, "Storage trash is empty.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ITEM\tTYPE\tORIGINAL NAME\tDISCARDED\tSIZE")
	var totalSize int64
	for _, it := range items {
		totalSize += it.Size
		discardedStr := "-"
		if !it.DiscardedAt.IsZero() {
			discardedStr = humanTime(it.DiscardedAt)
		}
		name := it.Name
		if len(name) > 36 {
			name = name[:33] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			name,
			it.ItemType,
			it.OriginalName,
			discardedStr,
			humanBytes(it.Size),
		)
	}
	_ = w.Flush()
	fmt.Fprintf(out, "\nTotal: %d item(s), %s in trash.\n", len(items), humanBytes(totalSize))
}
