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

// smokeEnv makes a child process launched by the smoke test skip Bootstrap directly.
//
// Strictly speaking nothing bad happens without it: the child's os.Executable() is artex.new, so every path derived
// from it carries a .new prefix and never touches the real upgrade files. But relying on that coincidence is fragile;
// an explicit short-circuit is clear at a glance and also spares the child one pointless disk probe.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is the instruction Bootstrap gives to main.
type Action int

const (
	// Continue: start the server as usual.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart, letting the daemon script relaunch.
	Restart
)

// State describes the upgrade state at this startup, so /api/update/check can honestly tell the frontend
// "whether the last upgrade succeeded or was rolled back".
type State struct {
	Pending     bool   // swapped but stability not yet confirmed
	RolledBack  bool   // this startup just performed an automatic rollback
	FailedStage bool   // the staged artifact failed verification/smoke test and was discarded
	Detail      string // one-sentence, user-facing explanation
}

// Bootstrap runs at the very start of main, and must be called before listening on any port or opening the database.
//
// Three situations:
//
//	① a staged artifact artex.new exists → verify + smoke test; if passed, swap and require a restart; if not, discard and keep running the old version
//	② only a marker file remains    → the swap just happened, count one attempt; after enough consecutive failures, roll back
//	③ nothing exists                 → normal startup
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

// applyStaged handles the "a staged artifact exists" situation: swap if verification passes, discard if it fails.
//
// This is the only place in the whole upgrade chain that overwrites the executable, and the last gate — the smoke test
// blocks corrupted downloads, wrong architecture, missing dynamic links, and the like. Once a binary that can't run
// gets through, the daemon script would relaunch it tirelessly while the Go code never gets a chance to run, and
// automatic rollback would be impossible.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] the staged new version failed verification, discarded, continuing with the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "new version verification failed, discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] swap failed, continuing with the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "swap failed: " + err.Error()}
	}

	// Swap succeeded. Keep the marker, leaving it to the next startup (which runs the new version) to confirm stability.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to write the upgrade marker (auto-rollback ability lost): %v", err)
	}
	log.Printf("[update] swapped to %s, exiting to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles the "post-swap startup": accumulate attempts, and swap the old version back once over the
// limit.
//
// The count only increments once the Go code runs, so it covers "executable but crashes during init" (incompatible
// config, port taken, DB migration blew up) faults; "can't exec at all" is blocked by the pre-swap smoke test. The two
// together are the complete picture.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// If rollback also failed, don't restart again, or it would loop forever. Clear the marker and
			// let the process start in its current state — if it can't start, the user can at least see why in the logs.
			log.Printf("[update] the new version failed to start %d times in a row, and rollback failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "new version failed to start and rollback failed: " + err.Error()}
		}
		log.Printf("[update] the new version failed to start %d times in a row, rolled back to %s, exiting to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("new version failed to start, rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to update the upgrade marker: %v", err)
	}
	log.Printf("[update] new version starting (attempt %d/%d), upgrade will be confirmed after stable run",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms the new version has been running stably and clears the upgrade marker.
//
// Called by main after the HTTP listener comes up, with a delay: surviving this long is what counts, otherwise the
// marker stays in place and the next startup keeps accumulating attempts until rollback triggers.
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
		log.Printf("[update] failed to clear the upgrade marker: %v", err)
		return
	}
	log.Printf("[update] new version running stably, upgrade complete (previous version kept as %s)", p.Old)
}

// SettleDelay is the runtime required to judge that "the new version survived".
const SettleDelay = 30 * time.Second

// verifyStaged verifies the staged artifact: first compare SHA256, then actually launch it once.
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
		return errors.New("SHA256 mismatch (download corrupted or tampered)")
	}
	return smokeTest(p.New)
}

// smokeTest launches the new binary with -h to confirm it can actually execute on the current system.
// This blocks a whole class of problems: truncated downloads, wrong architecture (exec format error), missing deps, etc.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("grant execute permission: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("smoke test timed out (new binary not responding)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap swaps the current binary for the staged new version.
//
// Both Unix and Windows allow renaming a running executable (Windows forbids deleting and overwriting, rename is not
// among them), so this needs no per-platform handling and no need to stop itself first.
func swap(p Paths) error {
	// Windows' rename does not overwrite an existing target; the .old left by the previous upgrade must be removed first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clean up old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("back up current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// Swap failed but the current version was already moved away; it must be put back as-is, otherwise there'd be no executable at next startup.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("install new version failed(%v), and restoring the current version failed: %w", err, rerr)
		}
		return fmt.Errorf("install new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback swaps the old version backed up by swap back in.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("no backup to roll back to %s: %w", p.Old, err)
	}
	// Move the version that couldn't start to .failed for investigation rather than deleting it outright.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("move away the failed version: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restore old version: %w", err)
	}
	return nil
}

// Rollback is the implementation of /api/update/rollback: proactively roll back to the previous version.
// It only swaps; restarting is likewise handed to the daemon script (the caller then exits with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("previous version cannot execute, refusing to roll back: %w", err)
	}
	// Swap current and backup: after rolling back you can roll forward again.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("move away current version: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("install previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] failed to tidy up the backup after rollback (does not affect running): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether there is a previous version to roll back to, so the frontend can decide whether to show the rollback button.
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
