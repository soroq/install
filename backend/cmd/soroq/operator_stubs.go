//go:build !soroq_operator

package main

// Default (public) build stubs for the operator-only publish commands. The real implementations live in
// toolchain_publish.go + frontend_publish.go behind `//go:build soroq_operator`; the DEFAULT public CLI
// omits them (and thereby links none of internal/artifacts or internal/store). These stubs keep the
// `soroq toolchain`/`soroq frontend` dispatchers compiling and make `... publish` fail with a clear,
// non-panicking operator-only error instead of a missing symbol.

import "errors"

func runToolchainPublish(args []string) error {
	return errors.New("`soroq toolchain publish` is an operator-only command and is not built into the public CLI")
}

func runToolchainStageArchive(args []string) error {
	return errors.New("`soroq toolchain stage-archive` is an operator-only command and is not built into the public CLI")
}

func runFrontendPublish(args []string) error {
	return errors.New("`soroq frontend publish` is an operator-only command and is not built into the public CLI")
}

func runFrontendStageArchive(args []string) error {
	return errors.New("`soroq frontend stage-archive` is an operator-only command and is not built into the public CLI")
}

func runFrontendAdoptArchive(args []string) error {
	return errors.New("`soroq frontend adopt-archive` is an operator-only command and is not built into the public CLI")
}

func runCatalogPublish(args []string) error {
	return errors.New("`soroq catalog publish` is an operator-only command and is not built into the public CLI")
}

func runCatalogPublishV2(args []string) error {
	return errors.New("`soroq catalog publish-v2` is an operator-only command and is not built into the public CLI")
}
