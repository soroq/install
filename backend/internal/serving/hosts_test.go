package serving

// The hosted control plane serves operator writes and device reads from DIFFERENT origins. Conflating
// them produced a publish summary advertising `https://soroq.dev/api/v1/engine/…`, which returns 401 to
// the unauthenticated read a device performs — so a developer whose patch had published correctly saw a
// URL that appeared broken. These tests pin the separation in both directions: the device base must be
// derived for hosted operator bases, and must NOT be invented for self-hosted ones.

import "testing"

func TestHostedOperatorBaseMapsToTheDeviceHost(t *testing.T) {
	for operator, want := range map[string]string{
		"https://soroq.dev/api":     "https://api.soroq.dev",
		"https://soroq.dev/api/":    "https://api.soroq.dev",
		"  https://soroq.dev/api  ": "https://api.soroq.dev",
		"https://www.soroq.dev/api": "https://api.soroq.dev",
	} {
		if got := DeviceServeBase(operator); got != want {
			t.Errorf("DeviceServeBase(%q) = %q, want %q", operator, got, want)
		}
	}
}

// A device URL must never be built from a host that rejects unauthenticated reads.
func TestDeviceBaseIsNeverAnOperatorOnlyHost(t *testing.T) {
	for _, operator := range []string{"https://soroq.dev/api", "https://www.soroq.dev/api"} {
		device := DeviceServeBase(operator)
		if IsOperatorOnlyBase(device) {
			t.Errorf("DeviceServeBase(%q) returned an operator-only host %q", operator, device)
		}
	}
	if IsOperatorOnlyBase("https://api.soroq.dev") {
		t.Error("the device host was classified as operator-only")
	}
}

// A self-hosted or local control plane serves both roles from one origin. Rewriting it would break
// every non-hosted deployment, so anything unknown maps to itself.
func TestSelfHostedBasesAreUnchanged(t *testing.T) {
	for _, base := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:9000",
		"https://soroq.internal.example.com",
		"https://api.soroq.dev", // already the device host
	} {
		if got := DeviceServeBase(base); got != base {
			t.Errorf("DeviceServeBase(%q) = %q, want it unchanged", base, got)
		}
	}
}

func TestEmptyBaseDoesNotGuess(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if got := DeviceServeBase(in); got != "" {
			t.Errorf("DeviceServeBase(%q) = %q, want empty rather than a guessed host", in, got)
		}
		if got := EngineChannelURL(in, "app", "stable"); got != "" {
			t.Errorf("EngineChannelURL(%q,…) = %q, want empty", in, got)
		}
	}
}

// THE REPORTED URL: built from the operator base, it must name the device host and the right path.
func TestEngineChannelURLIsDeviceFacing(t *testing.T) {
	got := EngineChannelURL("https://soroq.dev/api", "dev.soroq.canon3", "stable")
	want := "https://api.soroq.dev/v1/engine/dev.soroq.canon3/stable"
	if got != want {
		t.Fatalf("EngineChannelURL = %q, want %q", got, want)
	}
	// Self-hosted keeps its own origin.
	if got := EngineChannelURL("http://localhost:8080", "app", "beta"); got != "http://localhost:8080/v1/engine/app/beta" {
		t.Errorf("self-hosted engine URL = %q", got)
	}
}

// Idempotent: reporting a URL derived from an already-device base must not shift it again.
func TestDeviceBaseDerivationIsIdempotent(t *testing.T) {
	once := DeviceServeBase("https://soroq.dev/api")
	if twice := DeviceServeBase(once); twice != once {
		t.Errorf("re-deriving changed %q to %q", once, twice)
	}
}
