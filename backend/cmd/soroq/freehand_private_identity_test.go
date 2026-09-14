package main

import (
	"testing"
)

// --- the private-identity gate ------------------------------------------------------------------

// The natural control comes from one real gen_snapshot run over one file: CleanApp::build hit=1 next to
// _CleanHomeState@57413802::build hit=0. Public identities must pass; private ones must be refused.
func TestPrivateIdentitiesAreRefusedAndPublicOnesAreNot(t *testing.T) {
	public := []string{
		"package:ioslane/main.dart::::headline",
		"package:ioslane/main.dart::::detail",
		"package:ioslane/main.dart::CleanApp::build",
		"package:ioslane/main.dart::CleanHome::createState",
	}
	if got := freehandPrivateIdentities(public); len(got) != 0 {
		t.Fatalf("public identities were refused: %v", got)
	}
	private := []string{
		"package:ioslane/main.dart::_CleanHomeState::build",
		"package:ioslane/main.dart::::_privateTopLevel",
		"package:ioslane/main.dart::CleanApp::_privateMethod",
	}
	got := freehandPrivateIdentities(private)
	if len(got) != len(private) {
		t.Fatalf("expected all %d private identities refused, got %d: %v", len(private), len(got), got)
	}
}

// A mixed batch must be refused: a freehand batch is all-or-nothing, so one unpatchable private
// identity commits zero redirects for every public one beside it.
func TestOnePrivateIdentityBlocksTheWholeBatch(t *testing.T) {
	mixed := []string{
		"package:ioslane/main.dart::::headline",
		"package:ioslane/main.dart::_CleanHomeState::build",
	}
	got := freehandPrivateIdentities(mixed)
	if len(got) != 1 || got[0] != "package:ioslane/main.dart::_CleanHomeState::build" {
		t.Fatalf("the private member of a mixed batch was not identified: %v", got)
	}
}
