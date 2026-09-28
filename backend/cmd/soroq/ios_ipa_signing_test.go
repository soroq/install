package main

import (
	"archive/zip"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The signing flow is exercised end to end against a stubbed exec: `security cms -D` and
// `plutil -convert xml1` return the fixture file itself (fixtures are written as XML plists), and every
// codesign invocation is recorded instead of run. No keychain, certificate or Xcode is needed.

var (
	ipaTestCertDER   = []byte("fake-apple-distribution-certificate-der")
	ipaTestOtherCert = []byte("some-other-teams-certificate-der")
	ipaTestNow       = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
)

// writeIPAFile writes a fixture file, creating its parent directories (bundles are deep trees).
func writeIPAFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	writeFile(t, path, contents)
}

func ipaTestSHA1(der []byte) string {
	s := sha1.Sum(der)
	return strings.ToUpper(hex.EncodeToString(s[:]))
}

type fakeSigningExec struct {
	identities string // `security find-identity` stdout
	calls      [][]string
	codesignFn func(args []string) error // optional per-call behaviour
}

func (f *fakeSigningExec) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	switch {
	case name == "security" && len(args) > 0 && args[0] == "find-identity":
		return []byte(f.identities), nil
	case name == "security" && len(args) == 4 && args[0] == "cms":
		return os.ReadFile(args[3])
	case name == "plutil":
		return os.ReadFile(args[len(args)-1])
	case name == "codesign":
		if f.codesignFn != nil {
			return nil, f.codesignFn(args)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %s %v", name, args)
}

func (f *fakeSigningExec) codesignCalls() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[0] == "codesign" {
			out = append(out, c[1:])
		}
	}
	return out
}

func installFakeSigningExec(t *testing.T) *fakeSigningExec {
	t.Helper()
	f := &fakeSigningExec{identities: fmt.Sprintf(
		"  1) %s \"Apple Distribution: Example Co (TEAM123456)\"\n  2) %s \"Apple Development: Someone (OTHER99999)\"\n     2 valid identities found\n",
		ipaTestSHA1(ipaTestCertDER), ipaTestSHA1(ipaTestOtherCert))}
	prevExec, prevNow := iosSigningExecFn, iosSigningNowFn
	iosSigningExecFn = f.run
	iosSigningNowFn = func() time.Time { return ipaTestNow }
	t.Cleanup(func() { iosSigningExecFn, iosSigningNowFn = prevExec, prevNow })
	return f
}

type ipaTestProfile struct {
	team, appID string
	expires     time.Time
	certs       [][]byte
	devices     bool
	extraEnt    string
}

func writeIPATestProfile(t *testing.T, dir, name string, p ipaTestProfile) string {
	t.Helper()
	if p.team == "" {
		p.team = "TEAM123456"
	}
	if p.expires.IsZero() {
		p.expires = ipaTestNow.AddDate(1, 0, 0)
	}
	if p.certs == nil {
		p.certs = [][]byte{ipaTestCertDER}
	}
	var certs strings.Builder
	for _, c := range p.certs {
		certs.WriteString("<data>" + base64.StdEncoding.EncodeToString(c) + "</data>")
	}
	devices := ""
	if p.devices {
		devices = "<key>ProvisionedDevices</key><array><string>00008110-000000000000001E</string></array>"
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Name</key><string>%s</string>
<key>UUID</key><string>11111111-2222-3333-4444-555555555555</string>
<key>ApplicationIdentifierPrefix</key><array><string>%s</string></array>
<key>TeamIdentifier</key><array><string>%s</string></array>
<key>ExpirationDate</key><date>%s</date>
<key>DeveloperCertificates</key><array>%s</array>
%s
<key>Entitlements</key><dict>
  <key>application-identifier</key><string>%s.%s</string>
  <key>com.apple.developer.team-identifier</key><string>%s</string>
  <key>get-task-allow</key><false/>
  <key>keychain-access-groups</key><array><string>%s.*</string><string>com.apple.token</string></array>
  %s
</dict>
</dict></plist>
`, name, p.team, p.team, p.expires.UTC().Format(time.RFC3339), certs.String(), devices, p.team, p.appID, p.team, p.team, p.extraEnt)
	path := filepath.Join(dir, name+".mobileprovision")
	writeIPAFile(t, path, body)
	return path
}

func ipaTestInfoPlist(bundleID, exe string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>%s</string><key>CFBundleExecutable</key><string>%s</string></dict></plist>
`, bundleID, exe)
}

// writeIPATestApp builds a Runner.app with the SOROQ Flutter.framework, the base identity, two
// frameworks, and (optionally) one extension that has its own nested framework.
func writeIPATestApp(t *testing.T, projectDir string, withExtension bool) string {
	t.Helper()
	app := filepath.Join(projectDir, "build", "ios", "iphoneos", "Runner.app")
	writeIPAFile(t, filepath.Join(app, "Info.plist"), ipaTestInfoPlist("com.example.app", "Runner"))
	writeIPAFile(t, filepath.Join(app, "Runner"), "mach-o runner")
	writeIPAFile(t, filepath.Join(app, freehandBaseIdentityAssetName), `{"schema":"soroq.base_identity_asset.v1"}`+"\n")
	writeIPAFile(t, filepath.Join(app, "Frameworks", "Flutter.framework", "Flutter"), "engine bytes soroq_ios_interpreter_backend_v1 more")
	writeIPAFile(t, filepath.Join(app, "Frameworks", "App.framework", "App"), "aot snapshot")
	if withExtension {
		ext := filepath.Join(app, "PlugIns", "Share.appex")
		writeIPAFile(t, filepath.Join(ext, "Info.plist"), ipaTestInfoPlist("com.example.app.share", "Share"))
		writeIPAFile(t, filepath.Join(ext, "Share"), "mach-o share")
		writeIPAFile(t, filepath.Join(ext, "Frameworks", "Nested.framework", "Nested"), "nested")
	}
	return app
}

type ipaFixture struct {
	project, app, profiles, out string
	fake                        *fakeSigningExec
}

func newIPAFixture(t *testing.T, withExtension bool) *ipaFixture {
	t.Helper()
	f := &ipaFixture{project: t.TempDir(), profiles: t.TempDir()}
	f.out = filepath.Join(t.TempDir(), "Runner.ipa")
	f.fake = installFakeSigningExec(t)
	f.app = writeIPATestApp(t, f.project, withExtension)
	return f
}

func (f *ipaFixture) plan(t *testing.T, args ...string) (*iosSigningPlan, error) {
	t.Helper()
	req, err := parseIOSIPARequest(append([]string{"--ipa", f.out}, args...))
	if err != nil {
		return nil, err
	}
	return prepareIOSSigningPlan(req)
}

func zipNames(t *testing.T, path string) map[string]*zip.File {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open ipa: %v", err)
	}
	t.Cleanup(func() { zr.Close() })
	out := map[string]*zip.File{}
	for _, f := range zr.File {
		out[f.Name] = f
	}
	return out
}

func TestParseIOSIPARequest(t *testing.T) {
	if req, err := parseIOSIPARequest([]string{"--release-id", "r1"}); req != nil || err != nil {
		t.Fatalf("no signing flags must be a no-op, got %+v %v", req, err)
	}
	// --signing-identity alone keeps its soroqctl --archive-out meaning and is NOT stripped.
	passthrough := []string{"--archive-out", "h.json", "--signing-identity", "Apple Distribution: X"}
	if req, err := parseIOSIPARequest(passthrough); req != nil || err != nil {
		t.Fatalf("--signing-identity without --ipa must pass through, got %+v %v", req, err)
	}
	if got := stripIOSIPAFlags(passthrough); !reflect.DeepEqual(got, passthrough) {
		t.Fatalf("args without --ipa must reach the delegate unchanged, got %v", got)
	}

	args := []string{"--release-id", "r1", "--ipa", "out/App.ipa", "--signing-identity", "ABC",
		"--provisioning-profile=app.mobileprovision", "--extension-profile", "com.x.share=share.mobileprovision",
		"--extension-profile=com.x.widget=widget.mobileprovision", "--keychain", "ci.keychain-db", "--api", "https://a"}
	req, err := parseIOSIPARequest(args)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !filepath.IsAbs(req.OutPath) || filepath.Base(req.OutPath) != "App.ipa" || req.Identity != "ABC" ||
		req.AppProfile != "app.mobileprovision" || req.Keychain != "ci.keychain-db" ||
		req.ExtensionProfiles["com.x.share"] != "share.mobileprovision" || req.ExtensionProfiles["com.x.widget"] != "widget.mobileprovision" {
		t.Fatalf("unexpected request %+v", req)
	}
	if got, want := stripIOSIPAFlags(args), []string{"--release-id", "r1", "--api", "https://a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("strip = %v, want %v", got, want)
	}

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"profile without ipa":   {[]string{"--provisioning-profile", "p"}, "only apply with --ipa"},
		"keychain without ipa":  {[]string{"--keychain", "k"}, "only apply with --ipa"},
		"missing identity":      {[]string{"--ipa", "a.ipa", "--provisioning-profile", "p"}, "requires --signing-identity"},
		"missing profile":       {[]string{"--ipa", "a.ipa", "--signing-identity", "S"}, "requires --provisioning-profile"},
		"not an ipa":            {[]string{"--ipa", "a.zip", "--signing-identity", "S", "--provisioning-profile", "p"}, "ending in .ipa"},
		"bad extension profile": {[]string{"--ipa", "a.ipa", "--signing-identity", "S", "--provisioning-profile", "p", "--extension-profile", "nopath"}, "<extension-bundle-id>=<path"},
		"duplicate extension":   {[]string{"--ipa", "a.ipa", "--signing-identity", "S", "--provisioning-profile", "p", "--extension-profile", "a=b", "--extension-profile", "a=c"}, "given twice"},
		"ipa twice":             {[]string{"--ipa", "a.ipa", "--ipa", "b.ipa", "--signing-identity", "S", "--provisioning-profile", "p"}, "given 2 times"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseIOSIPARequest(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestBuildSignedIOSIPASignsInsideOutAndPackages(t *testing.T) {
	f := newIPAFixture(t, true)
	keychain := filepath.Join(t.TempDir(), "ci.keychain-db")
	writeIPAFile(t, keychain, "kc")
	appProfile := writeIPATestProfile(t, f.profiles, "app", ipaTestProfile{appID: "com.example.app"})
	// A wildcard extension profile: its application-identifier must be resolved to the concrete id.
	extProfile := writeIPATestProfile(t, f.profiles, "share", ipaTestProfile{appID: "com.example.*"})
	plan, err := f.plan(t, "--signing-identity", "Apple Distribution: Example Co (TEAM123456)",
		"--provisioning-profile", appProfile, "--extension-profile", "com.example.app.share="+extProfile, "--keychain", keychain)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Identity.SHA1 != ipaTestSHA1(ipaTestCertDER) || plan.AppProfile.Kind != "app-store" {
		t.Fatalf("identity/profile resolution wrong: %+v kind=%s", plan.Identity, plan.AppProfile.Kind)
	}

	// Capture the entitlements handed to codesign while the work dir still exists.
	entitlements := map[string]string{}
	f.fake.codesignFn = func(args []string) error {
		for i, a := range args {
			if a == "--entitlements" {
				b, _ := os.ReadFile(args[i+1])
				entitlements[filepath.Base(args[len(args)-1])] = string(b)
			}
		}
		return nil
	}
	res, err := buildSignedIOSIPA(f.app, plan, f.out, true)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if res.BundleID != "com.example.app" || res.TeamID != "TEAM123456" || !reflect.DeepEqual(res.Extensions, []string{"com.example.app.share"}) {
		t.Fatalf("unexpected result %+v", res)
	}

	sha := plan.Identity.SHA1
	var got []string
	for _, c := range f.fake.codesignCalls() {
		got = append(got, strings.Join(c, " "))
	}
	idx := func(sub string) int {
		for i, g := range got {
			if strings.HasSuffix(g, sub) {
				return i
			}
		}
		t.Fatalf("no codesign call ending in %q in %v", sub, got)
		return -1
	}
	nested, appex := idx("Share.appex/Frameworks/Nested.framework"), idx("PlugIns/Share.appex")
	appFw, flutterFw, app := idx("Runner.app/Frameworks/App.framework"), idx("Runner.app/Frameworks/Flutter.framework"), idx("Payload/Runner.app")
	verify := -1
	for i, g := range got {
		if strings.HasPrefix(g, "-v --deep --strict ") && strings.HasSuffix(g, "Payload/Runner.app") {
			verify = i
		}
	}
	if !(nested < appex && appex < appFw && appex < flutterFw && appFw < app && flutterFw < app && app < verify) {
		t.Fatalf("signing is not inside-out: %v", got)
	}
	for _, i := range []int{nested, appex, appFw, flutterFw, app} {
		if !strings.HasPrefix(got[i], "-f -s "+sha+" --timestamp --keychain "+keychain) {
			t.Fatalf("codesign call %q lacks identity/timestamp/keychain", got[i])
		}
	}
	for _, i := range []int{nested, appFw, flutterFw} {
		if strings.Contains(got[i], "--entitlements") {
			t.Fatalf("frameworks must be signed without entitlements: %q", got[i])
		}
	}
	if !strings.Contains(entitlements["Runner.app"], "<string>TEAM123456.com.example.app</string>") {
		t.Fatalf("app entitlements lack the concrete application-identifier:\n%s", entitlements["Runner.app"])
	}
	share := entitlements["Share.appex"]
	if !strings.Contains(share, "<string>TEAM123456.com.example.app.share</string>") || strings.Contains(share, "TEAM123456.*") {
		t.Fatalf("wildcard extension entitlements were not resolved:\n%s", share)
	}
	if !strings.Contains(share, "<string>com.apple.token</string>") {
		t.Fatalf("non-wildcard keychain groups must be kept:\n%s", share)
	}

	files := zipNames(t, f.out)
	for _, want := range []string{
		"Payload/", "Payload/Runner.app/Runner", "Payload/Runner.app/" + freehandBaseIdentityAssetName,
		"Payload/Runner.app/embedded.mobileprovision", "Payload/Runner.app/PlugIns/Share.appex/embedded.mobileprovision",
		"Payload/Runner.app/Frameworks/Flutter.framework/Flutter",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("IPA missing %s", want)
		}
	}
	for name := range files {
		if !strings.HasPrefix(name, "Payload/") {
			t.Errorf("unexpected top-level IPA member %s", name)
		}
	}
	// The built app is never modified; only the staged copy is signed.
	if _, err := os.Stat(filepath.Join(f.app, "embedded.mobileprovision")); err == nil {
		t.Fatal("source app was modified")
	}
	// No work directory left behind next to the output.
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(f.out), ".soroq-ipa-work-*")); len(left) != 0 {
		t.Fatalf("work dir leaked: %v", left)
	}
}

func TestIOSSigningPlanFailsEarly(t *testing.T) {
	for name, tc := range map[string]struct {
		identity string
		profile  ipaTestProfile
		ext      *ipaTestProfile
		want     string
	}{
		"identity missing":        {identity: "DEADBEEF", profile: ipaTestProfile{appID: "com.example.app"}, want: `signing identity "DEADBEEF" not found among 2`},
		"expired profile":         {profile: ipaTestProfile{appID: "com.example.app", expires: ipaTestNow.Add(-time.Hour)}, want: "expired on"},
		"certificate not in team": {profile: ipaTestProfile{appID: "com.example.app", certs: [][]byte{ipaTestOtherCert}}, want: "does not include the signing certificate"},
		"extension team mismatch": {profile: ipaTestProfile{appID: "com.example.app"}, ext: &ipaTestProfile{team: "OTHER99999", appID: "com.example.app.share"}, want: "every bundle in one IPA must be signed by the same team"},
		"extension id mismatch":   {profile: ipaTestProfile{appID: "com.example.app"}, ext: &ipaTestProfile{appID: "com.example.other"}, want: "not com.example.app.share"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newIPAFixture(t, true)
			identity := tc.identity
			if identity == "" {
				identity = ipaTestSHA1(ipaTestCertDER)
			}
			args := []string{"--signing-identity", identity, "--provisioning-profile", writeIPATestProfile(t, f.profiles, "app", tc.profile)}
			if tc.ext != nil {
				args = append(args, "--extension-profile", "com.example.app.share="+writeIPATestProfile(t, f.profiles, "share", *tc.ext))
			}
			_, err := f.plan(t, args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestBuildSignedIOSIPARefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		appID     string
		ext       bool
		extFlag   bool
		mutate    func(t *testing.T, f *ipaFixture)
		codesign  func(f *ipaFixture) func([]string) error
		requireID bool
		want      string
	}{
		"bundle id mismatch": {appID: "com.example.different", want: "but the app's CFBundleIdentifier is com.example.app"},
		"extension without profile": {appID: "com.example.app", ext: true,
			want: "has no provisioning profile; pass --extension-profile com.example.app.share=<path>"},
		"profile for unknown extension": {appID: "com.example.app", extFlag: true,
			want: "the app has no extension with that bundle id"},
		"stock engine": {appID: "com.example.app", mutate: func(t *testing.T, f *ipaFixture) {
			writeIPAFile(t, filepath.Join(f.app, "Frameworks", "Flutter.framework", "Flutter"), "stock engine")
		}, want: "refusing to sign"},
		"identity asset missing": {appID: "com.example.app", requireID: true, mutate: func(t *testing.T, f *ipaFixture) {
			os.Remove(filepath.Join(f.app, freehandBaseIdentityAssetName))
		}, want: "has no soroq_base_identity.json"},
		"swift runtime needs SwiftSupport": {appID: "com.example.app", mutate: func(t *testing.T, f *ipaFixture) {
			writeIPAFile(t, filepath.Join(f.app, "Frameworks", "libswiftCore.dylib"), "x")
		}, want: "SwiftSupport/"},
		"watch app": {appID: "com.example.app", mutate: func(t *testing.T, f *ipaFixture) {
			writeIPAFile(t, filepath.Join(f.app, "Watch", "W.app", "Info.plist"), "x")
		}, want: "contains Watch/"},
		"identity asset changed by signing": {appID: "com.example.app", requireID: true, codesign: func(f *ipaFixture) func([]string) error {
			return func(args []string) error {
				p := args[len(args)-1]
				if strings.HasSuffix(p, "Payload/Runner.app") && args[0] == "-f" {
					return os.WriteFile(filepath.Join(p, freehandBaseIdentityAssetName), []byte("tampered"), 0o644)
				}
				return nil
			}
		}, want: "missing or changed during signing"},
		"engine replaced by signing": {appID: "com.example.app", codesign: func(f *ipaFixture) func([]string) error {
			return func(args []string) error {
				p := args[len(args)-1]
				if strings.HasSuffix(p, "Flutter.framework") && args[0] == "-f" {
					return os.WriteFile(filepath.Join(p, "Flutter"), []byte("stock"), 0o755)
				}
				return nil
			}
		}, want: "SOROQ engine re-assertion"},
		"verification fails": {appID: "com.example.app", codesign: func(f *ipaFixture) func([]string) error {
			return func(args []string) error {
				if args[0] == "-v" {
					return errors.New("a sealed resource is missing or invalid")
				}
				return nil
			}
		}, want: "failed verification"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newIPAFixture(t, tc.ext)
			args := []string{"--signing-identity", ipaTestSHA1(ipaTestCertDER),
				"--provisioning-profile", writeIPATestProfile(t, f.profiles, "app", ipaTestProfile{appID: tc.appID})}
			if tc.extFlag {
				args = append(args, "--extension-profile", "com.example.app.share="+writeIPATestProfile(t, f.profiles, "share", ipaTestProfile{appID: "com.example.app.share"}))
			}
			plan, err := f.plan(t, args...)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			if tc.codesign != nil {
				f.fake.codesignFn = tc.codesign(f)
			}
			_, err = buildSignedIOSIPA(f.app, plan, f.out, tc.requireID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(f.out); statErr == nil {
				if b, _ := os.ReadFile(f.out); len(b) > 0 {
					t.Fatal("a refused signing must not write an IPA")
				}
			}
		})
	}
}

func TestRegisterIOSEngineBaselineStagesIPAUntilRegistration(t *testing.T) {
	f := newIPAFixture(t, false)
	profile := writeIPATestProfile(t, f.profiles, "app", ipaTestProfile{appID: "com.example.app"})
	base := []string{"--release-id", "r1", "--app-dill", "/x/app.dill"}
	signing := []string{"--ipa", f.out, "--signing-identity", ipaTestSHA1(ipaTestCertDER), "--provisioning-profile", profile}

	t.Run("no signing flags is exactly the delegate", func(t *testing.T) {
		var gotArgs []string
		err := registerIOSEngineBaseline(f.project, func(_ string, a []string) error { gotArgs = a; return nil }, "release", base, true)
		if err != nil || !reflect.DeepEqual(gotArgs, base) {
			t.Fatalf("err=%v args=%v", err, gotArgs)
		}
		if len(f.fake.codesignCalls()) != 0 {
			t.Fatal("codesign must not run without --ipa")
		}
	})

	t.Run("failed registration leaves no IPA", func(t *testing.T) {
		err := registerIOSEngineBaseline(f.project, func(string, []string) error { return errors.New("control plane 500") }, "release", append(append([]string{}, base...), signing...), true)
		if err == nil || !strings.Contains(err.Error(), "control plane 500") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(f.out); err == nil {
			t.Fatal("IPA published although the release was not registered")
		}
		if left, _ := filepath.Glob(filepath.Join(filepath.Dir(f.out), ".soroq-ipa-*")); len(left) != 0 {
			t.Fatalf("staged files leaked: %v", left)
		}
	})

	t.Run("signing failure registers nothing", func(t *testing.T) {
		delegated := false
		f.fake.codesignFn = func([]string) error { return errors.New("errSecInternalComponent") }
		defer func() { f.fake.codesignFn = nil }()
		err := registerIOSEngineBaseline(f.project, func(string, []string) error { delegated = true; return nil }, "release", append(append([]string{}, base...), signing...), true)
		if err == nil || !strings.Contains(err.Error(), "no release registered") || delegated {
			t.Fatalf("err=%v delegated=%v", err, delegated)
		}
	})

	t.Run("success publishes the IPA and strips signing flags", func(t *testing.T) {
		var gotArgs []string
		err := registerIOSEngineBaseline(f.project, func(_ string, a []string) error {
			gotArgs = a
			if _, err := os.Stat(f.out); err == nil {
				t.Error("IPA must not be in place before registration succeeds")
			}
			return nil
		}, "release", append(append([]string{}, base...), signing...), true)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if !reflect.DeepEqual(gotArgs, base) {
			t.Fatalf("delegate saw signing flags: %v", gotArgs)
		}
		if _, ok := zipNames(t, f.out)["Payload/Runner.app/embedded.mobileprovision"]; !ok {
			t.Fatal("published IPA lacks the embedded profile")
		}
	})
}

func TestReleaseIOSIPARequiresEngineBuild(t *testing.T) {
	for _, args := range [][]string{
		{"ios", "--ipa", "a.ipa", "--signing-identity", "S", "--provisioning-profile", "p"},
		{"ios", "--engine", "--ipa", "a.ipa", "--signing-identity", "S", "--provisioning-profile", "p"},
		{"ios", "--build", "--toolchain", "t", "--ipa", "a.ipa"},
	} {
		err := runRelease(args)
		if err == nil || !strings.Contains(err.Error(), "only available on the engine lane build") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

// The signing preflight must refuse BEFORE the engine route touches the project or builds anything.
func TestEngineLaneIPAPreflightRefusesBeforeAnySideEffect(t *testing.T) {
	for name, cfg := range map[string]string{"freehand": engineFreehandConfig, "scaffolded": engineScaffoldedConfig} {
		t.Run(name, func(t *testing.T) {
			project := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, cfg)
			fake := installFakeSigningExec(t)
			fake.identities = "     0 valid identities found\n"
			profile := writeIPATestProfile(t, t.TempDir(), "app", ipaTestProfile{appID: "com.example.app"})
			before := project.snapshot(t)
			err := runEngineLaneBuild(project, "--ipa", filepath.Join(t.TempDir(), "a.ipa"),
				"--signing-identity", "Apple Distribution: Nobody", "--provisioning-profile", profile)
			if err == nil || !strings.Contains(err.Error(), "not found among 0 valid codesigning identities") {
				t.Fatalf("err = %v", err)
			}
			project.assertUntouched(t, before)
		})
	}
}

func TestXMLPlistRoundTrip(t *testing.T) {
	in := map[string]any{
		"s": "a&b<c>", "t": true, "f": false, "i": int64(42), "arr": []any{"x", int64(1)},
		"d": map[string]any{"n": "v"}, "data": []byte{1, 2, 3}, "date": ipaTestNow,
	}
	out, err := parseXMLPlist(encodeXMLPlist(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", out, in)
	}
}

// Through the real router on the scaffolded route: the IPA flags never reach the delegate, and the
// IPA appears only after the (stubbed) registration succeeded.
func TestEngineLaneScaffoldedReleaseProducesSignedIPA(t *testing.T) {
	project := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, engineScaffoldedConfig)
	installFakeSigningExec(t)
	profile := writeIPATestProfile(t, t.TempDir(), "app", ipaTestProfile{appID: "com.example.app"})
	out := filepath.Join(t.TempDir(), "Runner.ipa")
	prevBuild, prevDelegate := engineLaneBuildAppDillFn, engineLaneDelegateFn
	t.Cleanup(func() { engineLaneBuildAppDillFn, engineLaneDelegateFn = prevBuild, prevDelegate })
	engineLaneBuildAppDillFn = func(dir, _ string, _ []string) (string, error) {
		writeIPATestApp(t, dir, false)
		return filepath.Join(dir, "build", "app.dill"), nil
	}
	var delegated []string
	engineLaneDelegateFn = func(_ string, a []string) error {
		delegated = a
		if _, err := os.Stat(out); err == nil {
			t.Error("IPA in place before registration")
		}
		return nil
	}
	err := runEngineLaneBuild(project, "--release-id", "r1", "--ipa", out,
		"--signing-identity", ipaTestSHA1(ipaTestCertDER), "--provisioning-profile", profile)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, a := range delegated {
		for _, f := range iosIPAValueFlags {
			if strings.HasPrefix(a, "--"+f) {
				t.Fatalf("delegate received signing flag %s: %v", a, delegated)
			}
		}
	}
	if _, ok := zipNames(t, out)["Payload/Runner.app/embedded.mobileprovision"]; !ok {
		t.Fatal("IPA missing embedded profile")
	}
}
