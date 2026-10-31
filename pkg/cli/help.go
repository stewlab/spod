// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// The shared help styling: podman-plain structure with command and flag
// names picked out. lipgloss strips the colors on pipes and honors
// NO_COLOR, so output degrades to plain text everywhere else. Embedders
// apply it with StyleHelp; ws calls the same function, so both CLIs
// render identically.

var (
	helpHead = lipgloss.NewStyle().Bold(true)
	helpCmd  = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D7D7")).Bold(true)
	helpFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D7D7"))
	helpDim  = lipgloss.NewStyle().Faint(true)
)

const styledUsageTemplate = `{{styleHead "Usage:"}}{{if .Runnable}}
  {{styleCmd .UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{styleCmd .CommandPath}} {{styleDim "[command]"}}{{end}}{{if gt (len .Aliases) 0}}

{{styleHead "Aliases:"}}
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

{{styleHead "Examples:"}}
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

{{styleHead "Available Commands:"}}{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{styleCmdName (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{styleHead $group.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{styleCmdName (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

{{styleHead "Additional Commands:"}}{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{styleCmdName (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{styleHead "Flags:"}}
{{styleFlags .LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

{{styleHead "Global Flags:"}}
{{styleFlags .InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

{{styleHead "Additional help topics:"}}{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

// Flag-usage lines start with the flag tokens; the description begins
// after the alignment gap FlagUsages lays down (two or more spaces).
// Continuation lines of wrapped descriptions do not start with a flag
// and are left untouched, as is the gap itself, so columns stay aligned.
var flagGapRe = regexp.MustCompile(`\s{2,}`)

func styleFlagUsages(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || !strings.HasPrefix(trimmed, "-") {
			continue
		}
		indent := line[:len(line)-len(trimmed)]
		if m := flagGapRe.FindStringIndex(trimmed); m != nil {
			lines[i] = indent + helpFlag.Render(trimmed[:m[0]]) + trimmed[m[0]:]
		} else {
			lines[i] = indent + helpFlag.Render(trimmed)
		}
	}
	return strings.Join(lines, "\n")
}

// StyleHelp applies the shared help styling to cmd; cobra inherits
// usage templates from the root, so every subcommand picks it up.
func StyleHelp(cmd *cobra.Command) {
	cobra.AddTemplateFunc("styleHead", helpHead.Render)
	cobra.AddTemplateFunc("styleCmd", helpCmd.Render)
	cobra.AddTemplateFunc("styleCmdName", helpCmd.Render)
	cobra.AddTemplateFunc("styleDim", helpDim.Render)
	cobra.AddTemplateFunc("styleFlags", styleFlagUsages)
	cmd.SetUsageTemplate(styledUsageTemplate)
}
