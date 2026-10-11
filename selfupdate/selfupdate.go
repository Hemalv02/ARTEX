// Package selfupdate implements ARTEX's one-click in-page upgrade: pull the new
// binary from a GitHub Release, verify, stage, and atomically swap on next start.
//
// Overall division of labor (see start.sh / start.bat):
//
//	Launch script = simple daemon loop, only "after the process exits, decide whether to relaunch based on the exit code"
//	This package  = all error-prone logic (download / SHA256 verify / smoke test / swap / failed rollback)
//
// The reason we do the swap in Go rather than in a script is that the SHA256 check and smoke test would have to be written twice (sha256sum / shasum / certutil) in sh and bat, and this is precisely the part that must not fail — swapping in a binary that can't run would have the daemon faithfully relaunch it over and over, forcing the user to recover manually on the machine.
//
// A full upgrade goes through three process starts:
//
//	① Old server receives /api/update/apply → download & verify → stage artex.new → exit 75
//	② Script relaunches the old build → Bootstrap finds artex.new → verify + smoke → swap → exit 75
//	③ Script relaunches, now running the new build → Bootstrap records an attempt → once startup succeeds, clear the marker
//
// Any step failing falls back to the old build: ② if verification fails, delete the staged file and keep running the old build; ③ if it doesn't survive long enough to clear the marker three times in a row (crashes instead of starting), automatically swap artex.old back in.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the exit code for "please have the daemon relaunch me" (EX_TEMPFAIL). When the launch script sees it,
// it immediately re-runs without counting it against the crash backoff. 0 means a normal user stop (script exits the loop), everything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after swap. Each time the new build starts it increments the counter; surviving past settleDelay clears the marker; crashing maxAttempts times in a row means the new build can't start at all and we auto-rollback.
const maxAttempts = 3

// Paths holds all files involved in a single upgrade, all anchored under **the directory of the executable**.
// Deliberately not CWD: under service runtime, the working directory could be / or anything, and using CWD would cause the staged file to land elsewhere, breaking the swap logic entirely.
type Paths struct {
	Dir     string // directory of the executable
	Current string // currently running binary           artex      / artex.exe
	New     string // staged new version                artex.new  / artex.new.exe
	Sum     string // sha256 (hex) of the new version   artex.new.sha256 / artex.new.exe.sha256
	Old     string // backup of the old version pre-swap artex.old  / artex.old.exe
	Marker  string // upgrade status marker              artex.upgrade.json
}

// ResolvePaths derives all upgrade paths from the current executable.
//
// On Windows, .new/.old also need the .exe suffix — otherwise smoke testing and post-swap execution will fail — so we strip the suffix first and reattach it, keeping the naming symmetric across platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locate executable: %w", err)
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

// marker tracks the progress of a single swap, used to trigger auto-rollback when the new build can't start.
type marker struct {
	From     string `json:"from"`     // version before the upgrade
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // number of startup attempts since swap
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

// cleanStaged removes the staged file. Swap success, verify failure, and user cancel all go through it, to prevent a leftover
// artex.new from being retried on the next start.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers and returns -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version (e.g. a local dev build "dev" or
// a git describe output like "0.3.7-2-gabc1234-dirty"); in that case the caller should disable one-click updates,
// otherwise a dev build would be "upgraded" to a release, overwriting uncommitted changes.
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

// parseVersion parses versions of the form "v0.3.7" / "0.3.7" into [3]int.
//
// Only a clean three-part form is accepted: build.sh uses git describe for non-tag builds, producing
// versions like "0.3.7-2-gabc1234" with suffixes — these must be judged incomparable rather than treated as 0.3.7 —
// otherwise a dev build would be misjudged as "already up to date" or be overwritten by a release.
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

// InDocker reports whether the process is running inside a container. In Docker, the swap writes to the container's writable layer,
// and `docker compose up -d` rebuilding the container falls back to the version baked into the image — this is expected behavior
// (the user is pulling a new image anyway), but the frontend should be able to explain this clearly.
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
