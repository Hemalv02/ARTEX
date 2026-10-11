package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// This file is the channel config and event layer for IM push.
// The pickup and state transitions of delivery tasks are in
// db/notification_delivery.go.
//
// Two invariants, which must be kept when modifying this file:
//
//  1. The finding-write transaction (RecordFindingTx) only calls
//     InsertNotificationEventTx for a single blind insert, reading
//     no notification-related tables and doing no filter matching.
//     Any read operation introduced here could pollute or even abort
//     the finding-write transaction because of user-misconfigured
//     filter conditions.
//  2. Filter matching never errors: malformed config is always
//     treated as "match" (see notify.Match). Better to over-push
//     than to under-push.

// ErrNotificationChannelNotFound — the channel does not exist.
var ErrNotificationChannelNotFound = errors.New("notification channel does not exist")

// Delivery states.
const (
	NotifyStatePending = "pending" // waiting to be sent
	NotifyStateSending = "sending" // picked up by some dispatcher, lease not yet expired
	NotifyStateSent    = "sent"    // delivered
	NotifyStateFailed  = "failed"  // retries exhausted or permanent failure, can be manually resent
	NotifyStateSkipped = "skipped" // channel has been disabled, no longer sending
)

// Push modes.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode whitelist-validates the push mode (same idea as
// findings.status: no DB CHECK, to make future extension easy).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel is a channel instance's config. Config and
// Filter are kept as raw JSON; parsing is left to the notify package
// — the db layer does not understand the meaning of their fields.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled is a pointer so we can distinguish "the field was not
	// passed" from "explicitly false" — the front-end switch
	// control only submits the fields that were modified.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled reports whether the channel is enabled; when Enabled
// is nil (not loaded) it is treated as enabled.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is a single event fact.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels returns all channel instances, with
// enabled ones first, and otherwise sorted by id. Sorting is done
// in SQL so that the UI and the dispatcher see the same stable
// order.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID fetches a single channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel creates or updates a channel.
//
// When updating, only the fields the caller explicitly provided
// (non-nil / non-empty) are overwritten, so the front-end can
// submit a drawer form for a partial edit, without having to echo
// back the fields it doesn't display in the config — echoing back
// would cause the "masked value overwrites the real key" accident.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// Deliberately do **not** massage 0 in any way here: 0 is a
	// valid config, meaning "no rate limit".
	//
	// This used to be `if c.RatePerMin <= 0 { c.RatePerMin = default }`,
	// the intent being "give a safe default when not specified",
	// but that also swallowed "explicitly set to 0" — docs, UI
	// hints, and takeTokens all interpret 0 as no rate limit, but
	// only here it was silently changed to 20 (DingTalk/WeCom/Telegram)
	// or 100 (Feishu), so operators thought they had removed the
	// rate limit while in fact they were being capped at 20/min
	// with no notice.
	//
	// The difference between "not specified" and "explicit 0" is
	// only known to the caller (field absent from the request
	// body vs. explicit 0), so the default is filled in by the
	// server layer when the field is absent, see
	// notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("rate limit value cannot be negative")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled toggles enabled/disabled.
//
// When a channel is disabled, its not-yet-sent deliveries are
// marked as skipped in the same transaction: otherwise re-enabling
// it would suddenly result in a batch of "stale-during-disabled"
// old findings, which have lost timeliness and are easily
// misjudged as new.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "channel has been disabled", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel deletes a channel. Its delivery history
// is cascade-deleted with the foreign key (with the config gone,
// the history cannot be interpreted).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx **best-effort** writes a push event
// inside the caller's transaction.
//
// This is the only notification-related change on the finding-write
// path: a single INSERT, reading no tables, knowing nothing about
// channels, running no filter. Transaction commit guarantees
// atomicity between "finding persisted" and "push task exists", so
// there is no window where the commit succeeds but the queue is
// missed and the message is permanently lost.
//
// Two key design choices, neither off-the-cuff:
//
//  1. **Why use SAVEPOINT**: in PostgreSQL, an error on any
//     statement within a transaction puts the whole transaction
//     into an aborted state, after which all subsequent statements
//     (including COMMIT) fail. So "ignore this INSERT's error and
//     let the caller continue committing" cannot be done in PG —
//     unless a savepoint isolates the error to this single
//     statement. Without a savepoint, the only option is "roll the
//     whole thing back".
//
//  2. **Why rolling back the whole thing is wrong**: push is a
//     convenience feature; the finding record is the product
//     itself. A problem with a notification table (an unmigrated
//     old database, a transient disk fault) should not prevent a
//     high-severity finding from being saved. So we isolate the
//     error, log it, and return false, letting the finding write
//     commit as usual — at the cost of losing this single push.
//     Returning bool rather than error is deliberate: the caller
//     should not treat it as an error that affects the success of
//     the write.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] failed to serialize push event finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] failed to create savepoint finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] failed to write push event finding=%d (finding record is not affected): %v", findingID, err)
		// Roll back to the savepoint to rescue the transaction
		// from the aborted state.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] failed to roll back to savepoint finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release the savepoint, to avoid piling up useless savepoints
	// in long transactions.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is the standalone-transaction version of
// InsertNotificationEventTx, for call sites not already in a
// transaction (such as the channel's "send test message", which
// has no real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("failed to serialize notification event snapshot: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands not-yet-fanned-out finding events
// into delivery tasks per the currently enabled channels, returning
// the number of events processed in this round and the number of
// deliveries created.
//
// The whole round runs in a single transaction: events are picked
// up with FOR UPDATE SKIP LOCKED, so multiple processes running
// concurrently each pick up a different set of rows (the project's
// archive queue uses the same technique for its pickup, see
// completeNextArchiveJob in db/task_archives.go).
//
// Filter matching is deliberately done on the Go side rather than
// in SQL: the channel filter is a JSONB of optional fields, and
// expressing six combinations of matches in SQL makes the query
// hard to maintain; the number of channels is "a few manually
// configured", and loading them all and comparing in memory is
// faster and easier to test.
//
// Events that don't match any channel are also marked
// fanned_out — otherwise they would stay in the to-be-fanned-out
// set forever, and be re-scanned every tick.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// The snapshot is written by us, so in theory it must be
		// parseable; a parse failure does not block the delivery
		// flow, but the event will be skipped by all channels
		// with filter conditions because all its fields are
		// empty — better to push one less than to let a bad row
		// stall the entire queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// The kind takes the in-row value as authoritative: the
		// copy in the snapshot is for rendering, and may have
		// been written by an older version.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// Mark this round's events as fanned out. Events that didn't
	// match any channel are marked together (see the function
	// comment).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx fetches the enabled channels
// inside a transaction. The count is small, no pagination or cache
// — caching would introduce an extra "when does a config change
// take effect" timing problem.
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames resolves asset ids into short display
// names, for use in push messages.
//
// The returned order matches the input order, and the length may be
// less than the input (ids that don't exist are skipped). Preserving
// the input order is so that the assets appear in a stable order
// across multiple deliveries of the same finding — otherwise the
// order in a retried message would change and be misread as
// "assets changed".
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName picks the most identifiable representation
// based on the asset type.
// The fallback is an empty string, with the caller deciding how to
// present "an asset whose name could not be retrieved" — this
// function does not invent a placeholder, otherwise noise like
// "Asset #42" would slip into push messages and readers would
// mistake it for a real domain.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify updates a finding's handling status
// and registers a status-change push event in the same transaction.
//
// Returns from=the pre-change status; found=whether the finding
// exists; notified=whether the event was registered successfully.
//
// Three deliberate behaviors:
//   - An event is not registered when the status did not actually
//     change. The front-end drawer resubmitting the same value, or
//     an automation script idempotently replaying, should not
//     produce push noise.
//   - When the finding does not exist, found=false is returned and
//     no writes are performed; the caller translates it to 404.
//   - Event registration failure does not affect the status update
//     (see the savepoint note in RecordNotificationEventTx), so
//     when notified=false the status has already been changed
//     successfully, and the caller should not report an error
//     because of that.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx updates a finding's status and registers a
// status-change push event within the **caller's transaction**.
//
// Extracting this as a transaction-level function is to make all
// status-changing paths share the same semantics — previously
// only patchFinding went through the version that notifies, while
// the `UPDATE findings SET status=...` in finding_retests for
// **"retest conclusion is Fixed"** went directly to the database,
// so channels configured with `on_status_change` would receive no
// push for these status transitions: the status changed silently
// in the UI, and operations would not notice until they opened
// the platform.
//
// Returns from=pre-change status, found=whether the finding
// exists, changed=whether the status actually changed,
// notified=whether the event was registered successfully
// (registration failure does not affect the status update, see
// RecordNotificationEventTx).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// An event is not registered when the status did not
		// actually change: repeated submission of the same
		// value, idempotent replay, must not produce push
		// noise.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats is the summary count at the top of the
// notification page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // age in milliseconds of the oldest pending delivery
}

// NotificationStatsSnapshot summarizes the health of the
// notification system.
// BacklogAgeMS is the most direct indicator of "is push stuck" —
// far more useful than the pending count, because the difference
// between 3 items in backlog and 3 items in backlog can be from
// 3 seconds to 3 hours.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
