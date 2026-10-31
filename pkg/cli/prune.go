// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stewlab/spod/pkg/engine"
	"golang.org/x/term"
)

func addPrune(app *cobra.Command, withStore storeHandler) {
	var force bool
	prune := &cobra.Command{
		Use: "prune", Args: cobra.NoArgs,
		Short: "Remove stored images no container uses (retained in storage trash)",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, _ []string) error {
			images, err := e.Images()
			if err != nil {
				return err
			}
			// Removing an image a container still needs would break that
			// container, so usage must be known before anything is offered.
			users, err := e.List(cmd.Context())
			if err != nil {
				return fmt.Errorf("cannot determine which images containers use: %w", err)
			}
			var unused []engine.Image
			for _, img := range images {
				if len(imageUsers(img.Reference, users)) == 0 {
					unused = append(unused, img)
				}
			}
			out := cmd.OutOrStdout()
			if len(unused) == 0 {
				fmt.Fprintln(out, "Nothing to prune: every stored image is used by a container.")
				return nil
			}
			for _, img := range unused {
				fmt.Fprintf(out, "  %s (%s)\n", shortImageRef(img.Reference), humanTime(img.Created))
			}
			if !force {
				if !stdinIsTerminal() {
					return fmt.Errorf("%d unused image(s) would be removed; re-run with --force to remove them (they are kept in the storage trash)", len(unused))
				}
				confirmed, err := promptYesNo(cmd.InOrStdin(), out, fmt.Sprintf("Remove the %d unused image(s) above? They are kept in the storage trash. [y/N] ", len(unused)))
				if err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(out, "Nothing removed.")
					return nil
				}
			}
			for _, img := range unused {
				if err := e.RemoveImage(img.Reference); err != nil {
					return err
				}
				fmt.Fprintf(out, "Removed %s (retained in the storage trash)\n", shortImageRef(img.Reference))
			}
			return nil
		}),
	}
	prune.Flags().BoolVar(&force, "force", false, "Remove without asking (for scripts)")
	app.AddCommand(prune)
}

// imageUsers names the containers whose image matches ref. Stores record
// references with varying spellings (registry-qualified, library/
// namespace, missing tag), so matching tolerates those forms.
func imageUsers(ref string, containers []engine.Container) []string {
	var names []string
	for _, c := range containers {
		if refsMatch(ref, c.Image) {
			names = append(names, c.Name)
		}
	}
	return names
}

// refsMatch compares two image references after canonicalization. Tag
// differences are real differences: alpine:3.21 and alpine:3.22 are
// distinct images, and only spelling variants of the same reference
// (short name vs registry-qualified) may fold into each other.
func refsMatch(a, b string) bool {
	na, nb := normalizeImageRef(a), normalizeImageRef(b)
	return na == nb || strings.Contains(na, nb) || strings.Contains(nb, na)
}

// normalizeImageRef folds the spellings a store may record: the default
// registries and docker's library/ namespace drop off, and a missing tag
// reads as latest.
func normalizeImageRef(ref string) string {
	r := strings.ToLower(strings.TrimSpace(ref))
	for _, prefix := range []string{"index.docker.io/", "docker.io/"} {
		r = strings.TrimPrefix(r, prefix)
	}
	r = strings.TrimPrefix(r, "library/")
	if r != "" && !strings.Contains(r, ":") && !strings.Contains(r, "@") {
		r += ":latest"
	}
	return r
}

// shortImageRef drops default-registry noise for display.
func shortImageRef(ref string) string {
	for _, prefix := range []string{"index.docker.io/", "docker.io/"} {
		ref = strings.TrimPrefix(ref, prefix)
	}
	return strings.TrimPrefix(ref, "library/")
}

// humanTime renders a timestamp relative to now in coarse units.
func humanTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2, 2006")
	}
}

// stdinIsTerminal reports whether stdin is an interactive terminal, gating
// prompts: scripts and pipes must never block on a question.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// promptYesNo asks a yes/no question and defaults to No: an empty answer,
// "no", or anything unrecognized keeps the current state.
func promptYesNo(in io.Reader, out io.Writer, question string) (bool, error) {
	fmt.Fprint(out, question)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && answer == "" {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
