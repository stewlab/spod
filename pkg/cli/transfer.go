// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/stewlab/spod/pkg/engine"
)

// addTransfer registers export and import, which move container filesystems
// through plain tar archives: export captures a container's filesystem, and
// import turns an archive into a stored image that other containers can use
// as their base.
func addTransfer(app *cobra.Command, withStore storeHandler) {
	var output string
	export := &cobra.Command{
		Use: "export NAME", Args: cobra.ExactArgs(1),
		Short: "Write a container's filesystem as a tar archive",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			out := io.Writer(cmd.OutOrStdout())
			if output != "" {
				f, err := os.Create(output)
				if err != nil {
					return err
				}
				defer f.Close()
				out = f
			}
			return e.Export(cmd.Context(), args[0], out)
		}),
	}
	export.Flags().StringVarP(&output, "output", "o", "", "Write to FILE instead of stdout")

	importArchive := &cobra.Command{
		Use: "import REFERENCE [FILE]", Args: cobra.RangeArgs(1, 2),
		Short: "Create an image from a tar archive, to use as a base image",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			var in io.Reader = cmd.InOrStdin()
			if len(args) == 2 && args[1] != "-" {
				f, err := os.Open(args[1])
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			}
			img, err := e.Import(cmd.Context(), args[0], in)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), img.Reference)
			return nil
		}),
	}
	app.AddCommand(export)
	app.AddCommand(importArchive)
}
