package main

import (
	"errors"
	"fmt"
	"os"
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
		fmt.Printf("soroq %s\n", buildVersion)
		return
	case "app":
		err = runApp(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
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
		err = runPatch(os.Args[2:])
	case "preview":
		err = runPreview(os.Args[2:])
	case "release":
		err = runRelease(os.Args[2:])
	case "rollback":
		err = runRollback(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "toolchain":
		err = runToolchain(os.Args[2:])
	case "whoami":
		err = runWhoami(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
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
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: soroq <command> [flags]

commands:
  app      register or manage Soroq apps in the control plane
  doctor   diagnose toolchain, signing, project, and control-plane readiness
  frontend install, list, or diagnose the Soroq Flutter build frontend
  init     create a project-level soroq.yaml in a Flutter app
  inspect  inspect bundled Soroq metadata in local artifacts
  login    store hosted control-plane operator credentials
  logout   remove stored hosted control-plane credentials
  patch    publish hosted Android asset or JSON config patches
  patches  list, inspect, stage, or promote hosted patches
  preview  preview hosted Android release and patch state
  release  register a built release artifact with the control plane
  rollback roll back a hosted patch by patch id
  status    inspect whether a Flutter app looks Soroq-ready
  toolchain publish, install, list, or diagnose hosted build-time engine toolchains
  whoami    verify the current hosted control-plane operator`)
}
