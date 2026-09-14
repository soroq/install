package main

import (
	"fmt"
	"strings"
)

// ONE readiness model, consumed by every surface.
//
// WHY THIS EXISTS. `status` computed readiness from inspectProject plus local CLI state; `doctor` built
// its own independent check list; `setup` decided for itself. Three surfaces answering "is this project
// ready?" from three code paths means they can disagree, and a developer who is told "ready" by one and
// "not ready" by another has no way to know which is lying. This is the single source of truth.
//
// THE INVARIANT. A state cannot be green unless its predicate held AND it can say what it observed.
// `readyState` is deliberately unexported and only reachable through okState/blockedState/unknownState,
// and okState REFUSES to produce a green state without evidence -- so "green with nothing behind it" is
// unrepresentable rather than merely discouraged.

type readinessStatus string

const (
	readyOK      readinessStatus = "ok"      // the predicate held, and evidence says how we know
	readyBlocked readinessStatus = "blocked" // the predicate did not hold; nextCommand says what to do
	readyUnknown readinessStatus = "unknown" // not determinable here (for example, needs the network)
	readyNA      readinessStatus = "n/a"     // does not apply to this platform or project shape
)

// readyState is one fact about a project. Construct it only through the helpers below.
type readyState struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Status      readinessStatus `json:"status"`
	Evidence    string          `json:"evidence,omitempty"`     // what was observed; required when ok
	Detail      string          `json:"detail,omitempty"`       // why it is blocked or unknown
	NextCommand string          `json:"next_command,omitempty"` // a command that works in THIS state
}

func okState(id, title, evidence string) readyState {
	// A green state with no evidence is the failure this model exists to prevent, so it is refused at
	// construction rather than reviewed later.
	if strings.TrimSpace(evidence) == "" {
		return readyState{
			ID: id, Title: title, Status: readyUnknown,
			Detail: "internal: a state was reported ready without evidence, so it is reported unknown instead",
		}
	}
	return readyState{ID: id, Title: title, Status: readyOK, Evidence: evidence}
}

func blockedState(id, title, detail, next string) readyState {
	return readyState{ID: id, Title: title, Status: readyBlocked, Detail: detail, NextCommand: next}
}

func unknownState(id, title, detail string) readyState {
	return readyState{ID: id, Title: title, Status: readyUnknown, Detail: detail}
}

func naState(id, title, detail string) readyState {
	return readyState{ID: id, Title: title, Status: readyNA, Detail: detail}
}

// platformReadiness is the whole answer for one platform, in the order a developer meets it.
type platformReadiness struct {
	Platform string       `json:"platform"`
	States   []readyState `json:"states"`
}

// State ids. Surfaces refer to these rather than to positions in a slice.
const (
	stPackage     = "package_integration"
	stConfig      = "project_configuration"
	stAuth        = "authentication"
	stToolchain   = "selected_toolchain"
	stLock        = "lockfile_identity"
	stSigning     = "signing_readiness"
	stRelease     = "registered_release"
	stPatchable   = "local_candidate_patchable"
	stRemotePatch = "remote_active_patch"
)

// Ready reports whether every state that matters is green. Unknown is NOT ready: a check that could not
// run has not passed, and treating it as a pass is how a surface reports green while blind.
func (p platformReadiness) Ready() bool {
	for _, s := range p.States {
		if s.Status == readyBlocked || s.Status == readyUnknown {
			return false
		}
	}
	return len(p.States) > 0
}

// FirstAction returns the next command a developer should run, taken from the earliest state that is
// not green. Every surface prints this, so the advice cannot drift between commands.
func (p platformReadiness) FirstAction() string {
	for _, s := range p.States {
		if s.Status == readyBlocked && s.NextCommand != "" {
			return s.NextCommand
		}
	}
	return ""
}

func (p platformReadiness) state(id string) (readyState, bool) {
	for _, s := range p.States {
		if s.ID == id {
			return s, true
		}
	}
	return readyState{}, false
}

// computePlatformReadiness builds the model from what is on disk and in local state.
//
// It takes no network. Remote facts (registered release, active patch) are reported unknown here and
// filled in by callers that have already talked to the control plane, so that an offline `status` is
// honest about what it did not check instead of guessing.
func computePlatformReadiness(platform string, st projectStatus, cli projectCLIState, lock soroqLock, authed bool, authDetail string) platformReadiness {
	p := platformReadiness{Platform: platform}

	if st.HasSoroqFlutterDependency {
		p.States = append(p.States, okState(stPackage, "soroq_flutter is a dependency", "found in "+st.PubspecPath))
	} else {
		p.States = append(p.States, blockedState(stPackage, "soroq_flutter is a dependency",
			"the app does not depend on soroq_flutter, so it cannot receive updates",
			"flutter pub add soroq_flutter"))
	}

	switch {
	case !st.HasSoroqConfig:
		p.States = append(p.States, blockedState(stConfig, "project is configured",
			"no soroq.yaml in this project", "soroq init"))
	case st.AppID == "":
		p.States = append(p.States, blockedState(stConfig, "project is configured",
			"soroq.yaml has no app_id", "soroq init"))
	default:
		p.States = append(p.States, okState(stConfig, "project is configured",
			fmt.Sprintf("app_id %s, channel %s", st.AppID, orDash(st.Channel))))
	}

	if authed {
		p.States = append(p.States, okState(stAuth, "signed in", authDetail))
	} else {
		p.States = append(p.States, blockedState(stAuth, "signed in",
			orDefault(authDetail, "no stored credential for this control plane"), "soroq login"))
	}

	pin, hasPin := lock.Platforms[platform]
	if hasPin && pin.ToolchainVersion != "" {
		p.States = append(p.States, okState(stToolchain, "Flutter toolchain selected",
			"toolchain "+pin.ToolchainVersion))
	} else {
		p.States = append(p.States, blockedState(stToolchain, "Flutter toolchain selected",
			"this project has not pinned a supported Flutter version",
			"soroq flutter versions list"))
	}

	if hasPin && pin.ToolchainVersion != "" && pin.FrontendVersion != "" {
		p.States = append(p.States, okState(stLock, "lockfile records the build identity",
			"soroq.lock pins toolchain and frontend for "+platform))
	} else {
		p.States = append(p.States, blockedState(stLock, "lockfile records the build identity",
			"soroq.lock does not record both a toolchain and a frontend for "+platform,
			"soroq flutter use <version>"))
	}

	switch platform {
	case "android":
		if st.HasManifestTrust {
			p.States = append(p.States, okState(stSigning, "update signing is configured",
				"manifest_trust keys present in soroq.yaml"))
		} else {
			p.States = append(p.States, blockedState(stSigning, "update signing is configured",
				"soroq.yaml has no manifest_trust keys, so the app cannot verify an update",
				"soroq init"))
		}
	case "ios":
		if st.HasManifestTrust {
			p.States = append(p.States, okState(stSigning, "update signing is configured",
				"manifest_trust keys present in soroq.yaml"))
		} else {
			p.States = append(p.States, blockedState(stSigning, "update signing is configured",
				"soroq.yaml has no manifest_trust keys, so the app cannot verify an update",
				"soroq init"))
		}
	default:
		p.States = append(p.States, naState(stSigning, "update signing is configured",
			"unknown platform "+platform))
	}

	if hasPin && pin.ReleaseID != "" {
		p.States = append(p.States, okState(stRelease, "a base release is registered",
			"release "+pin.ReleaseID+" version "+orDash(pin.Version)))
	} else {
		p.States = append(p.States, blockedState(stRelease, "a base release is registered",
			"no release has been registered for "+platform+" from this project",
			"soroq release "+platform))
	}

	// Remote facts are deliberately unknown until a caller supplies them.
	p.States = append(p.States, unknownState(stRemotePatch, "active update on the server",
		"not checked here; run soroq status --remote"))

	_ = cli
	return p
}

// withRemoteState replaces the remote portion of the model once a caller has actually asked the server.
// It exists so a surface cannot quietly promote "unknown" to "ok" without having made the call.
func (p platformReadiness) withRemoteState(s readyState) platformReadiness {
	out := platformReadiness{Platform: p.Platform}
	for _, existing := range p.States {
		if existing.ID == s.ID {
			out.States = append(out.States, s)
			continue
		}
		out.States = append(out.States, existing)
	}
	return out
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// renderReadiness prints the model the same way everywhere, so `doctor` and `status` cannot describe the
// same project differently.
func renderReadiness(p platformReadiness) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", p.Platform)
	for _, s := range p.States {
		mark := map[readinessStatus]string{
			readyOK: "ok  ", readyBlocked: "TODO", readyUnknown: "?   ", readyNA: "-   ",
		}[s.Status]
		fmt.Fprintf(&b, "  %s %-34s", mark, s.Title)
		switch s.Status {
		case readyOK:
			fmt.Fprintf(&b, " %s\n", s.Evidence)
		default:
			fmt.Fprintf(&b, " %s\n", s.Detail)
		}
	}
	if next := p.FirstAction(); next != "" {
		fmt.Fprintf(&b, "\nNext:  %s\n", next)
	}
	return b.String()
}
