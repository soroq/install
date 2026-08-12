// Package serving resolves the DEVICE-READ base URL from an operator/write base.
//
// The hosted control plane exposes two distinct endpoints for the same data:
//
//	operator / write   https://soroq.dev/api      authenticated; registers patches, uploads bundles
//	device / read      https://api.soroq.dev      unauthenticated; serves manifest.json, manifest.sig, bytecode
//
// They are NOT interchangeable. The operator host answers a device-style read with 401, so printing it
// as the device-serve base tells a developer their patch did not publish when it did — and copying it
// into an app would produce a client that can never fetch an update.
//
// The generated iOS bootstrap already pins the device host directly. This package exists so every
// place that REPORTS a device URL derives it the same way, instead of formatting whatever operator base
// happened to be in scope. Both `soroq` and `soroqctl` use it, so the two binaries cannot drift.
package serving

import "strings"

// hostedOperatorToDevice maps a known hosted operator/write base to its device-read base.
//
// Only the hosted deployment splits these hosts. A self-hosted or local control plane serves both roles
// from one origin, so anything not listed here maps to itself — which is the correct answer for
// `http://localhost:8080` and for a device base that was already passed in.
var hostedOperatorToDevice = map[string]string{
	"https://soroq.dev/api":     "https://api.soroq.dev",
	"http://soroq.dev/api":      "http://api.soroq.dev",
	"https://www.soroq.dev/api": "https://api.soroq.dev",
}

// DeviceServeBase returns the base a DEVICE should read from, given an operator/write base.
// The result never carries a trailing slash. An empty input returns empty rather than a guess.
func DeviceServeBase(operatorAPIBase string) string {
	base := strings.TrimRight(strings.TrimSpace(operatorAPIBase), "/")
	if base == "" {
		return ""
	}
	if device, ok := hostedOperatorToDevice[base]; ok {
		return device
	}
	return base
}

// IsOperatorOnlyBase reports whether base is a known operator/write host that will reject
// unauthenticated device reads. Used to assert that a device-facing URL is never built from one.
func IsOperatorOnlyBase(base string) bool {
	_, ok := hostedOperatorToDevice[strings.TrimRight(strings.TrimSpace(base), "/")]
	return ok
}

// EngineChannelURL builds the device-facing engine base for an app+channel, always from the
// device-read host. This is the single place that shape is constructed for reporting.
func EngineChannelURL(operatorAPIBase, appID, channel string) string {
	device := DeviceServeBase(operatorAPIBase)
	if device == "" {
		return ""
	}
	return device + "/v1/engine/" + appID + "/" + channel
}
