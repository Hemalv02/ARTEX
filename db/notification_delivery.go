package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file owns claiming of delivery tasks and the resulting state
// transitions.
//
// Claiming uses a "lease" rather than a long-lived transaction: the
// row is flipped to sending and next_attempt_at is pushed into the
// future as the lease's expiration, then the transaction commits and
// only after that does the network send happen. The point is to avoid
// holding a database lock during the send — network calls can take
// seconds (the channel HTTP client times out at 15), and holding a
// row lock that long would starve other writers on the same database.
//
// The trade-off: if a process crashes mid-send, the row stays in
// sending. This is **self-healing** — once the lease expires
// next_attempt_at falls into the past, the next claim pass picks the
// same row back up (see the state IN ('pending','sending') clause in
// the claim conditions). The retry counter is already incremented at
// claim time, so a crash does not lead to infinite retries — after
// MaxNotifyAttempts opportunities the row drops to failed for human
// review.

// MaxNotifyAttempts is the maximum number of attempts (including the
// first) for a single delivery.
// It is defined here rather than in the delivery engine: it is the
// state machine's own policy, and the engine is just the executor.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize caps how many deliveries a single digest batch
// can merge into one message.
//
// Why it exists: resource bounds. If a digest cycle scans tens of
// thousands of findings (entirely possible — one full scan can do it),
// without an upper bound the claim would read every row into memory,
// render one oversized message, and then have most of it truncated by
// the channel's length cap — wasting memory and **silently losing**
// the findings that got truncated. With the cap, the overflow stays
// in the database as the next batch, which the next cycle naturally
// sends out, so nothing is lost.
//
// The choice of 500 is calibrated: it is the magnitude that, after
// being rendered into a message, still leaves "readable content"
// within WeCom's 4096-byte limit; going larger just pushes the
// truncation point further down the message.
const MaxDigestBatchSize = 500

// NotificationDelivery is one delivery task, including the channel
// configuration and event snapshot needed for rendering.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// The render context loaded in via JOIN, excluded from JSON (the
	// server layer assembles the DTO).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind are propagated from the event so the
	// history list can deep-link straight into the finding detail.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind are redundant fields used for list
	// display, saving the front-end a second round-trip.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery is the unified read shape for delivery rows:
// delivery + event snapshot + channel config. All three are required
// to render a message, and splitting them would mean three round
// trips.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery describes one claim operation: first select candidate
// rows under FOR UPDATE, then flip them to sending and extend the
// lease. The lease's parameter position inside sel is up to the
// caller, which fills it with a $n placeholder and passes the value.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries claims up to limit due realtime deliveries
// for a given channel.
//
// Claiming is deliberately per-channel rather than "claim a global
// batch and then pick what to send": the rate-limit gate is maintained
// in the delivery engine per channel, so the engine must know how
// many sends this channel has left in this round **before** it
// claims the same number of rows — otherwise rate-limiting would eat
// into the retry budget. If the order were reversed, the rows that
// got rate-limited would already count as one attempt, the 3-attempt
// budget would be burned by pure waiting, and the row would end up
// in failed.
//
// The condition includes "sending with an expired lease" — that is
// the crash-recovery landing point. The lease must be substantially
// longer than the worst-case single delivery (the channel HTTP client
// times out at 15s), otherwise two dispatchers could deliver the
// same row concurrently. Disabled channels are also blocked: the
// disable operation has already marked in-flight deliveries as
// skipped, and this is a second line of defense against a
// disable-vs-claim race that could let one slip through.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue reports whether the channel has accumulated a
// due batch: there are pending deliveries, and **the oldest one** has
// aged past the digest period.
//
// The criterion is the oldest delivery's age, not the wall clock: a
// newly created channel should not immediately spit out a
// one-delivery "digest" just because it happens to align to the top
// of the hour, and a batch that has been backed up for a long time
// should not have to wait another full cycle either.
//
// DigestBatchDue is split from ClaimDigestBatch because the
// semantics differ: this function only answers "should we send",
// while the claim must take **every** pending delivery in the
// channel (including those that have not yet aged up) — otherwise
// one cycle would be split into multiple messages, and digest would
// lose its meaning.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch claims the channel's currently due pending
// deliveries as a single digest batch, capped at MaxDigestBatchSize
// per batch.
//
// All deliveries in the same batch share a batch_id, using the
// minimum id in the set as the batch number (stable, readable, no
// extra sequence needed). Retries use COALESCE to preserve the
// original batch id, so "these N items were sent together" remains
// true across retries.
//
// The claim takes the first N rows by id rather than at random: the
// earliest produced deliveries are sent out first, and there is no
// starvation of "new findings sent first, old findings stuck behind
// forever" under backlog.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit is the **memory upper bound**; the caller passes
	// MaxDigestBatchSize. We clamp here too, to defend against a
	// caller passing in a larger value.
	//
	// Deliberately does not accept a "rate-limit allowance" in
	// place of the batch size: rate limiting is denominated in
	// messages — one batch is one message, consuming one token,
	// deducted by takeTokens in the server layer — and that is a
	// different unit from "how many findings fit in one batch".
	// Once, to make rate_per_min apply to digest, the per-round
	// request budget was passed in as the batch size, which made
	// a rate=20/min channel put only 1 finding per batch, and
	// digest degenerated into realtime push with digest copy. If
	// you want to change rate limiting, change takeTokens's want;
	// do not touch this.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries runs "select + flip to sending + extend lease +
// load full rows" inside a single transaction. postClaim is an
// optional follow-up step (the digest batch uses it to write
// batch_id).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once commit succeeds

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Flip to sending and push next_attempt_at into the future: that
	// future moment is the lease's expiration, so "lease not
	// expired" and "not yet retry time" share the same condition —
	// no new column required.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent marks a batch of deliveries as sent.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries rolls a batch of deliveries back to pending
// and pushes out the next attempt time.
//
// Rolling back to pending rather than introducing a new intermediate
// state keeps "how many attempts are left" expressed in exactly one
// place (MaxNotifyAttempts) and prevents the state machine's
// branches from ballooning with the retry policy.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries rolls a batch of deliveries back to pending, makes
// them immediately re-claimable, and **undoes the attempt counted at
// claim time**.
//
// It has exactly one use: when a digest message is split across
// multiple sends because of the channel's length cap, the items that
// did not fit into this segment must be left for the next batch.
// That is not a failure, so it should not consume the retry budget
// — but attempts was already optimistically +1 at claim time, and
// must be decremented back here. Without this, a 500-item backlog
// would be cut into 25 segments of 20 each, and items in the tail
// would be marked failed by MaxNotifyAttempts on segment 3, even
// though they had never actually failed.
//
// GREATEST(...,0) protects against the case "someone manually
// re-sent and zeroed out attempts, then this path runs" — the
// counter must not go negative.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries marks a batch of deliveries as terminally failed,
// awaiting manual resend from the delivery history.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// Placeholders start from $3: $1 is state, $2 is last_error.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery manually resends one delivery: reset to
// pending, zero the retry counter, make it due immediately. Zeroing
// the counter is deliberate — a human clicking "resend" means the
// cause of the previous failure has been addressed, and there is no
// reason to carry the old count as a cap.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery %d does not exist or its current state does not allow resending", id)
	}
	return nil
}

// NotificationDeliveryFilter is the query condition for the
// delivery history.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries returns a page of the delivery history,
// newest first.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError truncates the error message to a length the
// column can hold. The response bodies a channel returns can be very
// long (especially when a generic webhook is hitting a self-hosted
// service), and leaving them uncut bloats the history list's
// payload.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// Back off to a character boundary so the front-end does not
	// see half a UTF-8 character rendered as a replacement glyph.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders produces a $n placeholder string starting at `start`
// along with the corresponding arguments, for use with IN (...).
// For example, start=3, ids=[7,8] yields "$3,$4" and [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
