// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/stewlab/spod/pkg/cli"
	"github.com/stewlab/spod/pkg/engine"
)

func main() {
	engine.Init()
	if err := cli.NewCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(cli.ExitCode(err))
	}
}
