// Package selfupdate implements ARTEX's one-click page update: fetching the new binary
// from a GitHub Release, verifying and staging it, then atomically swapping it in on the next startup.
//
// Overall division of labor (see start.sh / start.bat):
//
//	startup script = a dumb daemon loop, only deciding "whether to relaunch based on the exit code after the process exits"
//	this package    = all the error-prone logic (download / SHA256 verify / smoke test / swap / failure rollback)
//
// The swap lives in Go rather than the script because SHA256 verification and the smoke test would have to be written
// twice for sh and bat (sha256sum / shasum / certutil), and this is exactly the part that must not get anything wrong —
// swap in a binary that can't run and the daemon will faithfully relaunch it over and over, leaving the user to rescue
// the machine by hand.
//
// One complete upgrade spans three process startups:
//
//	① the old server receives /api/update/apply → download+verify → stage artex.new → exit 75
//	② the script relaunches the old version → Bootstrap finds artex.new → verify+smoke → swap → exit 75
//	③ the script relaunches, now already the new version → Bootstrap counts one attempt → clears the marker on successful startup
//
// Any step failing falls back to the old version: ② verification failure deletes the staged artifact and keeps running
// the old version; ③ failing to survive to marker-clear (crashes before starting) 3 times in a row automatically swaps
// artex.old back in.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the "please relaunch me, daemon" exit code (EX_TEMPFAIL). The startup script sees it
// and reruns immediately, not counted into crash backoff. 0 means the user stopped normally (the script exits the
// loop), everything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after a swap. Each startup of the new version increments the
// counter by 1; surviving settleDelay clears the marker; crashing maxAttempts times in a row means the new version
// simply won't start, triggering automatic rollback.
const maxAttempts = 3

// Paths holds all files involved in one upgrade, uniformly placed under **the executable's directory**.
// Deliberately not using CWD: under service-style runtime the working directory may be / or any path, and using CWD
// would land the staged artifacts elsewhere, breaking the swap logic outright.
type Paths struct {
	Dir     string // the executable's directory
	Current string // the currently running binary  artex      / artex.exe
	New     string // the staged new version       artex.new  / artex.new.exe
	Sum     string // the new version's sha256 (hex) artex.new.sha256 / artex.new.exe.sha256
	Old     string // the old version backed up before swap artex.old / artex.old.exe
	Marker  string // the upgrade state marker     artex.upgrade.json
}

// ResolvePaths derives all upgrade paths from the current executable.
//
// On Windows .new/.old must also carry the .exe suffix, otherwise the smoke test and post-swap execution would fail,
// so strip the suffix first and then re-append it, keeping the naming symmetric across both platforms.
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

// marker records the progress of one swap, used to trigger automatic rollback when the new version won't start.
type marker struct {
	From     string `json:"from"`     // the version before the upgrade
	To       string `json:"to"`       // the target version
	Attempts int    `json:"attempts"` // startup attempts already made after the swap
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

// cleanStaged removes the staged artifacts. Swap success, verification failure, and user cancel all go through it, to
// avoid a leftover artex.new being retried at the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers, returning -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version number (e.g. the "dev" of a local dev build, or the
// "0.3.7-2-gabc1234-dirty" produced by git describe), in which case the caller should disable one-click update,
// otherwise it would "upgrade" an in-development build into a release and overwrite uncommitted changes.
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

// parseVersion parses a "v0.3.7" / "0.3.7" form version number into [3]int.
//
// Only accepts a clean three-part form: build.sh produces suffixed versions like "0.3.7-2-gabc1234" via git describe
// on non-tag builds, and those must be judged non-comparable rather than treated as 0.3.7 — otherwise a dev build would
// be misjudged as "already latest" or get overwritten by a release.
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

// InDocker reports whether the process is running in a container. Under Docker the swap writes to the container's
// writable layer, and `docker compose up -d` rebuilding the container reverts to the image's own version — that's the
// expected behavior (the user is pulling a new image anyway), but the frontend must be able to say this clearly.
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
