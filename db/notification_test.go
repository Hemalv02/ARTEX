package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// The cases in this file all connect to a real PostgreSQL (skipped when no database is available).
// These SQL statements use FOR UPDATE SKIP LOCKED, make_interval, JSONB, and multi-row IN(...)
// placeholder stitching — all "compiles fine but may fail at runtime" patterns, so they must actually run to count as verified.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel creates a channel that is automatically deleted when the test ends.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "测试渠道-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("create channel failed: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent writes an event directly (bypassing a finding), for testing fan-out and delivery.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("write event failed: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// The three asset types each have their own display form: domain, IP, URL.
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// Input order is deliberately shuffled and includes one nonexistent id.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("resolve asset names failed: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("asset name count mismatch, want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/value mismatch, want %v got %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure is the core case for the savepoint mechanism:
// inside a transaction it first forces the notification_events write to always fail (by temporarily
// adding an always-false constraint), then asserts (1) the function reports false and (2) the transaction has not entered the aborted state, so later statements can still run.
//
// Without a savepoint, PostgreSQL would void the whole transaction and any later statement would
// fail with "current transaction is aborted" — exactly the failure path where "a problem in a
// notification table prevents a finding from being stored".
//
// This deliberately ends with **ROLLBACK rather than COMMIT**: ALTER TABLE is transactional in PG,
// and once committed that temporary constraint would stay in the schema forever and break every later case.
// Rollback undoes the DDL automatically, with no manual cleanup. The assertion only needs "the
// transaction is still alive", not an actual commit.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// Defensive cleanup: if an earlier run left this constraint behind, drop it first.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // undoes the temporary constraint, see the function comment

	// NOT VALID: constrains only rows written from now on, without validating existing historical
	// events in the database (otherwise pre-existing invalid rows would block adding the constraint).
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("add temporary constraint failed: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("reported a successful write under an always-failing constraint")
	}
	// Key assertion: the transaction is still usable.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("transaction was poisoned (savepoint did not take effect): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	// Confirm the DDL was undone by the rollback, leaving no landmine for later cases.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("temporary constraint was not undone by the rollback and would pollute later cases")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL注入"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("fan-out failed: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"catch-all channel receives high", highSQL, all.ID, true},
		{"catch-all channel receives low", lowXSS, all.ID, true},
		{"critical-only channel skips high", highSQL, onlyCritical.ID, false},
		{"critical-only channel receives critical", criticalXSS, onlyCritical.ID, true},
		{"SQL-only channel receives SQL", highSQL, sqlOnly.ID, true},
		{"SQL-only channel skips XSS", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("delivery existence: want %v got %v", tc.want, exists)
			}
		})
	}

	// Fanning out again should not produce duplicate deliveries (fanned_out is idempotent).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("already fanned-out events should not be processed again, got events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel covers the "event matches no channel" case.
// Such events must still be marked as fanned out, otherwise they would stay in the pending set forever and be rescanned every tick.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["绝不匹配的类型"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("should produce no deliveries, got %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("an event matching no channel must still be marked fanned out, otherwise it would be rescanned forever")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// A realtime claim should pick up only the realtime channel's delivery, not touch the digest channel's.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("should claim 1, got %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("after claim should be sending with attempts=1, got state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// The eagerly loaded render context must be complete (channel config + event snapshot + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("claim result is missing channel config; rendering would fail")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id was not carried over from the event, got %d", got[0].FindingID)
	}

	// The lease has not expired, so a second claim should be empty — this is the guarantee that
	// "the same row is never delivered by two dispatchers at once".
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("should not re-claim within the lease, got %d", len(again))
	}

	// A digest channel's deliveries should not be touched by a realtime claim.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a realtime claim should not pick up a digest channel's deliveries, got %d", len(left))
	}
}

// TestClaimExpiredLeaseRecovers covers crash self-healing: a process dying mid-delivery leaves a
// sending row, which must be re-claimable once the lease expires, otherwise this delivery is stuck forever.
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim failed: %v (%d)", err, len(first))
	}
	// Manually push the lease into the past to simulate "lease expired".
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("a sending row with an expired lease should be re-claimable, got %d", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("re-claiming should increment the attempt count, got %d", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// Disabling also marks existing pending deliveries as skipped.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("a disabled channel's existing pending deliveries should be marked skipped, got %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a disabled channel should not be claimable, got %d", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// The batch was just created with age 0, so under a 30-minute cycle it should not be due.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("checking batch due failed: %v", err)
	}
	if due {
		t.Fatal("a just-created batch should not be due immediately")
	}

	// Age the creation time of all three deliveries together to simulate a batch that has accumulated a full cycle.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("a batch past its cycle should be judged due")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("claim digest batch failed: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("a digest should claim all 3 at once, got %d", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("a digest batch must write batch_id, otherwise history cannot tell these were sent together")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("the same batch should share batch_id, got %v vs %d", dl.BatchID, firstBatchID)
		}
	}

	// Reschedule this batch **as a whole** and claim it again; batch_id must keep its original value
	// (this is what COALESCE is for): otherwise a single retry would erase the fact that "this batch was sent together".
	//
	// The whole batch must be rescheduled, not just one row — this is how the delivery engine handles
	// sending a digest message (one message represents the whole batch, sharing its success or failure). If
	// only one row were rescheduled, the rest would still be within their lease, so a re-claim picks up only that one.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "模拟失败"); err != nil {
		t.Fatal(err)
	}
	// Push the lease into the past to simulate the backoff time having elapsed.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("re-claim should get all 3, got %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("after retry batch_id should keep its original value %d, got %v", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim failed: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "网络抖动"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "网络抖动" {
		t.Fatalf("after reschedule should be pending and record the reason, got state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "重试耗尽"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("should be failed, got %s", state)
	}

	// A manual resend must zero the retry count and become due immediately, otherwise it would inherit the old failure budget.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("resend failed: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("after resend should be pending with attempts=0, got state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("a resend should be immediately claimable")
	}

	// A delivered delivery should not be resendable.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("a delivered delivery should not be allowed to resend")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "分页测试"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 5 {
		t.Fatalf("total should be 5, got %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("2 per page, got %d", len(page1))
	}
	// Newest first: the first row of page 1 should have a larger id than the first of page 2.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("paging order should be newest first, got page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// The render context must be returned along with the history, otherwise the list cannot show "what was pushed".
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("history item is missing display fields: %+v", page1[0])
	}

	// Filter by state: there are no pending ones.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("should have no pending deliveries, got %d (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("通知状态变更测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "状态变更用例",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// A finding_created event was already registered when it was stored; count it first as a baseline.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("storing a finding should register a notification event in the same transaction")
	}

	// Setting the same status: should produce no event (avoids duplicate submissions spamming notification noise).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("set status failed: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("no notification event should be registered when the status does not change")
	}
	if from != "pending" {
		t.Fatalf("should return the prior status pending, got %q", from)
	}

	// A real change: should register an event and record from/to.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("set status failed: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("a notification event should be registered when the status actually changes")
	}
	if from != "pending" {
		t.Fatalf("from should be pending, got %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("status-change event not found: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("the status transition in the snapshot is wrong: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// The snapshot must carry the fields needed for rendering, otherwise the status-change message would be an empty shell.
	if snap.VulnClass != "SQL注入" || snap.Severity != "high" || snap.Name != "状态变更用例" {
		t.Fatalf("snapshot is missing render fields: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("status should have been updated to fixed, got %s", status)
	}

	// Nonexistent finding: found=false, no error.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("a nonexistent finding should return found=false with no error, got found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("stats failed: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("channel count is wrong: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("should count pending deliveries: %+v", stats)
	}
	// A just-created delivery's backlog age should be close to 0, not negative or huge.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("backlog age is invalid: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 往返",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 往返" {
		t.Fatalf("round-trip fields inconsistent: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("should be enabled by default")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("config was not stored correctly: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("filter was not stored correctly: %+v", filter)
	}

	// Read again after updating.
	got.Name = "改名了"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "改名了" || after.IsEnabled() {
		t.Fatalf("update did not take effect: %+v", after)
	}

	// After deletion it should report "not found" rather than silently succeed.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("want ErrNotificationChannelNotFound, got %v", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("a repeated delete should report not found, got %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate locks down a spot that was once written wrong:
// **0 is a valid config meaning "no rate limit", and the db layer must not treat it as "unspecified" and overwrite it with a default**.
//
// Historical bug: SaveNotificationChannel contained `if RatePerMin <= 0 { use default }`, so the
// docs, UI hints, and takeTokens all interpreted "0 = no rate limit", yet the storage layer alone
// quietly changed it to 20 (DingTalk/WeCom/Telegram) or 100 (Feishu) — the operator thought the rate
// limit was lifted but was actually throttled, with no hint at all. The difference between "unspecified"
// and "explicit 0" can only be expressed by the request body, so the default is filled at the server layer (see notifyCreateChannel) and the db layer only stores.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// Explicit 0 (no rate limit): must be stored as-is.
	unlimited := &NotificationChannel{
		Name: "不限流", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("explicit 0 means no rate limit and must be saved as-is, got %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("default mode should be realtime, got %s", got.Mode)
	}

	// A negative value is invalid input and should be rejected, not quietly changed to something else.
	bad := &NotificationChannel{
		Name: "负限流", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("a negative rate limit should be rejected")
	}
}

// TestDeleteChannelCascadesDeliveries locks down the foreign-key behavior: when a channel is deleted
// its delivery history disappears with it (the config is gone, so the history is uninterpretable), but the event itself must remain — it may still be referenced by another channel.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("precondition not met: no deliveries were produced")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("a channel's deliveries should cascade-delete when the channel is deleted, still %d left", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("deleting a channel should not also delete the event itself")
	}
}

// TestClaimDigestBatchHonorsCallerLimit covers a gap the audit pointed out: digest channels
// previously bypassed the token bucket entirely — allow was deducted by takeTokens but never used, so
// rate_per_min had no effect on digest mode. Now limit participates in the constraint too.
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// Take limit=3: only 3 can be claimed, the rest stay in the database.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("should claim only 3 per the caller's rate limit, got %d", len(got))
	}
	// limit=0 means this round's budget is exhausted: none should be claimed and no error should occur.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("with a 0 budget should claim 0 with no error, got %d err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange covers an integrity gap the audit pointed out: when a
// retest concludes "fixed" the status does change, but that UPDATE wrote to the database directly,
// bypassing the notification-aware version — so a channel configured with on_status_change received
// no notification at all for this transition, the status changed quietly in the UI; operators had to open the platform to find out.
//
// This case locks down "every path that changes status must register a status-change event".
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("复测推送测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "复测目标",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// Create a retest record and push it straight to the completed state.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "复核")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("a retest should be associated with a conversation")
	}
	// A retest must enter running before a conclusion can be recorded (matching the real flow).
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("starting retest failed: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "已修复", "证据"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("finishing retest failed: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("after a retest concludes fixed the status should be fixed, got %s", status)
	}

	// Key assertion: there must be a status-change event with correct from/to.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("a retest concluding fixed should register a status-change notification event (otherwise channels configured with on_status_change receive nothing): %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("the status transition in the snapshot is wrong: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// The snapshot must carry the fields needed for rendering, otherwise the pushed message would be an empty shell.
	if snap.Name != "复测目标" || snap.Severity != "high" {
		t.Fatalf("snapshot is missing render fields: %+v", snap)
	}
}
