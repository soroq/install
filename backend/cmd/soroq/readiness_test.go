package main

import (
	"strings"
	"testing"
)

// FALSE READINESS is the defect this model exists to prevent: a surface reporting green before its
// predicate is true. These tests plant that defect in each shape it can take.

func TestGreenStateIsUnrepresentableWithoutEvidence(t *testing.T) {
	s := okState(stConfig, "project is configured", "")
	if s.Status == readyOK {
		t.Fatal("a state was reported ok with no evidence; green with nothing behind it must be impossible")
	}
	if s.Status != readyUnknown {
		t.Errorf("an evidence-less state should degrade to unknown, got %q", s.Status)
	}
}

func TestUnknownIsNotTreatedAsReady(t *testing.T) {
	// A check that could not run has not passed. Counting it as a pass is how a surface reports green
	// while blind -- for example an offline status claiming the server state is fine.
	p := platformReadiness{Platform: "ios", States: []readyState{
		okState(stPackage, "pkg", "found"),
		unknownState(stRemotePatch, "active update on the server", "not checked"),
	}}
	if p.Ready() {
		t.Error("a model containing an unknown state reported Ready; unknown is not a pass")
	}
}

func TestEmptyModelIsNotReady(t *testing.T) {
	if (platformReadiness{Platform: "ios"}).Ready() {
		t.Error("a model with no states reported Ready; nothing checked is not everything passing")
	}
}

func TestBlockedStateAlwaysOffersACommandThatFitsTheState(t *testing.T) {
	st := projectStatus{} // nothing set up at all
	p := computePlatformReadiness("android", st, projectCLIState{}, soroqLock{}, false, "")
	if p.Ready() {
		t.Fatal("an empty project reported ready")
	}
	for _, s := range p.States {
		if s.Status == readyBlocked && strings.TrimSpace(s.NextCommand) == "" {
			t.Errorf("state %q is blocked but offers no next command; the developer is told no and not told what to do", s.ID)
		}
	}
	// The first action for a bare project must be something that works right now.
	if got := p.FirstAction(); got != "flutter pub add soroq_flutter" {
		t.Errorf("first action for a bare project was %q; it must be the earliest unmet step", got)
	}
}

func TestReadinessTracksEachPredicateIndependently(t *testing.T) {
	st := projectStatus{
		HasSoroqFlutterDependency: true,
		HasSoroqConfig:            true,
		AppID:                     "com.example.app",
		Channel:                   "stable",
		HasManifestTrust:          true,
		PubspecPath:               "pubspec.yaml",
	}
	lock := soroqLock{Platforms: map[string]soroqLockPin{
		"android": {ToolchainVersion: "tc", FrontendVersion: "fe", ReleaseID: "rel-1", Version: "1.0.0+1"},
	}}
	p := computePlatformReadiness("android", st, projectCLIState{}, lock, true, "operator@example.com")

	for _, id := range []string{stPackage, stConfig, stAuth, stToolchain, stLock, stSigning, stRelease} {
		s, ok := p.state(id)
		if !ok {
			t.Fatalf("state %q missing from the model", id)
		}
		if s.Status != readyOK {
			t.Errorf("state %q should be ok for a fully set up project, got %q (%s)", id, s.Status, s.Detail)
		}
		if strings.TrimSpace(s.Evidence) == "" {
			t.Errorf("state %q is ok but records no evidence", id)
		}
	}
	// Remote is still unknown, because nothing asked the server.
	if s, _ := p.state(stRemotePatch); s.Status != readyUnknown {
		t.Errorf("remote state should be unknown before any network call, got %q", s.Status)
	}
	if p.Ready() {
		t.Error("model reported Ready while the remote state was never checked")
	}
}

// Signing is a separate predicate from configuration. A project with soroq.yaml but no trust keys
// cannot verify an update, and must not be green.
// EVERY platform, not just one. The first version of this test checked ios only, and a planted defect
// on the android branch went uncaught -- a control that covers half the code proves half the property.
func TestMissingSigningKeysBlocksEvenWithConfigPresent(t *testing.T) {
	for _, platform := range []string{"ios", "android"} {
		st := projectStatus{
			HasSoroqFlutterDependency: true, HasSoroqConfig: true, AppID: "a", PubspecPath: "pubspec.yaml",
			HasManifestTrust: false,
		}
		p := computePlatformReadiness(platform, st, projectCLIState{}, soroqLock{}, true, "op")
		s, ok := p.state(stSigning)
		if !ok {
			t.Fatalf("%s: signing state missing", platform)
		}
		if s.Status == readyOK {
			t.Errorf("%s: signing reported ok with no manifest_trust keys; the app could not verify an update", platform)
		}
	}
}

func TestRemoteStateCannotBePromotedWithoutBeingSupplied(t *testing.T) {
	p := computePlatformReadiness("ios", projectStatus{}, projectCLIState{}, soroqLock{}, false, "")
	before, _ := p.state(stRemotePatch)
	if before.Status != readyUnknown {
		t.Fatalf("precondition: remote should start unknown, got %q", before.Status)
	}
	p2 := p.withRemoteState(okState(stRemotePatch, "active update on the server", "server says version 4 active"))
	after, _ := p2.state(stRemotePatch)
	if after.Status != readyOK || after.Evidence == "" {
		t.Error("withRemoteState must carry evidence from the caller that actually made the call")
	}
	// The original is untouched: promoting is explicit, never in place.
	again, _ := p.state(stRemotePatch)
	if again.Status != readyUnknown {
		t.Error("the original model was mutated; promotion must be explicit and local to the caller")
	}
}

func TestRenderShowsEvidenceForGreenAndDetailForBlocked(t *testing.T) {
	p := platformReadiness{Platform: "android", States: []readyState{
		okState(stPackage, "soroq_flutter is a dependency", "found in pubspec.yaml"),
		blockedState(stConfig, "project is configured", "no soroq.yaml in this project", "soroq init"),
	}}
	out := renderReadiness(p)
	if !strings.Contains(out, "found in pubspec.yaml") {
		t.Error("a green state rendered without its evidence")
	}
	if !strings.Contains(out, "no soroq.yaml") {
		t.Error("a blocked state rendered without saying why")
	}
	if !strings.Contains(out, "Next:  soroq init") {
		t.Error("render did not print a next command that fits the state")
	}
}
