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

// Global settings keys (stored in the settings key/value table, no schema needed).
const (
	// settingNotifyEnabled is the master switch for notifications. Defaults to on: it's used
	// to quickly stop the bleeding during maintenance, not as a feature enable condition —
	// the real enable condition is "is any channel configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external access address used to generate links
	// back to finding details (e.g. https://artex.example.com). If empty, messages
	// won't include a back-link button. The project has no reusable external URL
	// config, so this is a new entry.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest mode's interval (in minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. 3 seconds is the upper bound
	// on the engine's real-timeliness, and the main source of latency between "finding
	// committed" and "message arrives in IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming a delivery. It must be significantly
	// greater than the worst-case single delivery duration (the notify package's HTTP
	// client has a 15s timeout), otherwise the same row could be delivered by two
	// dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits the number of events dispatched per round, to avoid
	// unfolding all backlog at once into delivery tasks the first time a channel is enabled.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest interval.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no rate limit.
	// It exists to prevent a single round's loop from being blocked for a long time when
	// "a channel is set to unlimited + a scan found thousands of findings".
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the max number of sends per channel per round.
	//
	// This cap is derived from the **lease duration**: when a row is claimed, it gets a lease
	// (notifyLease = 3 minutes). If the worst-case duration of serial sends in a round exceeds
	// the lease, the later sends haven't finished when the lease expires. Within a single
	// process this doesn't matter (Run is a single goroutine, the tick isn't reentrant), but
	// when **two processes share a database**, the other end will re-claim the expired rows
	// and resend them, double-incrementing attempts and judging the original still-in-progress
	// process as failed.
	//
	// Value: 3-minute lease / 30-second single send = 6 is **just full** with zero margin, so
	// we can't take it; 5 leaves 30s margin for a 150s worst case. This relationship is pinned
	// by TestNotifyTickBudgetFitsWithinLease — changing notifyLease, notifySendTimeout, or
	// this value will break that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the single-send timeout. It also determines the previous
	// constant's value; their product must not exceed notifyLease, see
	// TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the failure retry backoff sequence, indexed by the number of
// attempts already made. 3 chances (including the first attempt) correspond to
// db.MaxNotifyAttempts — change them together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for finding notifications.
//
// It runs as an independent goroutine alongside Scheduler (see server.New), and deliberately
// does not reuse Scheduler's tick: notification's real-time requirement (3 seconds) differs
// from triggers' business cadence, and their failures must be independent — a stuck notification
// must not affect agent triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. Channel counts are small and contention is low; a single
	// mutex is enough — not worth introducing finer-grained structures for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is a single channel's token bucket.
//
// We use a token bucket rather than "count per minute then reset" sliding windows because
// the latter has terrible boundary effects: send 20 at the window's end, then 20 more the
// next instant — to the platform that's 40 in one second, triggering rate limit. A token
// bucket refills at a constant rate and naturally avoids such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Launched once by server.New.
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

// step runs one round: first dispatch new events, then deliver due tasks.
//
// Any failure only logs — it never breaks the loop. A notification system failure must
// not escalate to a process-level problem. Each tick is independent; the next tick
// naturally retries.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] dispatch events failed: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] read channels failed: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket's unit is **message count** (equivalent to HTTP request count),
		// not finding count. In realtime mode the two are the same (one message per finding);
		// in digest mode a whole batch of findings collapses into one message, so only one
		// token is consumed.
		//
		// Both modes first ask the bucket, then claim by the quota — the order can't be
		// reversed, otherwise a rate-limit-blocked delivery has already consumed a retry.
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

// digestTickPlan returns the digest channel's token consumption and batch-size cap for this round.
//
// The two return values are **two different dimensions**, which is exactly why this is a
// standalone function:
//
//   - tokens is message count. A batch of findings collapses into one message, one HTTP request,
//     so this is always 1. rate_per_min therefore still applies to digest (at most this many
//     digest messages per minute).
//   - claimLimit is the max number of findings this batch can hold. It's only bounded by
//     memory and has nothing to do with the request budget.
//
// At one point, to make rate_per_min apply to digest, the per-round request budget
// (notifyMaxSendsPerChannelPerTick, derived from the lease) was passed straight down as the
// batch size. The result: a channel with rate_per_min=20 only refills 1 token per 3-second
// tick, so each digest message only holds 1 finding — digest devolved into "realtime push
// with digest wording", and readers received a stream of "1 new finding in the last 30 min",
// with db.MaxDigestBatchSize never reachable.
//
// This symptom is hard to catch in end-to-end tests (existing cases manually pass a large
// enough limit to stepDigest, sidestepping step's quota calculation), so we pin the decision
// here with TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers the realtime tasks for a channel, one message per finding.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] claim realtime deliveries failed channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A render failure is a local data problem — retrying won't help.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] check digest batch due failed channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] claim digest batch failed channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot is broken and didn't make it into the message must be
	// explicitly marked as failed. Otherwise they sit outside `included` — neither in the
	// message nor in the failed list — and when send succeeds the batch mark misses them,
	// leaving them stuck in "sending" until the lease expires and they're repeatedly claimed.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot cannot be parsed; this finding cannot be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] mark bad-snapshot deliveries failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Only pass the included ones to send: included[i] corresponds strictly to msg.Items[i];
	// send relies on this correspondence to land "the channel reports carrying the first K" on
	// the correct delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions state based on result.
//
// The same batch (potentially dozens in digest mode) shares a single send result: either
// delivered, or the whole batch retries. No per-item retry — digest messages are one, and
// resending part of a digest would garble the batch semantics.
//
// The only exception is **segmentation due to channel length limit**: if the channel reports
// it actually only carried the first K items, items from K+1 onward must stay in the next
// batch, not get marked successful together. Otherwise the truncated findings are neither
// in the message nor in the failed list — they just disappear.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap a single send to keep one slow channel from blocking the rest of this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The channel-reported count can never exceed the delivery count; if it does the
			// render layer is wrong, and treating it as fully delivered (and logging the issue)
			// is better than writing the records wrong.
			log.Printf("[notify] channel-reported delivered %d exceeds delivery count %d channel=%s; treating as all delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] mark delivered failed channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message has hit the channel's length cap: the rest are immediately re-queued
			// for the next tick to continue. Use DeferDeliveries rather than RescheduleDeliveries
			// — this isn't a failure, so it shouldn't consume the retry budget (claim optimistically
			// +1'd, and that gets decremented back there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message hit the channel length cap; only the first %d delivered, the rest will go in the next batch", delivered)); err != nil {
				log.Printf("[notify] enqueue segmented continuation failed channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// Channel neither errored nor said how many it delivered. Treat as failure (go through
		// backoff), so this delivery doesn't get repeatedly claimed yet never marked.
		err = fmt.Errorf("channel did not report delivered count (delivered=%d)", delivered)
	}

	// Failure handling is decided **per item**, not by using the batch's max attempts.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts` killing the whole batch,
	// but the items in a batch don't have the same attempt count: an old delivery that already
	// retried twice (attempts=2) would drag a brand-new delivery in the same batch (attempts=1)
	// into failed — a new finding gets permanently lost without ever using a retry, which is
	// exactly the opposite of "don't let old rows drag new rows down".
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
			log.Printf("[notify] mark failed state error channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] mark failed state error channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Reschedule grouped by delay: only 3 backoff tiers, so the group count is naturally
	// small — no need to send one UPDATE per item (that would turn a 500-item batch into
	// 500 round-trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] reschedule deliveries failed channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d exhausted=%d pending=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in `all` that are not in `keep` (compared by pointer identity).
// Used to find "didn't make it into the message" deliveries — they must be explicitly
// handled, not left in a gray zone.
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

// adapt gets the channel implementation and parses its config.
// ok=false means the type isn't registered; the delivery should be marked failed directly
// rather than infinitely retried.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On config parse failure give an empty map: the channel's own Validate will report
		// "which field is missing" — that error guides the user to fix the issue better than
		// a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single-finding message.
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

// renderBatch renders a digest message. Parse each snapshot individually — a single bad
// one only skips that item, without dragging the whole batch down.
//
// The included return value and msg.Items are **strictly one-to-one** (i-th delivery ↔
// i-th item). This correspondence is a hard requirement: the caller decides the first K
// deliveries to mark sent based on "the channel reports carrying the first K". If we skip
// a bad snapshot here but don't remove the skipped delivery from `included`, the indices
// get misaligned — the bad entries that should fail get marked sent, and the good ones
// get falsely judged undelivered. The bad ones are explicitly marked failed by the caller,
// see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A bad snapshot doesn't go into the message, nor into `included` — its handling
			// is the caller's responsibility (explicitly mark failed, not sneak into "delivered").
			log.Printf("[notify] skipped unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
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
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch are unparseable", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders the event snapshot into a push item, also resolving asset names and the detail link.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// An asset name resolution failure shouldn't block push: not getting a name is much
		// less bad than not getting the notification — the message will just be missing one line.
		log.Printf("[notify] resolve asset names failed finding=%d: %v", snap.FindingID, err)
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
		// See web/src/app/(main)/function/findings/detail/page.tsx for the detail page
		// route — it reads the finding id from the query param `id`.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most `want` tokens** from the channel's bucket and returns the
// actual number taken.
//
// One token = one message (one HTTP request). Realtime mode passes however many the caller
// wants; digest mode passes 1 since a whole batch of findings is one message.
//
// The bucket capacity is the channel's per-minute limit, refilled at a constant rate.
// ratePerMin<=0 means no rate limit, returns a finite but large-enough value to keep
// a single round's loop from being blocked by unbounded backlog.
//
// The `want` cap is necessary: without it you'd have to drain the whole bucket, and the
// caller also has its own per-round cap, so extra tokens are useless and vanish before
// the next refill — saved-up burst capacity would never be reachable, and even "this
// round has no pending deliveries" would still consume a token.
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
	// Refill based on real elapsed time, rate is ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: tokens are accumulated as floats, and refilling
	// in two steps like 0.5 + 0.5 can give 0.9999999999, which int() would truncate to 0 —
	// a bucket that is mathematically full yet can't yield a token. 1e-9 is far smaller than
	// a token, so it won't let a real shortfall through.
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

// publicBaseURL returns the external address for back-links, with the trailing slash trimmed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest interval, falling back to the default if invalid or unset.
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

// parseSnapshot parses the delivery's corresponding event snapshot.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("delivery %d has an empty event snapshot", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("parse delivery %d event snapshot failed: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event type follows the event's behavior; the snapshot's copy may have been
		// written by an older version.
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
