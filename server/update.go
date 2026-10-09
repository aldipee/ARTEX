package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// HTTP surface for the one-click in-page update. The real download/verify/swap logic is all in the selfupdate
// package; here we only handle the auth boundary, concurrency mutual exclusion, progress broadcast, and telling
// main that "it's time to exit".
//
// The restart is not done by this process: after staging the new version it exits with selfupdate.ExitRestart, and
// a watchdog script (start.sh / start.bat, or ENTRYPOINT under Docker) relaunches it.

// restartCh is closed when the upgrade is ready or the rollback completes; main exits with ExitRestart on receiving it.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested returns a channel that is closed when "please exit and let the watchdog relaunch me" fires.
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState is the conclusion of selfupdate.Bootstrap at this startup (upgrade succeeded / just rolled back /
// staged artifact discarded), injected by main so /api/update/check can honestly tell the frontend how the last
// upgrade ended.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState is called once by main at startup.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache caches the latest-version query results from GitHub.
//
// The top bar's "new version available" hint queries once per full page load, but the unauthenticated GitHub API
// allows 60 requests per IP per hour — without caching, opening a few tabs or refreshing a few times exhausts the
// quota, and then a real update can't even be checked. The user can force a cache bypass when explicitly clicking
// "check for updates".
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch is the data-fetch function, an injection point left for tests; when nil it does the real GitHub query.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// Failure results are also cached for a short while, otherwise every page load waits out a timeout when GitHub
	// is unreachable; but the TTL must be short so it self-heals soon after the network recovers.
	releaseErrTTL = 2 * time.Minute
	// The query timeout. NewClient's 30-minute timeout is for downloading the whole package; checking a version can't wait that long.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get returns the latest Release, hitting the network only on a cache miss.
//
// The lock is held throughout the fetch: concurrent requests queue up for the same query's result instead of each
// hitting GitHub (multiple tabs querying at once right after page load is exactly when rate-limiting is most likely).
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// A canceled request (the user closed the tab) doesn't mean GitHub is broken; don't cache it, otherwise the next
	// visitor gets a baffling "canceled" error.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress is one progress item pushed to the frontend.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // only meaningful during download; -1 otherwise
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub holds one upgrade's progress and broadcasts it to SSE subscribers.
//
// running doubles as a mutex: a second POST /api/update/apply during an upgrade returns 409, avoiding two goroutines
// writing to the same artex.new at once.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin seizes the upgrade right; returns false if one is already in progress.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "preparing…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish ends one upgrade. A nil err means staging succeeded, waiting for restart.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "update failed", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "new version ready, restarting…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout must be called while holding h.mu. Subscriber channels are buffered; when full we drop —
// progress is transient, discardable information, and a stuck SSE connection must never block the upgrade itself.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck queries the latest release on GitHub and compares it with the current version.
//
// The frontend also connects to api.github.com directly (GitHub's CORS is *), but this endpoint is authoritative:
// the download happens on the backend, so an update is only possible if the backend can reach GitHub. It's common
// for the browser to connect while the server can't (the server is on an intranet, or the proxy is only configured
// in the browser); then an update would inevitably fail, so it's better to report honestly at the check step.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// The top-bar hint uses the cache (default); the user's "check for updates" passes force=1 to force a re-fetch.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// Dev builds (dev / git describe with a suffix) have no comparable version number. Allowing it would only
		// overwrite the locally-debugged binary with a release build, so no update is offered.
		out["reason"] = fmt.Sprintf("current version %q is not a release build, one-click update is disabled", current)
	}
	writeJSON(w, 200, out)
}

// updateApply downloads and stages the new version, then exits so the watchdog script can restart.
//
// Returns 202 immediately; the real work runs on a background goroutine: downloading the whole package can take a
// few minutes, and tying it to the request would get cut off by a reverse-proxy timeout. Progress goes through
// /api/update/stream.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// Use the cache: ensure what gets installed is exactly the version the user saw and confirmed in the UI.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("current version %q is not a release build, one-click update is disabled", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("already on the latest version %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "an update is already in progress")
		return
	}

	go func() {
		// Deliberately use s.ctx instead of the request's ctx: the request ends as soon as the HTTP response returns,
		// and a download tied to it would be canceled immediately.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] update failed: %v", err)
			return
		}
		log.Printf("[update] %s → %s staged, exiting soon to complete the swap", current, rel.TagName)
		// Leave a moment to push the final progress item to the frontend, then trigger the exit.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback actively rolls back to the previous version (the artex.old backed up before the swap).
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "an update is in progress, cannot roll back")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] manually rolled back to the previous version, exiting soon to complete the switch")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream pushes update progress over SSE.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Send a current-state item first, so a page refresh can immediately show an in-progress upgrade.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
