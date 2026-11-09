// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/stewlab/spod/pkg/engine"
)

func addTrash(app *cobra.Command, withStore storeHandler) {
	var trashJSON bool
	trashCmd := &cobra.Command{
		Use:     "trash",
		Aliases: []string{"storage-trash"},
		Short:   "Manage discarded containers, images, and logs held in storage trash",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, _ []string) error {
			items, err := e.ListTrash()
			if err != nil {
				return err
			}
			if trashJSON {
				if items == nil {
					items = []engine.TrashItem{}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
			}
			renderTrashTable(cmd.OutOrStdout(), items)
			return nil
		}),
	}
	trashCmd.Flags().BoolVar(&trashJSON, "json", false, "Output trash items as JSON")

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List discarded resources in storage trash",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, _ []string) error {
			items, err := e.ListTrash()
			if err != nil {
				return err
			}
			if trashJSON {
				if items == nil {
					items = []engine.TrashItem{}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
			}
			renderTrashTable(cmd.OutOrStdout(), items)
			return nil
		}),
	}
	listCmd.Flags().BoolVar(&trashJSON, "json", false, "Output trash items as JSON")

	var forceClean bool
	var olderThan time.Duration
	cleanCmd := &cobra.Command{
		Use:     "clean",
		Aliases: []string{"prune", "empty", "purge"},
		Short:   "Permanently purge discarded items from storage trash",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, _ []string) error {
			if olderThan < 0 {
				return fmt.Errorf("--older-than duration cannot be negative: %v", olderThan)
			}
			items, err := e.ListTrash()
			if err != nil {
				return err
			}
			now := time.Now()
			var eligible []engine.TrashItem
			var eligibleSize int64
			for _, it := range items {
				if olderThan > 0 {
					if it.DiscardedAt.IsZero() {
						// Malformed or unknown timestamp: keep item
						continue
					}
					if now.Sub(it.DiscardedAt) < olderThan {
						continue
					}
				}
				eligible = append(eligible, it)
				eligibleSize += it.Size
			}
			if len(eligible) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No eligible items in storage trash to purge.")
				return nil
			}
			out := cmd.OutOrStdout()
			if !forceClean {
				if !stdinIsTerminal() {
					return fmt.Errorf("%d eligible item(s) in storage trash; re-run with --force to purge them permanently", len(eligible))
				}
				confirmed, err := promptYesNo(cmd.InOrStdin(), out, fmt.Sprintf("Permanently purge %d eligible item(s) (%s) from global storage trash (%s)? This cannot be undone. [y/N] ", len(eligible), humanBytes(eligibleSize), filepath.Join(e.Root, "trash")))
				if err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(out, "Nothing purged.")
					return nil
				}
			}
			count, freed, err := e.CleanTrash(olderThan)
			if err != nil {
				return fmt.Errorf("purging trash: %w", err)
			}
			fmt.Fprintf(out, "Purged %d item(s) from storage trash, freeing %s.\n", count, humanBytes(freed))
			return nil
		}),
	}
	cleanCmd.Flags().BoolVarP(&forceClean, "force", "f", false, "Purge trash without asking (for scripts)")
	cleanCmd.Flags().DurationVar(&olderThan, "older-than", 0, "Only purge items older than duration (e.g. 72h, 168h)")

	trashCmd.AddCommand(listCmd, cleanCmd)
	app.AddCommand(trashCmd)
}
