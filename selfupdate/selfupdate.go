// Package selfupdate implements ARTEX's one-click in-page update: it pulls the new
// binary from a GitHub Release, verifies it, stages it, and atomically swaps it in on the next startup.
//
// The overall division of labor (see start.sh / start.bat):
//
//	the start script = a dumb daemon loop that only "decides whether to relaunch based on the exit code after the process exits"
//	this package     = all the error-prone logic (download / SHA256 verification / smoke / swap / rollback on failure)
//
// The swap lives in Go rather than the script because the sha256 verification and the smoke test would need two
// implementations on sh and bat (sha256sum / shasum / certutil), and this is precisely the part that can least afford a mistake — install a
// binary that won't run and the daemon faithfully relaunches it over and over, leaving the user to rescue it by hand on the machine.
//
// A complete upgrade goes through three process startups:
//
//	① the old server receives /api/update/apply → download and verify → stage artex.new → exit 75
//	② the script relaunches the old version → Bootstrap finds artex.new → verify + smoke → swap → exit 75
//	③ the script relaunches, now the new version → Bootstrap records one attempt → clears the marker after a successful start
//
// Any failing step reverts to the old version: ② if verification fails, delete the staged file and keep running the old version; ③ if it fails to survive to
// the marker clearing 3 times in a row (crashing on start), automatically swap artex.old back in.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the "please relaunch me, daemon" exit code (EX_TEMPFAIL). When the start script sees it
// it reruns immediately, not counting it toward crash backoff. 0 means the user stopped normally (the script exits the loop); anything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after a swap. The new version adds 1 to the count on each startup; surviving
// settleDelay clears the marker; crashing maxAttempts times in a row means the new version simply won't start, so it auto-rolls-back.
const maxAttempts = 3

// Paths is every file involved in an upgrade, all kept under **the directory of the executable**.
// Deliberately not CWD: when run as a service the working directory may be / or any path, and using CWD would make the staged file land
// somewhere else, breaking the swap logic outright.
type Paths struct {
	Dir     string // the directory of the executable
	Current string // the currently running binary       artex      / artex.exe
	New     string // the staged new version             artex.new  / artex.new.exe
	Sum     string // the new version's sha256 (hex)      artex.new.sha256 / artex.new.exe.sha256
	Old     string // the old version backed up before the swap  artex.old  / artex.old.exe
	Marker  string // the upgrade-status marker           artex.upgrade.json
}

// ResolvePaths derives all the upgrade paths from the current executable.
//
// On Windows the .new/.old must also carry the .exe suffix, or the smoke test and the post-swap execution both fail,
// so the suffix is stripped off first and then re-joined, making the naming symmetric across the two platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locating the executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records the progress of one swap, used to trigger an automatic rollback when the new version won't start.
type marker struct {
	From     string `json:"from"`     // the version before the upgrade
	To       string `json:"to"`       // the target version
	Attempts int    `json:"attempts"` // the number of startup attempts since the swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes the staged files. A successful swap, a failed verification, and a user cancel all go through it, to avoid a leftover
// artex.new being retried on the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers, returning -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version number (e.g. a local dev build's "dev" or
// git describe's "0.3.7-2-gabc1234-dirty"); in that case the caller should disable one-click update,
// or it would "upgrade" an in-development build to a stable release and overwrite uncommitted changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a version number of the form "v0.3.7" / "0.3.7" into a [3]int.
//
// It accepts only a clean three-part form: on a non-tag build, build.sh produces via git describe
// suffixed versions like "0.3.7-2-gabc1234", which must be judged non-comparable rather than treated as
// 0.3.7 — otherwise a dev build would be misjudged as "already latest" or overwritten by a stable release.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process is running inside a container. Under Docker a swap writes to the container's writable layer,
// and `docker compose up -d` recreating the container reverts to the version shipped in the image — this is expected behavior
// (the user is pulling a new image anyway at that point), but the frontend needs to be able to state it clearly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
