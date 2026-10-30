// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package cli

import (
	"github.com/spf13/cobra"
	"github.com/stewlab/spod/pkg/engine"
)

func monitorStreams(cmd *cobra.Command, _ *engine.Engine, _ string, _ bool) (engine.IO, func(), error) {
	return engine.IO{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, func() {}, nil
}

func addAttach(app *cobra.Command, _ storeHandler) {
	app.AddCommand(&cobra.Command{
		Use: "attach NAME", Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return engine.ErrUnsupported },
	})
}
