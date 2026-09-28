package main

// [soroq] Opt-in signed, upload-ready IPA for the iOS engine lane (`soroq release ios --engine --build
// --ipa out.ipa --signing-identity … --provisioning-profile …`).
//
// WHY THE CLI SIGNS THE .app ITSELF INSTEAD OF DRIVING `xcodebuild -exportArchive` / `flutter build ipa`.
//
//   - The engine lane writes soroq_base_identity.json into the built .app AFTER the Flutter build, because
//     two of its fields are hashes of the compiled kernel (see freehand_identity_asset.go). Anything that
//     signs inside the build — `flutter build ipa`, `xcodebuild archive` — signs a bundle that the CLI
//     then edits, so that signature is broken by construction. Signing has to happen after delivery.
//   - `-exportArchive` needs an .xcarchive, which the `flutter build ios --local-engine` lane does not
//     produce. A hand-assembled archive works, but manual export resolves profiles by name/UUID from
//     ~/Library/MobileDevice/Provisioning Profiles only, may consult the Xcode account, and re-processes
//     the bundle opaquely. It re-signs Flutter.framework exactly as codesign does, so it buys nothing for
//     the SOROQ engine assertion — and it would make the "same app the release registered" proof depend
//     on a black box.
//   - Explicit codesign is deterministic, takes profiles from any path, fails early with actionable
//     messages, is unit-testable with a stubbed exec, and is the exact recipe App Store Connect accepted
//     for the pilot app (altool --validate-app passed).
//
// What export would add and this does NOT: SwiftSupport/. Xcode only embeds Swift runtime dylibs
// (Frameworks/libswift*.dylib) for deployment targets below iOS 12.2; Flutter's minimum is iOS 13, so a
// Flutter app has none. If one is present anyway this REFUSES rather than ship an IPA App Store Connect
// would reject for a missing SwiftSupport folder. Watch apps and App Clips are refused for the same
// reason: they need their own profiles and nested packaging this path does not implement.
//
// Credentials: this never accepts, reads or logs a password. The keychain holding the identity must be
// unlocked by the caller (`security unlock-keychain`), exactly as for xcodebuild.

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Flag names, shared by parsing, stripping (they must never reach the soroqctl delegate) and help.
const (
	iosIPAFlag                 = "ipa"
	iosSigningIdentityFlag     = "signing-identity"
	iosProvisioningProfileFlag = "provisioning-profile"
	iosExtensionProfileFlag    = "extension-profile"
	iosSigningKeychainFlag     = "keychain"
)

var iosIPAValueFlags = []string{iosIPAFlag, iosSigningIdentityFlag, iosProvisioningProfileFlag, iosExtensionProfileFlag, iosSigningKeychainFlag}

// iosSigningExecFn runs a signing tool (security, plutil, codesign) and returns its stdout. A seam so
// tests exercise the whole signing flow without a keychain. stderr is folded into the error only.
var iosSigningExecFn = func(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// iosSigningNowFn is the clock used for profile expiry; a seam for tests.
var iosSigningNowFn = time.Now

// iosIPARequest is the parsed opt-in signing request. nil means "no IPA requested".
type iosIPARequest struct {
	OutPath           string
	Identity          string
	AppProfile        string
	ExtensionProfiles map[string]string // extension CFBundleIdentifier -> profile path
	Keychain          string
}

// flagValues returns every value of a repeatable `--name v` / `--name=v` flag.
func flagValues(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--"+name || a == "-"+name {
			if i+1 < len(args) {
				out = append(out, strings.TrimSpace(args[i+1]))
				i++
			} else {
				out = append(out, "")
			}
			continue
		}
		for _, pfx := range []string{"--" + name + "=", "-" + name + "="} {
			if strings.HasPrefix(a, pfx) {
				out = append(out, strings.TrimSpace(strings.TrimPrefix(a, pfx)))
			}
		}
	}
	return out
}

// hasIOSIPAFlags reports whether an IPA was requested or an IPA-only flag is present. --signing-identity
// alone is NOT counted: without --ipa it keeps its pre-existing meaning on the soroqctl delegate
// (`--archive-out` handoff descriptor) and passes through untouched.
func hasIOSIPAFlags(args []string) bool {
	for _, f := range iosIPAValueFlags {
		if f != iosSigningIdentityFlag && hasFlag(args, f) {
			return true
		}
	}
	return false
}

// stripIOSIPAFlags removes every signing flag (and value) so the delegate never sees them. Without --ipa
// nothing is stripped, so the default command line reaches the delegate byte-for-byte.
func stripIOSIPAFlags(args []string) []string {
	if !hasFlag(args, iosIPAFlag) {
		return args
	}
	for _, f := range iosIPAValueFlags {
		args = stripFlag(args, f, false)
	}
	return args
}

// parseIOSIPARequest extracts the signing request from soroq-side args (never the Flutter passthrough).
// Returns (nil, nil) when no signing flag is present, so the default behaviour is untouched.
func parseIOSIPARequest(args []string) (*iosIPARequest, error) {
	if !hasIOSIPAFlags(args) {
		return nil, nil
	}
	single := func(name string) (string, error) {
		vals := flagValues(args, name)
		if len(vals) > 1 {
			return "", fmt.Errorf("--%s was given %d times; pass it once", name, len(vals))
		}
		if len(vals) == 1 && vals[0] == "" {
			return "", fmt.Errorf("--%s requires a value", name)
		}
		if len(vals) == 0 {
			return "", nil
		}
		return vals[0], nil
	}
	req := &iosIPARequest{ExtensionProfiles: map[string]string{}}
	var err error
	if req.OutPath, err = single(iosIPAFlag); err != nil {
		return nil, err
	}
	if req.Identity, err = single(iosSigningIdentityFlag); err != nil {
		return nil, err
	}
	if req.AppProfile, err = single(iosProvisioningProfileFlag); err != nil {
		return nil, err
	}
	if req.Keychain, err = single(iosSigningKeychainFlag); err != nil {
		return nil, err
	}
	if req.OutPath == "" {
		return nil, errors.New("--provisioning-profile/--extension-profile/--keychain only apply with --ipa <out.ipa>; pass --ipa to produce a signed IPA")
	}
	if !strings.HasSuffix(strings.ToLower(req.OutPath), ".ipa") {
		return nil, fmt.Errorf("--ipa %q must name an output file ending in .ipa", req.OutPath)
	}
	if req.Identity == "" {
		return nil, errors.New("--ipa requires --signing-identity <SHA1|certificate name> (list them with `security find-identity -v -p codesigning`)")
	}
	if req.AppProfile == "" {
		return nil, errors.New("--ipa requires --provisioning-profile <path/to/app.mobileprovision> for the app's bundle id")
	}
	for _, v := range flagValues(args, iosExtensionProfileFlag) {
		bundleID, path, ok := strings.Cut(v, "=")
		bundleID, path = strings.TrimSpace(bundleID), strings.TrimSpace(path)
		if !ok || bundleID == "" || path == "" {
			return nil, fmt.Errorf("--extension-profile %q must be <extension-bundle-id>=<path/to/profile.mobileprovision>", v)
		}
		if _, dup := req.ExtensionProfiles[bundleID]; dup {
			return nil, fmt.Errorf("--extension-profile given twice for %s", bundleID)
		}
		req.ExtensionProfiles[bundleID] = path
	}
	if req.OutPath, err = filepath.Abs(req.OutPath); err != nil {
		return nil, err
	}
	return req, nil
}

// ---------------------------------------------------------------------------------------------------
// Identity + profile inspection

type iosSigningIdentity struct {
	SHA1 string
	Name string
}

var findIdentityLineRe = regexp.MustCompile(`^\s*\d+\)\s+([0-9A-Fa-f]{40})\s+"(.*)"\s*$`)

// resolveIOSSigningIdentity finds a VALID codesigning identity by SHA1 or exact certificate name.
func resolveIOSSigningIdentity(identity, keychain string) (iosSigningIdentity, error) {
	args := []string{"find-identity", "-v", "-p", "codesigning"}
	where := "the default keychain search list"
	if keychain != "" {
		args = append(args, keychain)
		where = "keychain " + keychain
	}
	out, err := iosSigningExecFn("security", args...)
	if err != nil {
		return iosSigningIdentity{}, fmt.Errorf("list codesigning identities: %w", err)
	}
	want := strings.TrimSpace(identity)
	var matches []iosSigningIdentity
	seen := map[string]bool{}
	total := 0
	for _, line := range strings.Split(string(out), "\n") {
		m := findIdentityLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		total++
		id := iosSigningIdentity{SHA1: strings.ToUpper(m[1]), Name: m[2]}
		if (strings.EqualFold(id.SHA1, want) || id.Name == want) && !seen[id.SHA1] {
			seen[id.SHA1] = true
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return iosSigningIdentity{}, fmt.Errorf("signing identity %q not found among %d valid codesigning identities in %s (check `security find-identity -v -p codesigning`; expired or revoked certificates are not listed)", identity, total, where)
	case 1:
		return matches[0], nil
	default:
		return iosSigningIdentity{}, fmt.Errorf("signing identity %q is ambiguous (%d certificates share that name); pass its SHA-1 instead", identity, len(matches))
	}
}

type iosProvisioningProfile struct {
	Path          string
	Name          string
	UUID          string
	TeamID        string
	AppIDPrefix   string
	BundlePattern string // application-identifier without the prefix; may be "*" or "com.x.*"
	Expiration    time.Time
	CertSHA1s     []string
	Entitlements  map[string]any
	Kind          string // app-store | ad-hoc | development | enterprise
}

func loadIOSProvisioningProfile(path string) (*iosProvisioningProfile, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("provisioning profile %s: %w", path, err)
	}
	raw, err := iosSigningExecFn("security", "cms", "-D", "-i", path)
	if err != nil {
		return nil, fmt.Errorf("decode provisioning profile %s: %w", path, err)
	}
	v, err := parseXMLPlist(raw)
	if err != nil {
		return nil, fmt.Errorf("parse provisioning profile %s: %w", path, err)
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("provisioning profile %s is not a dictionary", path)
	}
	p := &iosProvisioningProfile{Path: path}
	p.Name, _ = root["Name"].(string)
	p.UUID, _ = root["UUID"].(string)
	if teams, _ := root["TeamIdentifier"].([]any); len(teams) > 0 {
		p.TeamID, _ = teams[0].(string)
	}
	if prefixes, _ := root["ApplicationIdentifierPrefix"].([]any); len(prefixes) > 0 {
		p.AppIDPrefix, _ = prefixes[0].(string)
	}
	exp, ok := root["ExpirationDate"].(time.Time)
	if !ok {
		return nil, fmt.Errorf("provisioning profile %s has no ExpirationDate", path)
	}
	p.Expiration = exp
	p.Entitlements, _ = root["Entitlements"].(map[string]any)
	if p.Entitlements == nil {
		return nil, fmt.Errorf("provisioning profile %s has no Entitlements", path)
	}
	appID, _ := p.Entitlements["application-identifier"].(string)
	if p.AppIDPrefix == "" {
		p.AppIDPrefix, _, _ = strings.Cut(appID, ".")
	}
	if p.TeamID == "" || appID == "" || !strings.HasPrefix(appID, p.AppIDPrefix+".") {
		return nil, fmt.Errorf("provisioning profile %s has no usable team/application-identifier", path)
	}
	p.BundlePattern = strings.TrimPrefix(appID, p.AppIDPrefix+".")
	certs, _ := root["DeveloperCertificates"].([]any)
	for _, c := range certs {
		if der, ok := c.([]byte); ok {
			sum := sha1.Sum(der)
			p.CertSHA1s = append(p.CertSHA1s, strings.ToUpper(hex.EncodeToString(sum[:])))
		}
	}
	getTaskAllow, _ := p.Entitlements["get-task-allow"].(bool)
	_, hasDevices := root["ProvisionedDevices"]
	switch all, _ := root["ProvisionsAllDevices"].(bool); {
	case all:
		p.Kind = "enterprise"
	case hasDevices && getTaskAllow:
		p.Kind = "development"
	case hasDevices:
		p.Kind = "ad-hoc"
	default:
		p.Kind = "app-store"
	}
	return p, nil
}

func (p *iosProvisioningProfile) label() string {
	name := p.Name
	if name == "" {
		name = filepath.Base(p.Path)
	}
	return fmt.Sprintf("%q (%s)", name, p.Path)
}

// checkUsable fails on an expired profile or one that does not embed the signing certificate.
func (p *iosProvisioningProfile) checkUsable(id iosSigningIdentity, now time.Time) error {
	if !p.Expiration.After(now) {
		return fmt.Errorf("provisioning profile %s expired on %s; download a current one from the Apple Developer portal", p.label(), p.Expiration.UTC().Format(time.RFC3339))
	}
	for _, s := range p.CertSHA1s {
		if s == id.SHA1 {
			return nil
		}
	}
	return fmt.Errorf("provisioning profile %s (team %s) does not include the signing certificate %s %q; use an identity from that team/profile or regenerate the profile with this certificate", p.label(), p.TeamID, id.SHA1, id.Name)
}

// matchesBundleID reports whether the profile's application-identifier covers bundleID.
func (p *iosProvisioningProfile) matchesBundleID(bundleID string) bool {
	if p.BundlePattern == bundleID || p.BundlePattern == "*" {
		return true
	}
	if strings.HasSuffix(p.BundlePattern, ".*") {
		return strings.HasPrefix(bundleID, strings.TrimSuffix(p.BundlePattern, "*"))
	}
	return false
}

// entitlementsFor returns the profile entitlements with wildcards resolved to the concrete bundle id:
// a wildcard application-identifier (TEAM.*) is not a valid signing entitlement.
func (p *iosProvisioningProfile) entitlementsFor(bundleID string) map[string]any {
	out := make(map[string]any, len(p.Entitlements))
	concrete := p.AppIDPrefix + "." + bundleID
	for k, v := range p.Entitlements {
		out[k] = v
	}
	out["application-identifier"] = concrete
	if groups, ok := out["keychain-access-groups"].([]any); ok {
		resolved := make([]any, len(groups))
		for i, g := range groups {
			if s, ok := g.(string); ok && strings.HasSuffix(s, ".*") && strings.HasPrefix(s, p.AppIDPrefix+".") {
				resolved[i] = concrete
			} else {
				resolved[i] = g
			}
		}
		out["keychain-access-groups"] = resolved
	}
	return out
}

// iosSigningPlan is everything that can be checked BEFORE the (long) build: the identity exists and
// every profile is current, self-consistent and contains the identity's certificate.
type iosSigningPlan struct {
	Request    *iosIPARequest
	Identity   iosSigningIdentity
	AppProfile *iosProvisioningProfile
	Extensions map[string]*iosProvisioningProfile
}

func prepareIOSSigningPlan(req *iosIPARequest) (*iosSigningPlan, error) {
	if req == nil {
		return nil, nil
	}
	if req.Keychain != "" {
		if _, err := os.Stat(req.Keychain); err != nil {
			return nil, fmt.Errorf("--keychain %s: %w", req.Keychain, err)
		}
	}
	if info, err := os.Stat(filepath.Dir(req.OutPath)); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("--ipa %s: output directory %s does not exist", req.OutPath, filepath.Dir(req.OutPath))
	}
	id, err := resolveIOSSigningIdentity(req.Identity, req.Keychain)
	if err != nil {
		return nil, err
	}
	now := iosSigningNowFn()
	appProfile, err := loadIOSProvisioningProfile(req.AppProfile)
	if err != nil {
		return nil, err
	}
	if err := appProfile.checkUsable(id, now); err != nil {
		return nil, err
	}
	plan := &iosSigningPlan{Request: req, Identity: id, AppProfile: appProfile, Extensions: map[string]*iosProvisioningProfile{}}
	for bundleID, path := range req.ExtensionProfiles {
		p, err := loadIOSProvisioningProfile(path)
		if err != nil {
			return nil, err
		}
		if err := p.checkUsable(id, now); err != nil {
			return nil, err
		}
		if p.TeamID != appProfile.TeamID {
			return nil, fmt.Errorf("extension profile %s is for team %s but the app profile %s is for team %s; every bundle in one IPA must be signed by the same team", p.label(), p.TeamID, appProfile.label(), appProfile.TeamID)
		}
		if !p.matchesBundleID(bundleID) {
			return nil, fmt.Errorf("extension profile %s is for %s.%s, not %s", p.label(), p.AppIDPrefix, p.BundlePattern, bundleID)
		}
		plan.Extensions[bundleID] = p
	}
	return plan, nil
}

// ---------------------------------------------------------------------------------------------------
// Signing + packaging

type iosSignedIPAResult struct {
	IPAPath     string
	BundleID    string
	TeamID      string
	ProfileKind string
	Identity    iosSigningIdentity
	Extensions  []string
}

type iosBundleInfo struct {
	Path       string
	BundleID   string
	Executable string
}

func readIOSBundleInfo(bundle string) (iosBundleInfo, error) {
	plistPath := filepath.Join(bundle, "Info.plist")
	raw, err := iosSigningExecFn("plutil", "-convert", "xml1", "-o", "-", plistPath)
	if err != nil {
		return iosBundleInfo{}, fmt.Errorf("read %s: %w", plistPath, err)
	}
	v, err := parseXMLPlist(raw)
	if err != nil {
		return iosBundleInfo{}, fmt.Errorf("parse %s: %w", plistPath, err)
	}
	m, _ := v.(map[string]any)
	info := iosBundleInfo{Path: bundle}
	info.BundleID, _ = m["CFBundleIdentifier"].(string)
	info.Executable, _ = m["CFBundleExecutable"].(string)
	if info.BundleID == "" {
		return info, fmt.Errorf("%s has no CFBundleIdentifier", plistPath)
	}
	return info, nil
}

// buildSignedIOSIPA copies appPath into a Payload/, embeds profiles, signs inside-out, verifies, re-asserts
// the SOROQ engine and the base identity, and zips to outPath. appPath itself is never modified.
// requireIdentityAsset is true on the freehand lane, whose release is keyed to soroq_base_identity.json.
func buildSignedIOSIPA(appPath string, plan *iosSigningPlan, outPath string, requireIdentityAsset bool) (*iosSignedIPAResult, error) {
	// The input must already be the SOROQ app — refuse before spending any signing work on it.
	if err := assertSoroqIOSFrameworkInApp(appPath); err != nil {
		return nil, fmt.Errorf("refusing to sign: %w", err)
	}
	identityPath := filepath.Join(appPath, freehandBaseIdentityAssetName)
	identitySHA := ""
	if sha, _, err := sha256OfFile(identityPath); err == nil {
		identitySHA = sha
	} else if requireIdentityAsset {
		return nil, fmt.Errorf("refusing to sign: %s has no %s — the IPA would not be the app this release registers", appPath, freehandBaseIdentityAssetName)
	}
	for _, unsupported := range []string{"Watch", "AppClips"} {
		if _, err := os.Stat(filepath.Join(appPath, unsupported)); err == nil {
			return nil, fmt.Errorf("refusing to sign: %s contains %s/, which --ipa does not support; export this app with Xcode", appPath, unsupported)
		}
	}
	if swift, _ := filepath.Glob(filepath.Join(appPath, "Frameworks", "libswift*.dylib")); len(swift) > 0 {
		return nil, fmt.Errorf("refusing to sign: %s embeds Swift runtime dylibs (%s), which require a SwiftSupport/ folder --ipa does not build; raise the iOS deployment target to 12.2+ (Flutter requires 13) or export with Xcode", appPath, filepath.Base(swift[0]))
	}

	work, err := os.MkdirTemp(filepath.Dir(outPath), ".soroq-ipa-work-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	payloadApp := filepath.Join(work, "Payload", filepath.Base(appPath))
	if err := copyIOSBundleTree(appPath, payloadApp); err != nil {
		return nil, fmt.Errorf("stage %s: %w", appPath, err)
	}

	app, err := readIOSBundleInfo(payloadApp)
	if err != nil {
		return nil, err
	}
	if !plan.AppProfile.matchesBundleID(app.BundleID) {
		return nil, fmt.Errorf("provisioning profile %s is for %s.%s, but the app's CFBundleIdentifier is %s", plan.AppProfile.label(), plan.AppProfile.AppIDPrefix, plan.AppProfile.BundlePattern, app.BundleID)
	}
	appexes, _ := filepath.Glob(filepath.Join(payloadApp, "PlugIns", "*.appex"))
	sort.Strings(appexes)
	var extensions []iosBundleInfo
	used := map[string]bool{}
	for _, appex := range appexes {
		ext, err := readIOSBundleInfo(appex)
		if err != nil {
			return nil, err
		}
		if _, ok := plan.Extensions[ext.BundleID]; !ok {
			return nil, fmt.Errorf("app extension %s (%s) has no provisioning profile; pass --extension-profile %s=<path>", filepath.Base(appex), ext.BundleID, ext.BundleID)
		}
		used[ext.BundleID] = true
		extensions = append(extensions, ext)
	}
	for bundleID := range plan.Extensions {
		if !used[bundleID] {
			return nil, fmt.Errorf("--extension-profile %s: the app has no extension with that bundle id", bundleID)
		}
	}

	entDir := filepath.Join(work, "entitlements")
	if err := os.MkdirAll(entDir, 0o755); err != nil {
		return nil, err
	}
	writeEntitlements := func(bundleID string, p *iosProvisioningProfile) (string, error) {
		path := filepath.Join(entDir, bundleID+".plist")
		return path, os.WriteFile(path, encodeXMLPlist(p.entitlementsFor(bundleID)), 0o644)
	}
	embedProfile := func(bundle string, p *iosProvisioningProfile) error {
		return copyFileContents(p.Path, filepath.Join(bundle, "embedded.mobileprovision"))
	}
	codesign := func(path, entitlements string) error {
		args := []string{"-f", "-s", plan.Identity.SHA1, "--timestamp"}
		if plan.Request.Keychain != "" {
			args = append(args, "--keychain", plan.Request.Keychain)
		}
		if entitlements != "" {
			args = append(args, "--entitlements", entitlements)
		}
		args = append(args, path)
		if _, err := iosSigningExecFn("codesign", args...); err != nil {
			return fmt.Errorf("codesign %s: %w", path, err)
		}
		return nil
	}
	signFrameworks := func(bundle string) error {
		entries, _ := filepath.Glob(filepath.Join(bundle, "Frameworks", "*"))
		sort.Strings(entries)
		for _, e := range entries {
			if strings.HasSuffix(e, ".framework") || strings.HasSuffix(e, ".dylib") {
				if err := codesign(e, ""); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Inside-out: nested frameworks, then each extension, then the app's frameworks, then the app.
	var extIDs []string
	for _, ext := range extensions {
		p := plan.Extensions[ext.BundleID]
		if err := embedProfile(ext.Path, p); err != nil {
			return nil, err
		}
		ent, err := writeEntitlements(ext.BundleID, p)
		if err != nil {
			return nil, err
		}
		if err := signFrameworks(ext.Path); err != nil {
			return nil, err
		}
		if err := codesign(ext.Path, ent); err != nil {
			return nil, err
		}
		extIDs = append(extIDs, ext.BundleID)
	}
	if err := embedProfile(payloadApp, plan.AppProfile); err != nil {
		return nil, err
	}
	appEnt, err := writeEntitlements(app.BundleID, plan.AppProfile)
	if err != nil {
		return nil, err
	}
	if err := signFrameworks(payloadApp); err != nil {
		return nil, err
	}
	if err := codesign(payloadApp, appEnt); err != nil {
		return nil, err
	}
	if _, err := iosSigningExecFn("codesign", "-v", "--deep", "--strict", payloadApp); err != nil {
		return nil, fmt.Errorf("signed app failed verification: %w", err)
	}

	// The IPA must be the same app the release registers: the SOROQ engine and the base identity survive.
	if err := assertSignedSoroqFramework(appPath, payloadApp, work); err != nil {
		return nil, fmt.Errorf("signed app failed the SOROQ engine re-assertion: %w", err)
	}
	if identitySHA != "" {
		got, _, err := sha256OfFile(filepath.Join(payloadApp, freehandBaseIdentityAssetName))
		if err != nil || got != identitySHA {
			return nil, fmt.Errorf("signed app's %s is missing or changed during signing", freehandBaseIdentityAssetName)
		}
	}

	if err := zipIOSPayload(work, outPath); err != nil {
		return nil, fmt.Errorf("package %s: %w", outPath, err)
	}
	return &iosSignedIPAResult{
		IPAPath:     outPath,
		BundleID:    app.BundleID,
		TeamID:      plan.AppProfile.TeamID,
		ProfileKind: plan.AppProfile.Kind,
		Identity:    plan.Identity,
		Extensions:  extIDs,
	}, nil
}

// assertSignedSoroqFramework re-runs the SOROQ engine assertion on the signed Flutter binary. Signing
// rewrites the Mach-O code signature, so a hash-pinned engine without the interpreter symbol no longer
// matches by hash; for that case the signature-stripped bytes must equal the (already asserted) input's.
func assertSignedSoroqFramework(srcApp, signedApp, work string) error {
	rel := filepath.Join("Frameworks", "Flutter.framework", "Flutter")
	signed := filepath.Join(signedApp, rel)
	if err := assertSoroqIOSFrameworkBinary(signed); err == nil {
		return nil
	} else if _, statErr := os.Stat(signed); statErr != nil {
		return err
	}
	stripped := func(src, name string) (string, error) {
		dst := filepath.Join(work, name)
		if err := copyFileContents(src, dst); err != nil {
			return "", err
		}
		// An unsigned input has nothing to strip; codesign reports that and the bytes are already bare.
		_, _ = iosSigningExecFn("codesign", "--remove-signature", dst)
		sha, _, err := sha256OfFile(dst)
		return sha, err
	}
	a, err := stripped(filepath.Join(srcApp, rel), "flutter-src.bin")
	if err != nil {
		return err
	}
	b, err := stripped(signed, "flutter-signed.bin")
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("Flutter.framework in the signed app differs from the SOROQ engine that was built (%s != %s after removing signatures)", b, a)
	}
	return nil
}

// stageSignedIOSIPA signs the built app into a temp file next to the requested output. The caller
// renames it into place only after release registration succeeds, so a failed registration never
// leaves an IPA that looks like a registered release.
func stageSignedIOSIPA(projectDir string, plan *iosSigningPlan, requireIdentityAsset bool) (string, *iosSignedIPAResult, error) {
	appPath, err := locateBuiltIOSApp(projectDir)
	if err != nil {
		return "", nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(plan.Request.OutPath), ".soroq-ipa-*.partial")
	if err != nil {
		return "", nil, err
	}
	tmp.Close()
	fmt.Fprintf(os.Stderr, "soroq: signing %s with %q (%s), profile %s [%s, team %s, expires %s]\n",
		appPath, plan.Identity.Name, plan.Identity.SHA1, plan.AppProfile.label(), plan.AppProfile.Kind,
		plan.AppProfile.TeamID, plan.AppProfile.Expiration.UTC().Format("2006-01-02"))
	res, err := buildSignedIOSIPA(appPath, plan, tmp.Name(), requireIdentityAsset)
	if err != nil {
		os.Remove(tmp.Name())
		return "", nil, err
	}
	res.IPAPath = plan.Request.OutPath
	return tmp.Name(), res, nil
}

// registerIOSEngineBaseline is the single registration point of the engine lane. Without signing flags
// it is exactly delegate(verb, args). With them it signs BEFORE registering (so a signing failure
// registers nothing) and publishes the IPA only AFTER registration succeeds.
func registerIOSEngineBaseline(projectDir string, delegate func(string, []string) error, verb string, args []string, requireIdentityAsset bool) error {
	req, err := parseIOSIPARequest(args)
	if err != nil {
		return err
	}
	args = stripIOSIPAFlags(args)
	if req == nil {
		return delegate(verb, args)
	}
	plan, err := prepareIOSSigningPlan(req)
	if err != nil {
		return err
	}
	staged, res, err := stageSignedIOSIPA(projectDir, plan, requireIdentityAsset)
	if err != nil {
		return fmt.Errorf("signed IPA not produced; no release registered: %w", err)
	}
	if err := delegate(verb, args); err != nil {
		os.Remove(staged)
		return err
	}
	if err := os.Rename(staged, req.OutPath); err != nil {
		return fmt.Errorf("release registered, but moving the signed IPA into place failed (%s left at %s): %w", req.OutPath, staged, err)
	}
	fmt.Fprintf(os.Stdout, "signed_ipa: %s\n", res.IPAPath)
	fmt.Fprintf(os.Stdout, "signed_bundle_id: %s (team %s, %s profile)\n", res.BundleID, res.TeamID, res.ProfileKind)
	if len(res.Extensions) > 0 {
		fmt.Fprintf(os.Stdout, "signed_extensions: %s\n", strings.Join(res.Extensions, ", "))
	}
	if res.ProfileKind == "app-store" {
		fmt.Fprintf(os.Stdout, "next: validate/upload with `xcrun altool --validate-app -f %s -t ios` then Transporter or `xcrun altool --upload-app`.\n", res.IPAPath)
	}
	return nil
}

// copyIOSBundleTree copies a bundle preserving symlinks (as links) and file modes.
func copyIOSBundleTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		default:
			return copyFileContents(path, target)
		}
	})
}

// zipIOSPayload writes every top-level IPA member under root (Payload/, and SwiftSupport/ if present)
// to outPath, storing symlinks as links and preserving executable bits (the equivalent of `zip -qry`).
func zipIOSPayload(root, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	walkErr := func() error {
		for _, top := range []string{"Payload", "SwiftSupport"} {
			base := filepath.Join(root, top)
			if _, err := os.Stat(base); err != nil {
				continue
			}
			err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := os.Lstat(path)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				hdr, err := zip.FileInfoHeader(info)
				if err != nil {
					return err
				}
				hdr.Name = filepath.ToSlash(rel)
				switch {
				case info.IsDir():
					hdr.Name += "/"
					hdr.Method = zip.Store
					_, err = zw.CreateHeader(hdr)
					return err
				case info.Mode()&os.ModeSymlink != 0:
					link, err := os.Readlink(path)
					if err != nil {
						return err
					}
					hdr.Method = zip.Store
					w, err := zw.CreateHeader(hdr)
					if err != nil {
						return err
					}
					_, err = io.WriteString(w, link)
					return err
				default:
					hdr.Method = zip.Deflate
					w, err := zw.CreateHeader(hdr)
					if err != nil {
						return err
					}
					in, err := os.Open(path)
					if err != nil {
						return err
					}
					defer in.Close()
					_, err = io.Copy(w, in)
					return err
				}
			})
			if err != nil {
				return err
			}
		}
		return nil
	}()
	closeErr := zw.Close()
	fileErr := f.Close()
	return errors.Join(walkErr, closeErr, fileErr)
}

// ---------------------------------------------------------------------------------------------------
// Minimal XML property-list codec (profiles via `security cms -D`, Info.plist via `plutil -convert xml1`).

func parseXMLPlist(data []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("no plist root: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "plist" {
			for {
				tok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if se, ok := tok.(xml.StartElement); ok {
					return decodePlistValue(dec, se)
				}
			}
		}
	}
}

func decodePlistValue(dec *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		key := ""
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					var k string
					if err := dec.DecodeElement(&k, &t); err != nil {
						return nil, err
					}
					key = k
					continue
				}
				v, err := decodePlistValue(dec, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
			case xml.EndElement:
				return m, nil
			}
		}
	case "array":
		var arr []any
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := decodePlistValue(dec, t)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			case xml.EndElement:
				if arr == nil {
					arr = []any{}
				}
				return arr, nil
			}
		}
	case "true", "false":
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	var text string
	if err := dec.DecodeElement(&text, &se); err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "string":
		return text, nil
	case "integer":
		return strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	case "real":
		return strconv.ParseFloat(strings.TrimSpace(text), 64)
	case "date":
		return time.Parse(time.RFC3339, strings.TrimSpace(text))
	case "data":
		return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
	}
	return nil, fmt.Errorf("unsupported plist element <%s>", se.Name.Local)
}

func encodeXMLPlist(v any) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	encodePlistValue(&b, v, 0)
	b.WriteString("</plist>\n")
	return b.Bytes()
}

func encodePlistValue(b *bytes.Buffer, v any, depth int) {
	indent := strings.Repeat("\t", depth)
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(indent + "<dict>\n")
		for _, k := range keys {
			b.WriteString(indent + "\t<key>" + esc(k) + "</key>\n")
			encodePlistValue(b, t[k], depth+1)
		}
		b.WriteString(indent + "</dict>\n")
	case []any:
		b.WriteString(indent + "<array>\n")
		for _, e := range t {
			encodePlistValue(b, e, depth+1)
		}
		b.WriteString(indent + "</array>\n")
	case string:
		b.WriteString(indent + "<string>" + esc(t) + "</string>\n")
	case bool:
		if t {
			b.WriteString(indent + "<true/>\n")
		} else {
			b.WriteString(indent + "<false/>\n")
		}
	case int64:
		b.WriteString(indent + "<integer>" + strconv.FormatInt(t, 10) + "</integer>\n")
	case float64:
		b.WriteString(indent + "<real>" + strconv.FormatFloat(t, 'g', -1, 64) + "</real>\n")
	case time.Time:
		b.WriteString(indent + "<date>" + t.UTC().Format(time.RFC3339) + "</date>\n")
	case []byte:
		b.WriteString(indent + "<data>" + base64.StdEncoding.EncodeToString(t) + "</data>\n")
	}
}
