package main

// `soroq analytics`: how an app's OTA delivery is going, from the control plane's GET /v1/analytics.
//
// It reports what the server derives from device boot reports: per patch, how many devices booted it
// successfully, how many failed and why, and whether it was rolled back. It is NOT app-user analytics;
// Soroq collects nothing about what an app's users do.
//
// The server's "observed" flag is carried through as-is: a patch no device has reported on is shown as
// "no reports yet", never as 0% failures.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type analyticsPatchRow struct {
	PatchID                   string         `json:"patch_id"`
	PatchNumber               int            `json:"patch_number"`
	Channel                   string         `json:"channel"`
	Track                     string         `json:"track"`
	ReleaseID                 string         `json:"release_id"`
	RolledBack                bool           `json:"rolled_back"`
	Observed                  bool           `json:"observed"`
	SuccessfulDevices         int            `json:"successful_devices"`
	FailedDevices             int            `json:"failed_devices"`
	VerifiedSuccessfulDevices int            `json:"verified_successful_devices"`
	VerifiedFailedDevices     int            `json:"verified_failed_devices"`
	UnverifiedDevices         int            `json:"unverified_devices"`
	FailureClasses            map[string]int `json:"failure_classes,omitempty"`
}

type analyticsView struct {
	AppID  string `json:"app_id"`
	Totals struct {
		Patches           int `json:"patches"`
		ObservedPatches   int `json:"observed_patches"`
		SuccessfulDevices int `json:"successful_devices"`
		FailedDevices     int `json:"failed_devices"`
		UnverifiedDevices int `json:"unverified_devices"`
		RolledBackPatches int `json:"rolled_back_patches"`
	} `json:"totals"`
	FailureClasses map[string]int      `json:"failure_classes,omitempty"`
	Patches        []analyticsPatchRow `json:"patches"`
	Notice         string              `json:"notice,omitempty"`
	// Devices is installs per release (runtime + channel), from device update checks. Older servers
	// do not send it; the section is then simply absent.
	Devices            []analyticsDeviceRow `json:"devices,omitempty"`
	DevicesUnavailable string               `json:"devices_unavailable,omitempty"`
}

type analyticsDeviceRow struct {
	ReleaseID  string    `json:"release_id"`
	ReleaseIDs []string  `json:"release_ids,omitempty"`
	Version    string    `json:"version"`
	Platform   string    `json:"platform,omitempty"`
	RuntimeID  string    `json:"runtime_id"`
	Channel    string    `json:"channel"`
	Devices    int       `json:"devices"`
	Active24h  int       `json:"active_24h"`
	Active7d   int       `json:"active_7d"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	// Platforms splits the counts by platform (android, ios, unknown). Older servers do not send it;
	// the per-platform columns are then left out.
	Platforms []analyticsDevicePlatform `json:"platforms,omitempty"`
}

type analyticsDevicePlatform struct {
	Platform  string `json:"platform"`
	Devices   int    `json:"devices"`
	Active24h int    `json:"active_24h"`
	Active7d  int    `json:"active_7d"`
}

// platformDevices returns the devices counted under one platform of a row.
func (row analyticsDeviceRow) platformDevices(platform string) int {
	for _, p := range row.Platforms {
		if p.Platform == platform {
			return p.Devices
		}
	}
	return 0
}

func runAnalytics(args []string) error {
	fs := flag.NewFlagSet("analytics", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "app id (default: app_id from soroq.yaml in --project-dir)")
	projectDir := fs.String("project-dir", ".", "Flutter project whose soroq.yaml names the app")
	releaseID := fs.String("release-id", "", "only patches of this release")
	channel := fs.String("channel", "", "only patches on this channel")
	jsonOut := fs.Bool("json", false, "emit the control plane's JSON unchanged")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq analytics [--app-id com.example.app] [--release-id <id>] [--channel <name>] [--project-dir .] [--api https://api.soroq.dev] [--json]

Shows how your OTA updates are reaching devices: per patch, devices that booted it, devices where it
failed (and why), and rollbacks. Numbers come from the boot reports devices send after applying a
patch. Also shows devices per release: distinct installs that checked for updates (total, active in
the last 24h / 7d). Only a salted hash of each install's random id is kept. Soroq collects nothing
about what your app's users do.`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := refuseUnconsumedArguments("analytics", fs.Args(), nil); err != nil {
		return err
	}

	resolvedAppID := strings.TrimSpace(*appID)
	if resolvedAppID == "" {
		status, err := inspectProject(*projectDir)
		if err != nil || strings.TrimSpace(status.AppID) == "" {
			return errors.New("no app id: pass --app-id, or run inside a Soroq project (soroq.yaml with app_id)")
		}
		resolvedAppID = strings.TrimSpace(status.AppID)
	}
	if !looksLikeSoroqAppID(resolvedAppID) {
		return fmt.Errorf("app id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", resolvedAppID)
	}

	query := url.Values{}
	query.Set("app_id", resolvedAppID)
	view, err := getJSONDecode[analyticsView](strings.TrimRight(*apiBase, "/") + "/v1/analytics?" + query.Encode())
	if err != nil {
		return err
	}
	view = filterAnalytics(view, strings.TrimSpace(*releaseID), strings.TrimSpace(*channel))
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	renderAnalytics(os.Stdout, view)
	return nil
}

// filterAnalytics narrows the rows and recomputes the totals from them, exactly as the server does.
func filterAnalytics(view analyticsView, releaseID, channel string) analyticsView {
	if releaseID == "" && channel == "" {
		return view
	}
	out := analyticsView{AppID: view.AppID, Notice: view.Notice, DevicesUnavailable: view.DevicesUnavailable}
	if view.Devices != nil {
		out.Devices = []analyticsDeviceRow{}
	}
	for _, row := range view.Devices {
		if channel != "" && row.Channel != channel {
			continue
		}
		if releaseID != "" && row.ReleaseID != releaseID && !containsString(row.ReleaseIDs, releaseID) {
			continue
		}
		out.Devices = append(out.Devices, row)
	}
	classes := map[string]int{}
	for _, row := range view.Patches {
		if (releaseID != "" && row.ReleaseID != releaseID) || (channel != "" && row.Channel != channel) {
			continue
		}
		out.Patches = append(out.Patches, row)
		out.Totals.Patches++
		if row.Observed {
			out.Totals.ObservedPatches++
		}
		out.Totals.SuccessfulDevices += row.SuccessfulDevices
		out.Totals.FailedDevices += row.FailedDevices
		out.Totals.UnverifiedDevices += row.UnverifiedDevices
		if row.RolledBack {
			out.Totals.RolledBackPatches++
		}
		for class, n := range row.FailureClasses {
			classes[class] += n
		}
	}
	if len(classes) > 0 {
		out.FailureClasses = classes
	}
	return out
}

// successRate is successful / (successful + failed), or "" when no device has reported.
func successRate(ok, failed int) string {
	if ok+failed == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f%%", 100*float64(ok)/float64(ok+failed))
}

func renderAnalytics(w io.Writer, view analyticsView) {
	t := view.Totals
	fmt.Fprintf(w, "app: %s\n", view.AppID)
	fmt.Fprintf(w, "patches: %d (%d with device reports, %d rolled back)\n", t.Patches, t.ObservedPatches, t.RolledBackPatches)
	fmt.Fprintf(w, "devices: %d booted a patch, %d failed", t.SuccessfulDevices, t.FailedDevices)
	if rate := successRate(t.SuccessfulDevices, t.FailedDevices); rate != "" {
		fmt.Fprintf(w, " (%s success)", rate)
	}
	fmt.Fprintln(w)
	if len(view.Patches) == 0 {
		fmt.Fprintln(w, "\nno patches published for this app yet")
		renderDevices(w, view)
		return
	}

	// One release's patches together, newest patch first: the server orders by patch number alone,
	// which interleaves every release's #1, #2, ...
	rows := append([]analyticsPatchRow(nil), view.Patches...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ReleaseID != rows[j].ReleaseID {
			return rows[i].ReleaseID < rows[j].ReleaseID
		}
		return rows[i].PatchNumber > rows[j].PatchNumber
	})

	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATCH\tCHANNEL\tTRACK\tBOOTED\tFAILED\tSUCCESS\tSTATE\tRELEASE")
	for _, row := range rows {
		booted, failed, rate := "-", "-", "no reports yet"
		if row.Observed {
			booted, failed = fmt.Sprint(row.SuccessfulDevices), fmt.Sprint(row.FailedDevices)
			rate = successRate(row.SuccessfulDevices, row.FailedDevices)
			if rate == "" {
				rate = "-"
			}
		}
		state := "live"
		if row.RolledBack {
			state = "rolled back"
		}
		fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.PatchNumber, row.Channel, row.Track, booted, failed, rate, state, row.ReleaseID)
	}
	tw.Flush()

	if len(view.FailureClasses) > 0 {
		fmt.Fprintln(w, "\nfailure reasons:")
		names := make([]string, 0, len(view.FailureClasses))
		for name := range view.FailureClasses {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			if view.FailureClasses[names[i]] != view.FailureClasses[names[j]] {
				return view.FailureClasses[names[i]] > view.FailureClasses[names[j]]
			}
			return names[i] < names[j]
		})
		for _, name := range names {
			fmt.Fprintf(w, "  %-32s %d\n", name, view.FailureClasses[name])
		}
	}
	if t.UnverifiedDevices > 0 {
		fmt.Fprintf(w, "\nnote: %d of these device reports carried no signed identity; boot reports are open by design, so treat counts as indicative.\n", t.UnverifiedDevices)
	}
	renderDevices(w, view)
}

// renderDevices prints installs per release. Nothing is printed for a server that predates it.
func renderDevices(w io.Writer, view analyticsView) {
	if view.DevicesUnavailable != "" {
		fmt.Fprintf(w, "\ndevices per release: unavailable (%s)\n", view.DevicesUnavailable)
		return
	}
	if view.Devices == nil {
		return
	}
	fmt.Fprintln(w, "\ndevices per release (distinct installs that checked for updates):")
	if len(view.Devices) == 0 {
		fmt.Fprintln(w, "  no devices have checked in yet")
		return
	}
	// Per-platform columns only when the server splits by platform; UNKNOWN only when some install is not
	// yet attributed.
	split, unknown := false, 0
	for _, row := range view.Devices {
		split = split || row.Platforms != nil
		unknown += row.platformDevices("unknown")
	}
	platformHeader := ""
	if split {
		platformHeader = "ANDROID\tIOS\t"
		if unknown > 0 {
			platformHeader += "UNKNOWN\t"
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RELEASE\tVERSION\tCHANNEL\tDEVICES\t"+platformHeader+"ACTIVE 24H\tACTIVE 7D\tLAST SEEN\tRUNTIME")
	for _, row := range view.Devices {
		release, version := row.ReleaseID, row.Version
		if release == "" {
			release = "(unregistered)"
		} else if extra := len(row.ReleaseIDs) - 1; extra > 0 {
			release = fmt.Sprintf("%s (+%d)", release, extra)
		}
		if version == "" {
			version = "-"
		}
		runtime := row.RuntimeID
		if len(runtime) > 12 {
			runtime = runtime[:12]
		}
		lastSeen := "-"
		if !row.LastSeen.IsZero() {
			lastSeen = row.LastSeen.UTC().Format("2006-01-02 15:04")
		}
		platforms := ""
		if split {
			platforms = fmt.Sprintf("%d\t%d\t", row.platformDevices("android"), row.platformDevices("ios"))
			if unknown > 0 {
				platforms += fmt.Sprintf("%d\t", row.platformDevices("unknown"))
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s%d\t%d\t%s\t%s\n",
			release, version, row.Channel, row.Devices, platforms, row.Active24h, row.Active7d, lastSeen, runtime)
	}
	tw.Flush()
	if unknown > 0 {
		fmt.Fprintf(w, "note: %d install(s) are not attributed to a platform yet; each is once it next checks for updates"+
			" through the iOS engine or the Android runtime lane.\n", unknown)
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
