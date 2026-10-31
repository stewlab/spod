package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
)

func TestStyleHelpRendersStyledUsage(t *testing.T) {
	root := &cobra.Command{Use: "spod", Run: func(*cobra.Command, []string) {}}
	sub := &cobra.Command{Use: "run", Short: "Create and run a container"}
	root.AddCommand(sub)
	root.Flags().Bool("force", false, "force the operation")
	StyleHelp(root)

	var buf bytes.Buffer
	root.SetOut(&buf)
	if err := root.Usage(); err != nil {
		t.Fatal(err)
	}
	plain := buf.String()
	for _, want := range []string{"Usage:", "run", "Create and run a container", "--force"} {
		if !strings.Contains(plain, want) {
			t.Errorf("usage missing %q:\n%s", want, plain)
		}
	}

	// Subcommands inherit the template from the root.
	buf.Reset()
	sub.SetOut(&buf)
	if err := sub.Usage(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Usage:") {
		t.Errorf("subcommand did not inherit the styled template:\n%s", buf.String())
	}

	// Force a color profile and assert the styling actually lands.
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	buf.Reset()
	if err := root.Usage(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\x1b[1m") {
		t.Errorf("expected a bold escape in styled usage:\n%q", buf.String())
	}
}
