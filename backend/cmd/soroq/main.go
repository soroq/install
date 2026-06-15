package main

import (
	"errors"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		printRootUsage(os.Stderr)
		return 2
	}

	var err error
	switch args[0] {
	case "app":
		err = runApp(args[1:])
	case "init":
		err = runInit(args[1:])
	case "inspect":
		err = runInspect(args[1:])
	case "login":
		err = runLogin(args[1:])
	case "logout":
		err = runLogout(args[1:])
	case "patch":
		err = runPatch(args[1:])
	case "release":
		err = runRelease(args[1:])
	case "rollback":
		err = runRollback(args[1:])
	case "status":
		err = runStatus(args[1:])
	case "whoami":
		err = runWhoami(args[1:])
	case "-h", "--help", "help":
		printRootUsage(os.Stdout)
		return 0
	default:
		printUnknownCommand(os.Stderr, args[0])
		return 2
	}

	if err == nil {
		return 0
	}
	if errors.Is(err, errAlreadyPrinted) {
		return 1
	}
	printFatalError(os.Stderr, err)
	return 1
}

func usage() {
	printRootUsage(os.Stderr)
}
