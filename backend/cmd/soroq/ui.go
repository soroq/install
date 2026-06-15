package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type commandHelp struct {
	Name        string
	Summary     string
	Group       string
	Usage       string
	Example     string
	Aliases     []string
	SearchTerms []string
}

type usageSection struct {
	Title string
	Rows  []usageRow
}

type usageRow struct {
	Name        string
	Description string
}

var rootCommands = []commandHelp{
	{
		Name:    "init",
		Summary: "Create a project-level soroq.yaml in a Flutter app.",
		Group:   "Project",
		Usage:   "soroq init [--project-dir .] [--app-id APP_ID] [--channel stable]",
		Example: "soroq init --app-id com.example.app",
	},
	{
		Name:    "status",
		Summary: "Inspect whether the current Flutter app is Soroq-ready.",
		Group:   "Project",
		Usage:   "soroq status [--project-dir .] [--json]",
		Example: "soroq status",
	},
	{
		Name:    "app",
		Summary: "Register or manage Soroq apps in the control plane.",
		Group:   "Control plane",
		Usage:   "soroq app <create|list|status> [flags]",
		Example: "soroq app create --bundle-id com.example.app",
	},
	{
		Name:    "login",
		Summary: "Store hosted control-plane operator credentials.",
		Group:   "Control plane",
		Usage:   "soroq login [--token TOKEN] [--api URL]",
		Example: "soroq login --token $SOROQ_CONTROL_PLANE_OPERATOR_TOKEN",
	},
	{
		Name:    "logout",
		Summary: "Remove stored hosted control-plane credentials.",
		Group:   "Control plane",
		Usage:   "soroq logout",
		Example: "soroq logout",
	},
	{
		Name:    "whoami",
		Summary: "Verify the current hosted control-plane operator.",
		Group:   "Control plane",
		Usage:   "soroq whoami [--api URL]",
		Example: "soroq whoami",
	},
	{
		Name:    "release",
		Summary: "Register a built Android release artifact.",
		Group:   "Release",
		Usage:   "soroq release <android|list|status> [flags]",
		Example: "soroq release android --artifact build/app/outputs/bundle/release/app-release.aab",
	},
	{
		Name:    "patch",
		Summary: "Publish hosted Android, asset, or config patches.",
		Group:   "Release",
		Usage:   "soroq patch <android|config|health|list|status> [flags]",
		Example: "soroq patch android --release-id rel_123 --artifact patch.zip",
	},
	{
		Name:    "inspect",
		Summary: "Inspect bundled Soroq metadata in local artifacts.",
		Group:   "Verification",
		Usage:   "soroq inspect android --artifact PATH",
		Example: "soroq inspect android --artifact app-release.aab",
	},
	{
		Name:    "rollback",
		Summary: "Roll back a hosted patch by patch id.",
		Group:   "Recovery",
		Usage:   "soroq rollback --patch-id PATCH_ID [--reason TEXT]",
		Example: "soroq rollback --patch-id patch_123 --reason \"bad rollout\"",
	},
}

type terminalUI struct {
	out   io.Writer
	color bool
}

const (
	styleReset = "\033[0m"
	styleBold  = "\033[1m"
	styleDim   = "\033[2m"
	styleRed   = "\033[31m"
	styleGreen = "\033[32m"
	styleBlue  = "\033[34m"
	styleAmber = "\033[33m"
)

func newTerminalUI(out io.Writer) terminalUI {
	return terminalUI{
		out:   out,
		color: shouldUseColor(out),
	}
}

func shouldUseColor(out io.Writer) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOROQ_COLOR"))) {
	case "always", "1", "true", "yes", "on":
		return true
	case "never", "0", "false", "no", "off":
		return false
	}
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (ui terminalUI) paint(style string, value string) string {
	if !ui.color || value == "" {
		return value
	}
	return style + value + styleReset
}

func (ui terminalUI) bold(value string) string {
	return ui.paint(styleBold, value)
}

func (ui terminalUI) dim(value string) string {
	return ui.paint(styleDim, value)
}

func (ui terminalUI) red(value string) string {
	return ui.paint(styleRed, value)
}

func (ui terminalUI) green(value string) string {
	return ui.paint(styleGreen, value)
}

func (ui terminalUI) blue(value string) string {
	return ui.paint(styleBlue, value)
}

func (ui terminalUI) amber(value string) string {
	return ui.paint(styleAmber, value)
}

func printRootUsage(out io.Writer) {
	ui := newTerminalUI(out)
	groups := groupedRootCommands()

	fmt.Fprintln(out, ui.bold("Soroq CLI"))
	fmt.Fprintln(out, ui.dim("Professional OTA release tooling for Flutter apps."))
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.blue("Usage"))
	fmt.Fprintln(out, "  soroq <command> [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.blue("Workflow"))
	fmt.Fprintln(out, "  1. soroq login")
	fmt.Fprintln(out, "  2. soroq init")
	fmt.Fprintln(out, "  3. soroq release android --artifact <app.aab|app.apk>")
	fmt.Fprintln(out, "  4. soroq patch android --release-id <id> --artifact <patch.zip>")
	fmt.Fprintln(out, "  5. soroq inspect android --artifact <app.aab|app.apk>")
	fmt.Fprintln(out)

	order := []string{"Project", "Control plane", "Release", "Verification", "Recovery"}
	for _, group := range order {
		commands := groups[group]
		if len(commands) == 0 {
			continue
		}
		fmt.Fprintln(out, ui.blue(group))
		for _, command := range commands {
			fmt.Fprintf(out, "  %-9s %s\n", ui.green(command.Name), command.Summary)
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, ui.blue("Examples"))
	fmt.Fprintln(out, "  soroq status")
	fmt.Fprintln(out, "  soroq app create --bundle-id com.example.app")
	fmt.Fprintln(out, "  soroq release android --artifact build/app/outputs/bundle/release/app-release.aab")
	fmt.Fprintln(out, "  soroq patch android --release-id rel_123 --artifact patch.zip")
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.blue("Environment"))
	fmt.Fprintln(out, "  SOROQ_API                          Hosted control-plane URL override.")
	fmt.Fprintln(out, "  SOROQ_CONTROL_PLANE_OPERATOR_TOKEN Operator token for automation.")
	fmt.Fprintln(out, "  SOROQ_COLOR=auto|always|never      Terminal color policy.")
	fmt.Fprintln(out, "  NO_COLOR=1                         Disable ANSI color output.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.dim("Run `soroq <command> --help` for command-specific flags."))
}

func printCommandUsage(out io.Writer, title string, summary string, usage string, sections []usageSection, examples []string) {
	ui := newTerminalUI(out)

	fmt.Fprintln(out, ui.bold(title))
	if strings.TrimSpace(summary) != "" {
		fmt.Fprintln(out, ui.dim(summary))
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, ui.blue("Usage"))
	fmt.Fprintf(out, "  %s\n", usage)
	for _, section := range sections {
		if len(section.Rows) == 0 {
			continue
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, ui.blue(section.Title))
		for _, row := range section.Rows {
			fmt.Fprintf(out, "  %-10s %s\n", ui.green(row.Name), row.Description)
		}
	}
	if len(examples) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, ui.blue("Examples"))
		for _, example := range examples {
			fmt.Fprintf(out, "  %s\n", example)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.dim("Use `--help` on a subcommand for exact flags."))
}

func printUnknownSubcommand(out io.Writer, parent string, subcommand string, allowed []string) {
	ui := newTerminalUI(out)
	trimmed := strings.TrimSpace(subcommand)
	if trimmed == "" {
		fmt.Fprintf(out, "%s missing subcommand for `%s`.\n", ui.red("[ERROR]"), parent)
	} else {
		fmt.Fprintf(out, "%s unknown subcommand `%s %s`.\n", ui.red("[ERROR]"), parent, trimmed)
	}

	if suggestion, ok := closestString(trimmed, allowed); ok {
		fmt.Fprintf(out, "%s did you mean `%s %s`?\n", ui.amber("[HINT]"), parent, suggestion)
	}
	fmt.Fprintf(out, "%s run `soroq %s --help`.\n", ui.blue("[HELP]"), parent)
}

func groupedRootCommands() map[string][]commandHelp {
	groups := map[string][]commandHelp{}
	for _, command := range rootCommands {
		groups[command.Group] = append(groups[command.Group], command)
	}
	for group := range groups {
		sort.Slice(groups[group], func(i, j int) bool {
			return groups[group][i].Name < groups[group][j].Name
		})
	}
	return groups
}

func printUnknownCommand(out io.Writer, command string) {
	ui := newTerminalUI(out)
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		printRootUsage(out)
		return
	}

	fmt.Fprintf(out, "%s unknown command %q\n", ui.red("[ERROR]"), trimmed)
	if suggestion, ok := closestRootCommand(trimmed); ok {
		fmt.Fprintf(out, "%s did you mean `%s`?\n", ui.amber("[HINT]"), suggestion)
	}
	fmt.Fprintf(out, "%s run `soroq --help` to see every command.\n", ui.blue("[HELP]"))
}

func printFatalError(out io.Writer, err error) {
	ui := newTerminalUI(out)
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "command failed"
	}

	fmt.Fprintf(out, "%s %s\n", ui.red("[ERROR]"), message)
	nextSteps := suggestionsForError(message)
	if len(nextSteps) == 0 {
		fmt.Fprintf(out, "%s rerun with the command-specific `--help` output, then check the inputs above.\n", ui.blue("[HELP]"))
		return
	}

	fmt.Fprintf(out, "%s next steps\n", ui.amber("[FIX]"))
	for _, step := range nextSteps {
		fmt.Fprintf(out, "  - %s\n", step)
	}
}

func suggestionsForError(message string) []string {
	lower := strings.ToLower(message)
	var suggestions []string
	add := func(step string) {
		for _, existing := range suggestions {
			if existing == step {
				return
			}
		}
		suggestions = append(suggestions, step)
	}

	switch {
	case strings.Contains(lower, "not logged in"), strings.Contains(lower, "operator token"), strings.Contains(lower, "unauthorized"), strings.Contains(lower, "401"):
		add("Run `soroq login` or set `SOROQ_CONTROL_PLANE_OPERATOR_TOKEN` for automation.")
		add("Verify the API target with `soroq whoami`.")
	}
	if strings.Contains(lower, "forbidden") || strings.Contains(lower, "403") {
		add("Check that the operator token has access to this app, channel, and release.")
	}
	if strings.Contains(lower, "pubspec.yaml") {
		add("Run the command from a Flutter project root or pass `--project-dir <path>`.")
	}
	if strings.Contains(lower, "soroq.yaml") {
		add("Run `soroq init` to create project configuration, then retry.")
	}
	if strings.Contains(lower, "soroq_flutter") {
		add("Add the runtime package with `flutter pub add soroq_flutter`, then rebuild the app.")
	}
	if strings.Contains(lower, "artifact") || strings.Contains(lower, ".apk") || strings.Contains(lower, ".aab") || strings.Contains(lower, ".ipa") {
		add("Pass the built artifact explicitly with `--artifact <path>`.")
	}
	if strings.Contains(lower, "adb") || strings.Contains(lower, "device") || strings.Contains(lower, "emulator") {
		add("Start an Android emulator or connect a device, then confirm it appears in `adb devices`.")
	}
	if strings.Contains(lower, "bundletool") {
		add("Install bundletool or pass the binary path with `--bundletool <path>`.")
	}
	if strings.Contains(lower, "java") || strings.Contains(lower, "jdk") {
		add("Install a JDK and ensure `java` is available on PATH.")
	}
	if strings.Contains(lower, "network") || strings.Contains(lower, "timeout") || strings.Contains(lower, "connection") || strings.Contains(lower, "request failed") {
		add("Check network access and confirm `SOROQ_API` points at the intended control plane.")
	}
	if strings.Contains(lower, "json") {
		add("Validate the JSON file, then retry with the exact file path.")
	}
	return suggestions
}

func closestRootCommand(input string) (string, bool) {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return "", false
	}

	bestName := ""
	bestScore := 99
	for _, command := range rootCommands {
		candidates := append([]string{command.Name}, command.Aliases...)
		candidates = append(candidates, command.SearchTerms...)
		for _, candidate := range candidates {
			score := levenshtein(input, strings.ToLower(candidate))
			if score < bestScore {
				bestScore = score
				bestName = command.Name
			}
		}
	}

	threshold := 2
	if len(input) > 8 {
		threshold = 3
	}
	return bestName, bestName != "" && bestScore <= threshold
}

func closestString(input string, choices []string) (string, bool) {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return "", false
	}

	bestName := ""
	bestScore := 99
	for _, choice := range choices {
		score := levenshtein(input, strings.ToLower(choice))
		if score < bestScore {
			bestScore = score
			bestName = choice
		}
	}

	threshold := 2
	if len(input) > 8 {
		threshold = 3
	}
	return bestName, bestName != "" && bestScore <= threshold
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return len(b)
	}
	if b == "" {
		return len(a)
	}

	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}

	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = minInt(
				current[j-1]+1,
				previous[j]+1,
				previous[j-1]+cost,
			)
		}
		copy(previous, current)
	}
	return previous[len(b)]
}

func minInt(values ...int) int {
	if len(values) == 0 {
		return 0
	}
	best := values[0]
	for _, value := range values[1:] {
		if value < best {
			best = value
		}
	}
	return best
}
