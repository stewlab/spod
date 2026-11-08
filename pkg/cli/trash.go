// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
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
			items, err := e.ListTrash()
			if err != nil {
				return err
			}
			if len(items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Storage trash is already empty.")
				return nil
			}
			out := cmd.OutOrStdout()
			if !forceClean {
				if !stdinIsTerminal() {
					return fmt.Errorf("%d item(s) in storage trash; re-run with --force to purge them permanently", len(items))
				}
				confirmed, err := promptYesNo(cmd.InOrStdin(), out, fmt.Sprintf("Permanently purge %d item(s) from storage trash? This cannot be undone. [y/N] ", len(items)))
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
				return err
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
