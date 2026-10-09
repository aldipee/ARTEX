package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global settings keys (stored in the settings key-value table, no schema needed).
const (
	// settingNotifyEnabled is the master switch for push notifications. On by default: it is for one-click kill-switch
	// during maintenance, not the feature's enablement condition — the real enablement condition is "is any channel
	// configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external address used to build the vulnerability-detail back link
	// (e.g. https://artex.example.com). When empty, messages carry no back-link button.
	// The project has no reusable external-address config, so this adds one.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest-mode period (in minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. 3 seconds is the upper bound of the engine's real-time
	// responsiveness, and the main source of latency between "vulnerability landed in the DB" and "message reached IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming a delivery. It must be significantly longer than the worst-case
	// duration of a single send (the notify package's HTTP client timeout is 15 seconds), otherwise the same row would
	// be delivered by two dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick caps the number of events fanned out per round, to avoid expanding the entire historical
	// backlog into delivery tasks all at once the first time a channel is enabled.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default value of the digest period.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no rate limit configured.
	// It exists to prevent "a channel configured as unlimited + a scan surfacing thousands of vulnerabilities" from
	// dragging a single round's loop into a long block.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the max deliveries a single channel sends per round.
	//
	// This cap is derived backwards from the **lease duration**: claiming stamps rows with a lease (notifyLease = 3
	// minutes), so if a round serially delivers enough that the worst case exceeds the lease, the lease expires before
	// the later rows even finish sending. Within a single process this doesn't matter (Run is a single goroutine running
	// serially, a tick never re-enters), but when **two processes share the same DB**, the peer would re-claim the
	// expired rows and send them again, double-incrementing attempts and marking them failed while the original process
	// is still delivering.
	//
	// Value: 3-minute lease / 30-second single send = 6 is **exactly consuming the lease** with zero margin, so it's
	// not usable; 5 leaves a 30-second margin over the 150-second worst case. This relationship is pinned down by
	// TestNotifyTickBudgetFitsWithinLease — changing notifyLease, notifySendTimeout, or this value would fail that
	// assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the value of the constant above; the
	// two multiplied together must not exceed notifyLease, see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failed retries, indexed by the number of attempts already made.
// The 3 chances (including the first) correspond to db.MaxNotifyAttempts; the two must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for vulnerability push notifications.
//
// It runs alongside Scheduler as an independent goroutine (see server.New). It deliberately reuses not Scheduler's
// tick: push's real-time requirement (3 seconds) differs from the trigger's business cadence, and the two failures
// don't implicate each other — a stuck push must not affect agent triggering.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. There are few channels and low contention, so one mutex is enough;
	// not worth introducing a finer-grained structure for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is a single channel's token bucket.
//
// A token bucket is used instead of a "count per minute then reset" sliding window because the latter has terrible
// boundary effects: send 20 at the very end of a window, then 20 more the next instant, and the platform sees 40 in
// one second and rate-limits you; a token bucket refills at a constant rate, naturally avoiding such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: first fan out new events, then deliver due tasks.
//
// Any step failure only logs and does not break the loop — a notification-system fault must never escalate into a
// process-level problem. Each tick is independent, and the next round will naturally retry.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to fan out events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket's unit is **message count** (equivalent to HTTP request count), not vulnerability count.
		// In real-time mode the two are equal (one vulnerability, one message); in digest mode a whole batch of
		// vulnerabilities composes into one message, so it consumes only one token.
		//
		// Both modes ask the token bucket first, then claim up to the allowance — the order must not be reversed,
		// otherwise a delivery blocked by rate-limiting has already spent retry attempts.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns this round's token consumption and batch-size upper bound for a digest channel.
//
// The two return values are **two different dimensions**, which is exactly why this is its own function:
//
//   - tokens is the message count. A batch of vulnerabilities composes into one message and one HTTP request, so it
//     is always 1. rate_per_min therefore still applies to digest (at most this many digest messages per minute).
//   - claimLimit is the max number of vulnerabilities this batch carries. It is bounded only by memory, unrelated to
//     the request budget.
//
// It once passed the per-round request budget (notifyMaxSendsPerChannelPerTick, derived from the lease) directly as
// the batch size, to make rate_per_min apply to digest. The consequence was that a rate_per_min=20 channel only
// refilled to 1 token within a 3-second tick, so each digest message carried only 1 vulnerability — digest degraded
// into "real-time push with digest wording", and readers received a string of "1 new vulnerability in the last 30
// minutes" while db.MaxDigestBatchSize was never reachable.
//
// This symptom is hard to catch in end-to-end tests (existing cases all hand a sufficiently large limit to stepDigest,
// bypassing the allowance calculation in step), so the decision is collected here and pinned directly by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers a channel's real-time tasks, one vulnerability per message.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim real-time deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// Render failure is a local data problem; retrying won't fix it.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message and sends it when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to determine digest batch due channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot is broken and thus failed to make it into the message must be explicitly marked
	// failed. Otherwise they'd stay outside `included`, neither in the message nor in the failure list — on a
	// successful send their status would be missed by the later batch mark, stuck at `sending` forever until the
	// lease expires and they get repeatedly re-claimed.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot could not be parsed, this vulnerability cannot be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad-snapshot deliveries channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipping %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Only hand the ones that made it into the message to send: included[i] corresponds strictly to msg.Items[i],
	// and send relies on this correspondence to land "the channel reported it fit the first K" on the correct
	// delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions status based on the result.
//
// A single batch of deliveries (possibly dozens in digest mode) shares one send result: either delivered, or the whole
// batch retries. No per-item retry — the digest message is one piece, and resending a subset would scramble the batch
// semantics.
//
// The only exception is **segmentation caused by the channel length limit**: if the channel reported it only fit the
// first K items, then from item K+1 onward must be held for the next batch rather than marked sent along with the
// rest. Otherwise the truncated vulnerabilities would be in neither the message nor the failure list — lost entirely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap a single send to avoid a stuck channel dragging down all remaining channels this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The delivered count the channel reported cannot exceed the delivery count; if it really happened, the
			// render layer miscounted. Treat as all delivered and record the problem, better than corrupting records.
			log.Printf("[notify] channel reported delivered count %d exceeding delivery count %d channel=%s, treating as all delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message hit the channel length limit: put the rest back on the queue immediately, to be continued
			// by the next tick. Use DeferDeliveries rather than RescheduleDeliveries — this is not a failure, and
			// must not spend the retry budget (the claim already optimistically +1'd, which will be decremented back).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message hit the channel length limit, only the first %d were delivered, the rest deferred to the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to enqueue segmented continuation channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither errored nor reported how many it delivered. Treat as failure (go through backoff),
		// lest this delivery be repeatedly re-claimed but never marked off.
		err = fmt.Errorf("channel did not report the delivered count (delivered=%d)", delivered)
	}

	// Failure disposition is decided **per item**, not by taking the whole batch's max attempt count.
	//
	// It once judged the whole batch dead via `if maxAttempts(deliveries) >= MaxNotifyAttempts`, but items within a
	// batch don't share the same attempt count: an old delivery already retried twice (attempts=2) would drag a
	// brand-new delivery in the same batch (attempts=1) into `failed` — a new vulnerability permanently lost without
	// a single retry, the exact opposite of the "don't let old rows drag new rows down" intent.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] error marking failure status channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] error marking failure status channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Regroup by delay: there are only 3 backoff tiers, so the group count is naturally small, no need to fire a
	// separate UPDATE per item (which would turn a 500-item batch into 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d exhausted=%d pending-retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in `all` that are not in `keep` (compared by pointer identity).
// Used to find deliveries that "failed to make it into the message" — they must be explicitly disposed of, not left
// in a gray zone.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt fetches the channel implementation and parses its config.
// ok=false means the type is not registered; the delivery should be marked failed directly rather than retried forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On config parse failure give an empty map: the channel's own Validate will report "which field is missing",
		// and that error guides the user's fix better than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single vulnerability message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders a digest message. It parses snapshots one by one — a single broken snapshot only skips that one
// item, without letting it drag down the whole batch.
//
// The returned `included` corresponds **strictly one-to-one** with msg.Items (the i-th delivery ↔ the i-th item).
// This correspondence is a hard requirement: the caller decides the first K deliveries are marked sent based on "the
// channel reported it fit the first K". If a broken snapshot were skipped here without removing the skipped delivery
// from `included`, the indices would misalign — a broken item that should fail would be marked sent, while a good
// item is wrongly judged not delivered. The broken ones are explicitly marked failed by the caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A broken snapshot neither enters the message nor `included` — its disposition is the caller's
			// responsibility (explicitly mark failed, rather than slip through as "delivered").
			log.Printf("[notify] skipping unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch could not be parsed", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a pending push item, resolving the asset names and detail back link along the
// way.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Asset-name resolution failure must not block the push: failing to read a name is far lighter than failing to
		// receive the notification — the message is just missing one line of assets.
		log.Printf("[notify] failed to resolve asset names finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// The detail-page route is at web/src/app/(main)/function/findings/detail/page.tsx,
		// which reads the vulnerability id from the `id` query parameter.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens withdraws **up to `want`** tokens from the channel's token bucket, returning the number actually taken.
//
// One token = one message (one HTTP request). In real-time mode the caller passes however many it needs; in digest
// mode a whole batch of vulnerabilities sends one message, so it passes 1.
//
// The bucket capacity is the channel's per-minute cap, refilled at a constant rate. ratePerMin<=0 means no rate
// limit, and a finite but large-enough value is returned to prevent a single round's loop from being dragged down by
// an unbounded backlog.
//
// The `want` cap is necessary: without it one could only drain the whole bucket, while the caller itself has a
// per-round cap, so over-withdrawn tokens are both unusable and silently vanish before the next refill — accumulated
// burst capacity would never be reachable, and even "no pending deliveries this round" would still deduct a chunk.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill by the real elapsed time, at ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: the token count is accumulated from floats, so refilling in two steps can
	// turn 0.5 + 0.5 into 0.9999999999, and a direct int() would truncate it to 0 — a mathematically-full bucket that
	// can't yield a token. 1e-9 is far smaller than one token, so it won't let a genuinely under-funded bucket through.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for back links, with the trailing slash stripped.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest period, falling back to the default when invalid or unset.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of the event corresponding to a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot of delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse event snapshot of delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event type follows the event's behavior; the one in the snapshot may have been written by an old version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
