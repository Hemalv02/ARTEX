package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv lets the child process spawned by the smoke test skip
// Bootstrap directly.
//
// Strictly speaking omitting it would not cause trouble: the child's
// os.Executable() is artex.new, so every derived path carries a .new
// suffix and never touches the real upgrade files. But relying on that
// coincidence is too fragile; short-circuiting explicitly is clearer
// and saves the child one wasted disk probe.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is the instruction Bootstrap hands back to main.
type Action int

const (
	// Continue: start the server as usual.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart and let the daemon
	// script respawn the process.
	Restart
)

// State describes the upgrade status at the current startup, so
// /api/update/check can honestly tell the front end "was the previous
// upgrade successful or rolled back".
type State struct {
	Pending     bool   // staged but not yet confirmed stable
	RolledBack  bool   // an automatic rollback just ran on this startup
	FailedStage bool   // staged binary failed verification/smoke, discarded
	Detail      string // one-sentence user-facing explanation
}

// Bootstrap runs at the very beginning of main, before any listener is
// opened or any database is touched.
//
// Three situations:
//
//	① staged binary artex.new exists → verify + smoke; on success swap and
//	   request a restart, on failure discard and keep running the old version
//	② only the marker file is present → we just swapped, increment the
//	   attempt counter; if too many failures in a row, roll back
//	③ nothing of the above            → normal startup
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] skipping bootstrap: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged handles the "staged binary exists" case: swap it in on
// success, discard it on failure.
//
// This is the only place in the entire upgrade chain that overwrites
// the executable, and it is the last line of defence — the smoke test
// stops corrupted downloads, wrong architecture picks, missing dynamic
// libraries, and the like. Once we let through a binary that won't
// run, the daemon script will respawn it tirelessly while the Go code
// never gets a chance to execute, so automatic rollback becomes
// impossible.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] staged new version failed verification, discarded, continuing on the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "New version failed verification, discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] swap failed, continuing on the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "Swap failed: " + err.Error()}
	}

	// Swap succeeded. Keep the marker and let the next startup
	// (running the new version) confirm stability.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to write upgrade marker (lost automatic rollback capability): %v", err)
	}
	log.Printf("[update] swapped to %s, exiting for restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles "startup after a swap": increment the
// attempt counter, roll back if the limit is exceeded.
//
// The counter only increments after the Go code has run, so it covers
// failures where the binary "executes but crashes during init"
// (incompatible config, port already taken, exploding DB migrations).
// Failures where the binary "can't exec at all" are caught by the
// pre-swap smoke test; together they form a complete safety net.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// If rollback itself fails, don't keep restarting — that
			// would loop forever. Drop the marker and let the process
			// start in its current state; if it can't start, at least
			// the user can see the reason in the logs.
			log.Printf("[update] new version failed to start %d times in a row, and rollback also failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "New version failed to start and rollback also failed: " + err.Error()}
		}
		log.Printf("[update] new version failed to start %d times in a row, rolled back to %s, exiting for restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("New version failed to start, rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to update upgrade marker: %v", err)
	}
	log.Printf("[update] new version starting (attempt %d/%d); once it runs stably the upgrade will be confirmed",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms the new version has run stably and clears the
// upgrade marker.
//
// Called by main with a delay after the HTTP listener is up: surviving
// this window is what counts. Otherwise the marker stays in place and
// the next startup keeps incrementing the attempt counter until the
// rollback threshold is hit.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // not a post-upgrade startup, nothing to do
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] failed to clear upgrade marker: %v", err)
		return
	}
	log.Printf("[update] new version is running stably, upgrade complete (previous version retained as %s)", p.Old)
}

// SettleDelay is the runtime required to decide that "the new version
// survived".
const SettleDelay = 30 * time.Second

// verifyStaged validates the staged binary: first compare SHA256,
// then actually launch it once to confirm it runs.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("compute checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (download corrupted or tampered with)")
	}
	return smokeTest(p.New)
}

// smokeTest launches the new binary with -h to confirm it can actually
// execute on the current system. This blocks a large class of
// problems: truncated downloads, wrong architecture (exec format
// error), missing dependencies, and so on.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("set executable bit: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("smoke test timed out (new binary is unresponsive)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return fmt.Errorf("smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap replaces the current binary with the staged new version.
//
// Both Unix and Windows allow renaming a running executable (Windows
// only forbids deletion and overwriting; rename is not on that list),
// so we don't need a per-platform branch, nor do we need to stop
// ourselves first.
func swap(p Paths) error {
	// Windows' rename does not overwrite an existing target, so the
	// .old left behind by the previous upgrade must be cleared first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("back up current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// Swap failed but the current version has already been moved
		// out, so we must put it back exactly as it was; otherwise the
		// next startup will have no executable.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("install new version failed (%v) and restoring current version also failed: %w", err, rerr)
		}
		return fmt.Errorf("install new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback swaps the old version (backed up by swap) back in.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("no rollback backup %s: %w", p.Old, err)
	}
	// Move the failed new version aside as .failed for inspection
	// instead of deleting it outright.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("move failed version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restore old version: %w", err)
	}
	return nil
}

// Rollback implements /api/update/rollback: actively revert to the
// previous version. It only performs the swap; the restart is again
// left to the daemon script (caller subsequently exits with
// ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version available to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("previous version cannot execute, refusing to roll back: %w", err)
	}
	// Swap current and backup: after the rollback we can still roll
	// forward again.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("move current version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("install previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] failed to clean up backup after rollback (does not affect runtime): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether a previous version is available to roll
// back to, so the front end can decide whether to show the rollback
// button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown version"
	}
	return s
}
