package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일의 테스트는 실제 PostgreSQL에 연결한다(DB가 없으면 건너뜀). SQL에
// FOR UPDATE SKIP LOCKED, make_interval, JSONB, 다중 행 IN(...) 자리표시자 조합을 사용하며
// 컴파일되어도 실행 시 실패할 수 있으므로 실제 실행해야 검증된다.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel은 테스트 종료 시 자동 삭제되는 채널을 만든다.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "테스트 채널-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("채널 생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent는 분배와 전송 테스트용으로 finding 없이 이벤트를 직접 저장한다.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("이벤트 저장 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 세 자산 유형은 각각 도메인, IP, URL을 표시한다.
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

	// 입력 순서를 의도적으로 섞고 존재하지 않는 ID도 포함한다.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("자산 이름 해석 실패: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("자산 이름 개수 불일치, 기대 %v 실제 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("순서/값 불일치, 기대 %v 실제 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure는 저장점의 핵심을 검증한다.
// 트랜잭션에서 항상 false인 임시 제약으로 notification_events 저장을 강제 실패시키고
// 함수가 false를 반환하며 트랜잭션은 aborted가 되지 않아 이후 문장을 실행할 수 있는지 확인한다.
//
// 저장점이 없으면 PostgreSQL이 전체 트랜잭션을 무효화하여 이후 모든 문장이
// current transaction is aborted로 실패한다. 이것이 알림 테이블 문제로
// 취약점을 저장하지 못하는 장애 경로다.
//
// COMMIT 대신 ROLLBACK으로 끝낸다. PG의 ALTER TABLE은 트랜잭션에 포함되므로
// 커밋하면 임시 제약이 영구 저장되어 후속 테스트를 모두 실패시킨다.
// 롤백이 DDL을 자동 취소하므로 수동 정리가 필요 없다. 트랜잭션이 유효한지만 확인하며
// 실제 커밋은 필요 없다.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 이전 실행이 제약을 남겼을 경우에 대비해 먼저 제거한다.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 임시 제약 취소, 함수 주석 참고

	// NOT VALID는 이후 쓰는 행에만 적용하고 기존 이벤트는 검증하지 않는다.
	// 기존 행 위반 때문에 제약 추가가 실패하는 것을 방지한다.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("임시 제약 추가 실패: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("반드시 실패하는 제약에서 저장 성공을 보고함")
	}
	// 핵심 검증: 트랜잭션이 여전히 유효하다.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("트랜잭션이 오염됨(저장점 미작동): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("롤백 실패: %v", err)
	}
	// 후속 테스트에 영향을 주지 않도록 롤백으로 DDL이 취소되었는지 확인한다.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("임시 제약이 롤백되지 않아 후속 테스트를 오염시킬 수 있음")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL 인젝션"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"전체 채널이 high 수신", highSQL, all.ID, true},
		{"전체 채널이 low 수신", lowXSS, all.ID, true},
		{"치명적 전용 채널이 high 생략", highSQL, onlyCritical.ID, false},
		{"치명적 전용 채널이 critical 수신", criticalXSS, onlyCritical.ID, true},
		{"SQL 전용 채널이 SQL 수신", highSQL, sqlOnly.ID, true},
		{"SQL 전용 채널이 XSS 생략", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("전송 존재 여부: 기대 %v 실제 %v", tc.want, exists)
			}
		})
	}

	// 재분배해도 전송이 중복 생성되면 안 된다(fanned_out 멱등성).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("분배한 이벤트를 재처리하면 안 됨, 실제 events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel은 어느 채널과도 일치하지 않는 이벤트를 검증한다.
// 이 경우도 분배 완료로 표시해야 대기 집합에 남아 tick마다 재검색되지 않는다.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["절대 일치하지 않는 유형"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("전송이 생성되면 안 됨, 실제 %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("채널 불일치 이벤트도 분배 완료로 표시해야 무한 재검색을 막을 수 있음")
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

	// 실시간 획득은 realtime 채널만 가져오고 digest 채널은 건드리지 않아야 한다.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("획득 실패: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1개 획득 기대, 실제 %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("획득 후 sending, attempts=1 기대, 실제 state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// 채널 설정, 이벤트 스냅샷, finding ID 등 렌더링 컨텍스트가 모두 있어야 한다.
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("획득 결과에 채널 설정이 없어 렌더링 실패 가능")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("이벤트의 finding ID가 전달되지 않음, 실제 %d", got[0].FindingID)
	}

	// 임대가 끝나기 전 두 번째 획득은 비어 있어야 한다. 같은 행을 두 dispatcher가
	// 동시에 전송하지 않음을 보장한다.
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("임대 중 중복 획득하면 안 됨, 실제 %d개", len(again))
	}

	// 실시간 획득은 digest 채널 전송을 가져오면 안 된다.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("실시간 획득에 digest 전송 포함, 실제 %d개", len(left))
	}
}

// TestClaimExpiredLeaseRecovers는 전송 중 프로세스 종료로 남은 sending 행이
// 임대 만료 후 다시 획득되어 영구 정체되지 않는지 검증한다.
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
		t.Fatalf("첫 획득 실패: %v (%d개)", err, len(first))
	}
	// 임대를 과거로 옮겨 만료 상태를 재현한다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("임대 만료 sending 행은 재획득할 수 있어야 함, 실제 %d개", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("재획득 시 시도 수가 증가해야 함, 실제 %d", second[0].Attempts)
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
	// 비활성화는 기존 대기 전송도 skipped로 표시한다.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("비활성 채널의 대기 전송은 skipped여야 함, 실제 %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("비활성 채널은 획득할 수 없어야 함, 실제 %d개", len(got))
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

	// 방금 생성된 배치는 경과 시간이 0이므로 30분 주기에 도달하지 않았다.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("배치 기한 판정 실패: %v", err)
	}
	if due {
		t.Fatal("새 배치는 즉시 기한이 되면 안 됨")
	}

	// 세 전송의 생성 시각을 과거로 옮겨 주기가 지난 배치를 재현한다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("주기가 지난 배치는 기한 도달로 판정해야 함")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("요약 배치 획득 실패: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("요약은 세 개 모두 한 번에 획득해야 함, 실제 %d개", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("함께 전송한 항목을 기록에서 식별하도록 요약 배치에 batch_id가 필요함")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("같은 배치는 batch_id를 공유해야 함, 실제 %v vs %d", dl.BatchID, firstBatchID)
		}
	}

	// 배치 전체 실패 후 재배치·재획득해도 COALESCE로 기존 batch_id를 유지해야 한다.
	// 재시도로 함께 전송했다는 사실이 사라지면 안 된다.
	//
	// 전송 엔진처럼 하나가 아닌 전체 배치를 재배치해야 한다.
	// 메시지 하나가 배치 전체의 성공/실패를 대표한다. 하나만 재배치하면 나머지는 임대 중이므로
	// 재획득 시 그 하나만 반환되는 것이 정상이다.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "모의 실패"); err != nil {
		t.Fatal(err)
	}
	// 임대를 과거로 옮겨 백오프 종료를 재현한다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("재획득은 세 개 모두 반환해야 함, 실제 %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("재시도 후 batch_id는 기존 %d여야 함, 실제 %v", firstBatchID, reclaimed[0].BatchID)
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
		t.Fatalf("획득 실패: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "일시적 네트워크 불안정"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "일시적 네트워크 불안정" {
		t.Fatalf("재배치 후 pending과 사유 기록 기대, 실제 state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "재시도 소진"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed 기대, 실제 %s", state)
	}

	// 수동 재전송은 기존 실패 예산을 물려받지 않도록 시도 수를 초기화하고 즉시 기한을 설정한다.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("재전송 실패: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("재전송 후 pending, attempts=0 기대, 실제 state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("재전송은 즉시 획득 가능해야 함")
	}

	// 전송 완료 항목은 재전송할 수 없어야 한다.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("전송 완료 항목은 재전송을 허용하면 안 됨")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "페이지 나누기 테스트"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if total != 5 {
		t.Fatalf("총 5개 기대, 실제 %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("페이지당 2개 기대, 실제 %d", len(page1))
	}
	// 최신순이므로 첫 페이지 첫 ID가 두 번째 페이지 첫 ID보다 커야 한다.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("최신순 페이지 정렬 기대, 실제 page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// 무엇을 전송했는지 표시할 수 있도록 기록에 렌더링 컨텍스트를 포함한다.
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("기록 항목에 표시 필드 누락: %+v", page1[0])
	}

	// 상태 필터: pending은 없다.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending 전송이 없어야 함, 실제 %d개(total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("알림 상태 변경 테스트", "목표", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 인젝션", Name: "상태 변경 사례",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 저장 시 생성된 finding_created 이벤트 수를 기준값으로 기록한다.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("취약점 저장과 같은 트랜잭션에 알림 이벤트를 등록해야 함")
	}

	// 같은 상태 설정은 이벤트를 만들지 않아야 반복 제출 알림을 방지한다.
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("상태가 같으면 알림 이벤트를 등록하면 안 됨")
	}
	if from != "pending" {
		t.Fatalf("이전 상태 pending 반환 기대, 실제 %q", from)
	}

	// 실제 변경은 이벤트와 from/to를 기록해야 한다.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("실제 상태 변경 시 알림 이벤트를 등록해야 함")
	}
	if from != "pending" {
		t.Fatalf("from은 pending이어야 함, 실제 %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("상태 변경 이벤트 없음: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("스냅샷 상태 전환 오류: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 상태 변경 메시지가 비지 않도록 렌더링 필드를 스냅샷에 포함한다.
	if snap.VulnClass != "SQL 인젝션" || snap.Severity != "high" || snap.Name != "상태 변경 사례" {
		t.Fatalf("스냅샷 렌더링 필드 누락: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("상태가 fixed로 갱신되어야 함, 실제 %s", status)
	}

	// 없는 취약점은 오류 없이 found=false.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("없는 취약점은 found=false와 오류 없음 기대, 실제 found=%v err=%v", found, err)
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
		t.Fatalf("집계 실패: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("채널 개수 오류: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("대기 전송이 집계되어야 함: %+v", stats)
	}
	// 새 전송의 대기 시간은 음수나 큰 값 대신 0에 가까워야 한다.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("잘못된 대기 시간: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 왕복",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("읽기 실패: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 왕복" {
		t.Fatalf("저장·조회 필드 불일치: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("기본적으로 활성 상태여야 함")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("설정이 올바르게 저장되지 않음: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("필터가 올바르게 저장되지 않음: %+v", filter)
	}

	// 갱신 후 다시 읽는다.
	got.Name = "변경된 이름"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "변경된 이름" || after.IsEnabled() {
		t.Fatalf("갱신이 적용되지 않음: %+v", after)
	}

	// 삭제 후에는 조용한 성공 대신 없음 오류를 반환해야 한다.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("ErrNotificationChannelNotFound 기대, 실제 %v", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("중복 삭제는 없음 오류를 반환해야 함, 실제 %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate는 과거 오류의 재발을 막는다.
// 0은 무제한을 뜻하는 유효한 값이며 DB 계층이 미설정으로 해석하여 기본값으로 바꾸면 안 된다.
//
// 이전 SaveNotificationChannel의 if RatePerMin <= 0 { 기본값 } 때문에
// 문서, UI, takeTokens는 0을 무제한으로 해석하지만 저장 계층만
// DingTalk/WeCom/Telegram은 20, Feishu는 100으로 바꿨다. 사용자는 제한을 풀었다고 생각해도
// 안내 없이 제한되었다. 미설정과 명시적 0은 요청 본문만 구분할 수 있으므로
// 기본값은 서버 notifyCreateChannel에서 채우고 DB는 저장만 담당한다.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 명시적 0(무제한)은 그대로 저장해야 한다.
	unlimited := &NotificationChannel{
		Name: "무제한", Kind: notify.KindDingTalk, RatePerMin: 0,
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
		t.Fatalf("명시적 0은 무제한이므로 그대로 저장해야 함, 실제 %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("기본 모드는 realtime이어야 함, 실제 %s", got.Mode)
	}

	// 음수는 다른 값으로 바꾸지 않고 잘못된 입력으로 거부한다.
	bad := &NotificationChannel{
		Name: "음수 제한", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("음수 제한을 거부해야 함")
	}
}

// TestDeleteChannelCascadesDeliveries는 채널 삭제 시 기록도 삭제되는 외래 키 동작을 검증한다.
// 설정 없이 기록을 해석할 수 없지만 이벤트 자체는 다른 채널이 참조할 수 있으므로 유지한다.
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
		t.Fatal("사전 조건 불충족: 전송이 생성되지 않음")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("채널 삭제 시 전송도 연쇄 삭제해야 함, %d개 남음", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("채널 삭제 시 이벤트 자체까지 삭제하면 안 됨")
	}
}

// TestClaimDigestBatchHonorsCallerLimit는 감사에서 발견한 제한 누락을 검증한다.
// 이전 요약 채널은 takeTokens가 차감한 allow를 사용하지 않아 토큰 버킷을 우회했고
// rate_per_min이 digest에 영향을 주지 않았다. 이제 limit도 제한에 참여한다.
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
	// limit=3이면 세 개만 획득하고 나머지는 DB에 남긴다.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("획득 실패: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("호출자 제한에 따라 세 개만 획득해야 함, 실제 %d", len(got))
	}
	// limit=0은 이번 예산 소진으로 오류 없이 아무것도 획득하지 않는다.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("예산 0이면 오류 없이 0개 획득 기대, 실제 %d개 err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange는 감사에서 발견한 완전성 누락을 검증한다.
// 재검증이 수정됨으로 결론 나면 직접 UPDATE로 상태는 바뀌었지만
// 알림 포함 버전을 우회하여 on_status_change 채널이 이 전환을
// 받지 못했다. 화면 상태만 조용히 바뀌어 운영자가 플랫폼을 열어야 알 수 있었다.
//
// 모든 상태 변경 경로가 이벤트를 등록해야 한다는 조건을 보장한다.
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("재검증 알림 테스트", "목표", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 인젝션", Name: "재검증 대상",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 재검증 기록을 만들고 완료 상태까지 진행한다.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "재확인")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("재검증에 세션이 연결되어야 함")
	}
	// 실제 흐름처럼 running에 진입한 뒤 결론을 저장해야 한다.
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("재검증 시작 실패: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "수정됨", "증거"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("재검증 종료 실패: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("수정됨 판정 후 상태는 fixed여야 함, 실제 %s", status)
	}

	// 핵심 검증: 올바른 from/to를 가진 상태 변경 이벤트가 있어야 한다.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("수정됨 재검증은 on_status_change 채널을 위해 상태 변경 알림 이벤트를 등록해야 함: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("스냅샷 상태 전환 오류: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 빈 알림을 방지하도록 스냅샷에 렌더링 필드를 포함한다.
	if snap.Name != "재검증 대상" || snap.Severity != "high" {
		t.Fatalf("스냅샷 렌더링 필드 누락: %+v", snap)
	}
}
