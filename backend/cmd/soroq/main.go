package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// buildVersion is stamped at build time via `-ldflags "-X main.buildVersion=<v>"`.
var buildVersion = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "version", "-v", "--version":
		// `soroq version --bogus` and `soroq version bogusword` both printed the version and exited 0.
		// The command takes nothing, so an argument here is a reader who expects something else --
		// most likely `soroq --version` confusion, or a subcommand that does not exist.
		if extra := os.Args[2:]; len(extra) > 0 {
			if len(extra) == 1 && isHelpFlag(extra[0]) {
				fmt.Println("usage: soroq version\n\nPrints the installed CLI version and exits.")
				return
			}
			fmt.Fprintln(os.Stderr, refuseUnconsumedArguments("version", extra, nil))
			os.Exit(1)
		}
		fmt.Printf("soroq %s\n", buildVersion)
		return
	case "app":
		err = runApp(os.Args[2:])
	case "cache":
		err = runCache(os.Args[2:])
	case "catalog":
		err = runCatalog(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "flutter":
		err = runFlutter(os.Args[2:])
	case "frontend":
		err = runFrontend(os.Args[2:])
	case "init":
		err = runInit(os.Args[2:])
	case "inspect":
		err = runInspect(os.Args[2:])
	case "login":
		err = runLogin(os.Args[2:])
	case "logout":
		err = runLogout(os.Args[2:])
	case "patch":
		err = runPatch(os.Args[2:])
	case "patches":
		// `patches` IS THE READ SIDE, AND ITS HELP MUST SAY SO.
		//
		// It routes to runPatch because they share a dispatch table, and a bare `soroq patches --help`
		// therefore printed `soroq patch`'s usage verbatim: it never mentioned the word `patches` and
		// advertised publish targets, while the root help describes it as "list your updates, inspect
		// one, promote it, or change its track". A reader following `--help` was told about a
		// different command.
		// EVERY PATH THROUGH THE ALIAS, not just the bare and --help ones.
		//
		// The first fix covered `soroq patches` and `soroq patches --help`. It did not cover
		// `soroq patches bogussub`, which fell through to runPatch and printed the PUBLISHING usage --
		// verbatim the defect the fix was for, one argument along. Nor did it cover
		// `soroq patches list --help`, which still answered `usage: soroq patch ...`.
		err = runPatches(os.Args[2:])
	case "preview":
		err = runPreview(os.Args[2:])
	case "release":
		err = runRelease(os.Args[2:])
	case "rollback":
		err = runRollback(os.Args[2:])
	case "setup":
		err = runSetup(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "support-bundle":
		err = runSupportBundle(os.Args[2:])
	case "toolchain":
		err = runToolchain(os.Args[2:])
	case "uninstall":
		err = runUninstall(os.Args[2:])
	case "update":
		err = runUpdate(os.Args[2:])
	case "whoami":
		err = runWhoami(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		// SAY WHAT WAS WRONG, THEN SHOW THE LIST. Printing the whole help for an unrecognised word
		// leaves a reader who mistyped scanning thirty lines to work out what happened. The mistake
		// goes first, on stderr, and a near match is offered when there is one.
		fmt.Fprintf(os.Stderr, "soroq: unknown command %q\n", os.Args[1])
		if suggestion := closestCommand(os.Args[1]); suggestion != "" {
			fmt.Fprintf(os.Stderr, "Did you mean `soroq %s`?\n", suggestion)
		}
		fmt.Fprintln(os.Stderr)
		usage()
		os.Exit(2)
	}

	if err == nil {
		return
	}
	if errors.Is(err, errAlreadyPrinted) {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	// SAY WHAT TO DO ABOUT IT. Some failures have exactly one fix and the raw server body does not
	// name it: a reader who sees {"error":"operator authentication required"} has been told a fact and
	// not an action. Added HERE rather than at each call site, because there are a dozen of those and
	// the thirteenth would be the one that forgot.
	if next := suggestedFix(err); next != "" {
		fmt.Fprintln(os.Stderr, next)
	}
	os.Exit(1)
}

// suggestedFix maps a failure onto the one command that resolves it, or returns empty when there is no
// single answer. Guessing wrong sends someone down a path they did not ask for, so this stays narrow.
func suggestedFix(err error) string {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "operator authentication required"),
		strings.Contains(text, "not logged in"),
		strings.Contains(text, "401"):
		return "Run `soroq login` to sign in, then try again."
	case strings.Contains(text, "no soroq frontend is installed"),
		strings.Contains(text, "flutter was not found"):
		return "Run `soroq setup android` (or `soroq setup ios`) to install what this needs."
	case strings.Contains(text, "no soroq.yaml"), strings.Contains(text, "soroq.yaml not found"):
		return "Run `soroq init` in your Flutter project first."
	case strings.Contains(text, "does not allow"):
		return "Ask an admin of this app to grant you a developer or admin role."
	default:
		return ""
	}
}

// runCatalog dispatches the `soroq catalog` subcommands. Today the only subcommand is `publish` (operator-
// only; the DEFAULT public build links a friendly stub via operator_stubs.go). Mirrors runToolchain's
// help/default handling so both build tags behave identically.
func runCatalog(args []string) error {
	if len(args) == 0 {
		catalogUsage()
		return errAlreadyPrinted
	}
	switch args[0] {
	case "publish":
		return runCatalogPublish(args[1:])
	case "publish-v2":
		return runCatalogPublishV2(args[1:])
	case "-h", "--help", "help":
		catalogUsage()
		return nil
	default:
		catalogUsage()
		return errAlreadyPrinted
	}
}

func catalogUsage() {
	fmt.Fprintln(os.Stderr, `usage: soroq catalog <subcommand> [flags]

subcommands:
  publish     operator: sign + PUT the soroq.catalog.v1 compatibility catalog (per-platform frontend + toolchain pins)
  publish-v2  operator: sign + PUT the soroq.catalog.v2 version matrix (per-platform pairs selected by exact Flutter revision)`)
}

func usage() {
	// GROUPED AND BEGINNER-FACING ON PURPOSE.
	//
	// The previous text omitted `setup` entirely -- a command that dispatches and runs, so it was
	// implemented and undiscoverable at the same time. It also described the ordinary workflow in
	// operator terms ("publish hosted Android asset or JSON config patches" for a command that also
	// serves iOS) and told users to roll back "by patch id", when the product goal is that nobody
	// handles raw ids during the normal workflow.
	//
	// TestUsageListsEveryDispatchedCommand parses the switch in this file and fails if a command is
	// added there without a line here, so this text cannot silently drift again.
	fmt.Fprintln(os.Stderr, `usage: soroq <command> [flags]

Start here:
  init       add Soroq to a Flutter app (creates soroq.yaml)
  setup      prepare a platform for Soroq builds (soroq setup ios|android)
  login      sign in to the Soroq control plane in your browser
  doctor     check whether this project is ready, and say what to fix

Ship updates:
  release    register a built app version as the base for future updates
  patch      publish a code update for the current project
  patches    list your updates, inspect one, promote it, or change its track
  rollback   return users to the previous good version
  preview    show what this project would publish, without publishing

Look around:
  status     show local and remote state for this project
  whoami     show who you are signed in as, with scopes and expiry
  inspect    inspect Soroq metadata inside a built artifact
  app        list or manage your apps

Manage the toolchain:
  flutter    choose which supported Flutter version this project uses
  frontend   install, list or diagnose the Soroq Flutter build frontend
  toolchain  install, list or diagnose build-time engine toolchains
  cache      list or clean cached frontends and toolchains under ~/.soroq

Maintain the CLI:
  version    print the installed CLI version
  support-bundle  collect diagnostics to send for help, with secrets removed
  update     update the Soroq CLI (--check to look without installing)
  logout     sign out and remove stored credentials
  uninstall  remove ~/.soroq and the installed binaries (requires --yes)

Operator:
  catalog    publish the signed per-platform compatibility catalog

Run soroq <command> --help for the flags a command accepts.`)
}

// closestCommand offers the nearest command name when a typo is close enough to be worth guessing.
//
// The bar is deliberately high: a wrong suggestion sends someone down a path they did not ask for, so
// this only fires for a prefix match or a single-character slip.
func closestCommand(typed string) string {
	typed = strings.ToLower(strings.TrimSpace(typed))
	if typed == "" {
		return ""
	}
	best := ""
	for _, command := range dispatchableCommands {
		if command == typed {
			return command
		}
		if strings.HasPrefix(command, typed) && len(typed) >= 3 {
			if best == "" || len(command) < len(best) {
				best = command
			}
			continue
		}
		if editDistanceWithin(typed, command, 1) && best == "" {
			best = command
		}
	}
	return best
}

// dispatchableCommands is the list `soroq --help` shows, kept beside the dispatch it describes. The
// usage-contract test holds the two together, so this cannot drift into naming something that does not
// exist.
var dispatchableCommands = []string{
	"app", "cache", "catalog", "doctor", "flutter", "frontend", "init", "inspect", "login", "logout",
	"patch", "patches", "preview", "release", "rollback", "setup", "status", "support-bundle",
	"toolchain", "uninstall", "update", "version", "whoami",
}

// editDistanceWithin reports whether two words differ by at most `limit` single-character edits.
func editDistanceWithin(a, b string, limit int) bool {
	if abs(len(a)-len(b)) > limit {
		return false
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min3(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		copy(previous, current)
	}
	return previous[len(b)] <= limit
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
