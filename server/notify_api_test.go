package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 취약점 저장→이벤트→분배→실제 HTTP 전송의 E2E 동작 테스트.
//
// 전역 Notifier.step() 대신 직접 만든 채널의 stepRealtime/stepDigest만 호출한다.
// step()은 DB의 모든 활성 채널을 순회하므로 실제 DingTalk/WeCom 봇이 설정된
// 개발 DB에서 실행하면 테스트 중 생성된 취약점을 실제 그룹으로 보낼 수 있다.
// 채널별 호출로 영향 범위를 테스트용 가짜 수신 서버에 한정한다.
//
// 종료 시 테스트 이벤트(전송 연쇄 삭제)와 채널을 지워 실제 채널에 누적 항목을 남기지 않는다.
//
// stepRealtime/stepDigest는 반환값 없이 내부 로그를 쓰므로 테스트는
// 가짜 서버 수신 내용과 전송 행 상태 등 관찰 가능한 외부 동작을 검증한다.
// 반환값 모킹보다 실제 호출 경로에 가깝다.

// notifyFixture는 이 파일의 공통 테스트 장치다.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 직접 만든 task/exploration에 취약점을 기록하여 다른 테스트와 격리한다.
	taskID int64
	expID  int64
	// cleanupMark 이후 생성한 이벤트를 정리 시 함께 삭제한다.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 가짜 수신 서버는 127.0.0.1에 있지만 기본 전송 보호는 루프백을 차단한다
	// (SSRF로 로컬 서비스/클라우드 메타데이터 접근 방지). 테스트에서 명시적으로 허용하며
	// 기본 거부는 notify의 ssrf_test.go가 검증한다.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// 공유 trafficEvidenceServer의 작업은 exploration ID를 얻을 수 없으므로 직접 생성한다.
	// 취약점 기록에는 이 ID가 필요하다.
	task, err := s.m.CreateTask("알림 전송 테스트", "전송 동작 검증", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// fixture 생성 전 이벤트는 모두 분배 완료로 표시하여 독립적인 테스트를 만든다.
	//
	// FanOutPendingEvents는 전역 함수로 모든 미분배 이벤트를 일치 채널에 확장한다.
	// 공유 trafficEvidenceServer도 반환하는 초기 취약점 하나를 기록하고
	// 다른 사례의 잔여 데이터도 있을 수 있다. 격리하지 않으면 이런 이벤트가
	// 테스트 채널에 분배되어 전송 개수 검증이 실행 순서에 따라
	// 간헐적으로 실패하므로 원인을 찾기 더 어렵다.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("알림 이벤트 정리 실패: %v", err)
		}
	})
	// 다른 테스트가 껐을 수 있으므로 전체 설정을 활성화한다.
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record는 실제 증거 저장 경로로 취약점을 기록하고 finding ID를 반환한다.
// 같은 트랜잭션에 알림 이벤트가 등록되는 이 경로가 기능 연결 지점이다.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 요약",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("취약점 기록 실패: %v", err)
	}
	return out.FindingID
}

// channel은 채널별 stepX 호출용 설정을 읽는다.
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("채널 읽기 실패: %v", err)
	}
	return ch
}

// deliver는 이벤트를 분배하고 지정 채널만 한 회차 전송한다.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel은 HTTP로 채널을 생성하며 API 자체 검증도 확인한다.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("채널 생성 실패 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("채널 생성 응답 오류: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook은 수신 본문을 기록하는 가짜 수신 서버다.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("가짜 서버 수신 요청이 %d개여서 %d번째를 읽을 수 없음", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("가짜 서버가 요청을 받지 못함")
	}
	return f.body(t, f.count()-1)
}

// markdownText는 플랫폼별 필드 차이를 처리하여 본문을 추출한다.
// DingTalk markdown/ActionCard는 text, WeCom markdown은 content를 사용한다.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("요청에서 본문을 식별할 수 없음: %v", body)
	return ""
}

// agePendingBatch는 대기 전송의 시각을 과거로 옮겨 요약 기한을 테스트한다.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "실시간 알림",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL 인젝션", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("메시지 1개 기대, 실제 %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL 인젝션", "높음", "요약"} {
		if !strings.Contains(text, want) {
			t.Fatalf("메시지 본문에 %q 누락:\n%s", want, text)
		}
	}
	// 전송은 sent로 전환되어야 한다.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("전송 후 sent로 표시되지 않은 항목 %d개", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "마스킹 사례",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("채널 목록 실패 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("API 응답에 인증 정보 노출: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("새 채널이 목록에 없음")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("인증 정보 필드는 마스킹 값이어야 함: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("API가 인증 정보 필드를 UI에 알려야 함")
	}

	// 이름만 PATCH하고 마스킹 인증 정보를 반환하면 실제 인증 정보는 유지해야 한다.
	body, _ := json.Marshal(map[string]any{
		"name":   "변경 후 이름",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("갱신 실패 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("마스킹 값으로 실제 인증 정보가 덮어써짐: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("마스킹 값으로 secret이 덮어써짐: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "변경 후 이름" {
		t.Fatal("이름이 갱신되지 않음")
	}

	// 마스킹 재전송과 달리 명시적 빈 secret은 삭제해야 한다.
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret 비우기 실패 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("빈 문자열은 secret을 지워야 함")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"잘못된 유형", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "잘못된 채널 유형"},
		{"이름 누락", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "채널 이름이 없습니다"},
		{"webhook 누락", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"잘못된 webhook 프로토콜", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "유효하지 않은 Webhook 주소"},
		{"잘못된 모드", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "잘못된 알림 모드"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400 기대, 실제 %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("오류에 %q가 있어야 함, 실제 %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("없는 채널 삭제는 404 기대, 실제 %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "치명적만",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "낮은 위험 문제", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("임계값 미만 취약점은 전송을 생성하면 안 됨, 실제 %d개", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("필터링된 취약점은 메시지를 보내면 안 됨")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "요약 알림",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("요약 취약점%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 기한 전에는 전송하지 않는다.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("요약 기한 전에 전송함")
	}

	// 배치를 오래된 것으로 바꾸면 세 개를 메시지 하나로 합친다.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("세 개를 메시지 하나로 요약해야 함, 실제 %d개 전송", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "최근") || !strings.Contains(text, "취약점 3개") {
		t.Fatalf("요약 메시지에 개수/기간 문구 누락:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("요약 취약점%d", i)) {
			t.Fatalf("요약 메시지의 %d번째 항목 누락:\n%s", i, text)
		}
	}
	// 같은 배치는 batch_id를 공유해야 한다.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("세 전송이 batch_id 하나를 공유해야 함, 실제 distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "비활성 채널",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "비활성 기간 취약점", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("비활성 채널은 전송을 생성하면 안 됨, 실제 %d개", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 구독",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "상태 변경 사례", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// fixed 상태 변경과 같은 회차에 finding_created도 전송되어 두 개일 수 있다.
	// 상태 변경이 나중에 생성되지만 순서에 의존하지 않고 전체를 찾는다.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("상태 변경 → 수정됨 메시지를 받지 못함(총 %d개)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 미구독",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "변경 미구독", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("미구독 채널은 상태 변경 전송을 받으면 안 됨, 실제 %d개", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "테스트 전송",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("테스트 전송 실패 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("가짜 서버에 테스트 메시지 1개 기대, 실제 %d", hook.count())
	}
	// 실제 취약점으로 오해하지 않도록 한눈에 테스트임을 알 수 있어야 한다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("테스트 메시지임을 명시해야 함: %s", text)
	}
	// 잘못된 설정은 채널 원래 오류를 그대로 반환한다.
	badID := f.createChannel(t, map[string]any{
		"name":   "잘못된 주소",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("전송 실패는 502 기대, 실제 %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 반드시 실패하는 주소로 failed 전송을 만든다.
	chID := f.createChannel(t, map[string]any{
		"name":   "실패 재시도",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "실패할 알림", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 재시도 예산이 소진될 때까지 전송한다.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("재시도 소진 후 failed 기대, 실제 %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("기록 조회 실패 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("실패 전송 1개 기대, 실제 total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("사용자가 진단할 수 있도록 기록에 실패 원인이 필요함")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("시도 수를 기록해야 함, 실제 %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "실패할 알림" {
		t.Fatalf("기록에 취약점 제목이 있어야 함, 실제 %q", hist.Deliveries[0].Title)
	}

	// 수동 재전송은 pending과 시도 수 0으로 초기화한다.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("재전송 실패 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("재전송 후 pending, attempts=0 기대, 실제 %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 실패: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta는 전체 %d개 채널을 나열해야 함, 실제 %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("채널 %s가 인증 정보 필드를 보고하지 않음", k.Kind)
		}
	}

	// 전역 설정 세 항목의 저장·조회. 링크가 //function/...이 되지 않도록 끝 슬래시를 제거한다.
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("설정 저장 실패 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("상세 링크 주소가 정규화되지 않음: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("요약 주기가 적용되지 않음: %v", payload["notify_digest_interval_min"])
	}

	// 잘못된 값은 거부한다.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s는 400 기대, 실제 %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL은 public_base_url 설정 시
// 단일 메시지가 버튼 있는 ActionCard이며 링크가 취약점 상세를 가리키는지 검증한다.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상세 링크",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "링크 있는 취약점", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("링크가 있으면 ActionCard 기대, 실제 msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("링크 불일치\n기대 %s\n실제 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL은 외부 주소 없을 때 localhost나 상대 경로 같은
// 잘못된 링크를 만들지 않고 일반 markdown을 사용하는지 검증한다.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "링크 없음",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "링크 없는 취약점", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("외부 주소 없으면 markdown 기대, 실제 %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "상세 보기") {
		t.Fatalf("외부 주소 없으면 상세 링크가 없어야 함:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder는 조용한 누락 수정의 E2E 검증이다.
//
// 채널 길이 제한(WeCom 4096바이트)으로 배치를 다 담지 못하면 항목 단위로 나눈다.
// 담긴 항목만 완료로 표시하고 나머지는 다음 메시지를 위해 대기열에 둔다. 이전에는
// 배치 전체를 성공으로 표시하여 잘린 항목이 메시지와 실패 목록 모두에 없고
// 기록에는 성공으로 남아 취약점 알림이 사라졌다.
//
// 실제 수용 수만 완료, 나머지는 대기, 지연 항목의 재시도 예산 유지,
// 다음 회차에서 정체 없이 나머지 전송이라는 네 조건을 검증한다.
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// 여섯 채널 중 가장 엄격한 markdown 4096바이트 제한의 WeCom 사용.
	chID := f.createChannel(t, map[string]any{
		"name":   "분할 요약",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 60개가 4096바이트를 확실히 넘도록 긴 제목을 사용한다.
	longName := strings.Repeat("매우 긴 취약점 이름", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("메시지 하나만 전송 기대, 실제 %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("완료로 표시된 항목이 있어야 함")
	}
	if pending == 0 {
		t.Fatalf("%d개 배치는 4096바이트에 담기지 않아 대기 항목이 남아야 함; sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("항목 수 불일치: sent=%d pending=%d total=%d(완료도 대기도 아니면 누락)", sent, pending, total)
	}
	// 본문에 이번 메시지에 포함하지 못한 개수를 정확히 알린다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("메시지에 미포함 항목 안내가 필요함:\n%.400s", text)
	}

	// 획득 시 증가한 attempts를 지연 시 되돌려 재시도 예산을 보존한다.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("지연 항목이 재시도 횟수를 소모하면 안 됨(이후 잘못 실패 처리됨), 실제 attempts=%d", maxAttempts)
	}

	// 수렴할 때까지 반복하여 여러 회차에 걸쳐 결국 모두 전송되는지 확인한다.
	// 두 번째에 끝난다는 검증보다 강하며 분할의 정체와 잔여 항목 누락을 방지한다.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("분할 전송이 수렴하지 않음: %d회 후에도 %d개 미해결", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d회에 진전이 없어 남은 %d개가 영구 정체될 수 있음", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096바이트 메시지에 긴 제목 %d개를 담을 수 없어 여러 회차가 필요함, 실제 %d회", total, rounds)
	}
	// 첫 회차 이후는 채널 거부 없이 순수한 후속 전송이어야 한다.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("가짜 서버가 항상 성공하므로 실패 항목이 없어야 함, 실제 %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget은 설정 불일치 방지 검증이다.
//
// db.MaxNotifyAttempts 예산은 상태 머신 정책이고 notifyBackoff는 엔진 실행 간격으로
// 다른 패키지에 있다. 예산만 5회로 늘리고 백오프를 늘리지 않으면
// 오류 없이 4·5회에 마지막 간격을 재사용하여
// 원인 모르게 재시도 속도가 느려지는 현상이 생긴다.
// 길이가 같은지 검증하여 CI에서 불일치를 발견한다.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("백오프 단계 수(%d)와 최대 시도 수(%d)가 다름. 둘을 함께 변경해야 함",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// 재시도가 급해져 제한을 악화시키지 않도록 백오프 간격은 단조 비감소여야 한다.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("백오프 간격은 단조 비감소여야 함: %d단계 %v < %d단계 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget은 토큰 확인 후 획득 순서를 보장한다.
// 획득 후 폐기하면 제한에 막힌 전송에도 attempts가 증가하여
// 대기만으로 예산이 소진되고 failed가 된다.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// 토큰 버킷만 테스트하므로 Server를 만들 필요가 없다.
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 분당 1개면 가득 찬 버킷도 최대 1개다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("분당 1개의 가득 찬 버킷에서 토큰 1개 기대, 실제 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("토큰 소진 후 즉시 0 기대, 실제 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("반 주기에는 토큰 하나가 채워지면 안 됨, 실제 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("한 주기 후 토큰 하나 보충 기대, 실제 %d", got)
	}
	// 무제한 채널도 한 회차가 무한 누적으로 막히지 않도록 유한 상한을 사용한다.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("무제한은 회차 상한 %d 기대, 실제 %d", notifyUnlimitedBurstPerTick, got)
	}
	// 채널별 버킷은 독립적이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("채널 1 버킷은 여전히 비어야 함, 실제 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens는 want만큼만 가져오는 의미를 보장한다.
//
// 이전 구현은 버킷을 다 비운 뒤 호출자가 잘라 분당 100개 중 회차에 5개만 쓰고
// 95개를 버렸다. 대기 항목 없는 회차도 차감하여
// 누적 시 rate_per_min만큼 한 번에 보낸다는 주석의 동작이 불가능했다.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 초기 버킷 100개 중 이번에는 5개만 필요하다.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5이면 정확히 토큰 5개 기대, 실제 %d", got)
	}
	// 나머지 95개는 버리지 않고 버킷에 남아야 한다.
	// 시간을 진행하지 않아 보충이 아닌 기존 토큰만 가져오게 한다.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("남은 토큰 95개를 사용할 수 있어야 함, 실제 %d. 버킷을 통째로 비움", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("버킷 소진 후 0 기대, 실제 %d", got)
	}
	// want<=0이면 토큰을 차감하지 않는다(빈 회차 무료).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0이면 0 기대, 실제 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0은 토큰을 쓰지 않아 20개 모두 남아야 함, 실제 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget은 요약의 두 단위를 구분한다.
//
// 배치 크기를 요청 예산에 연결하면 분당 20개 채널은
// 3초 tick에 토큰 하나만 보충되어 요약에 취약점 하나만 담는다.
// 기능상 요약이 없는데도 최근 30분 새 취약점 1개라는 제목이 붙는다. 오류가 나지 않고
// 기존 E2E는 stepDigest에 큰 limit를 직접 전달하여
// step 예산 계산을 우회하므로 여기서 결정 자체를 검증한다.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 배치 하나 = 메시지 하나 = 요청 하나 = 토큰 하나. 토큰 단위는 취약점이 아닌 메시지다.
	if tokens != 1 {
		t.Fatalf("요약 배치는 메시지 하나이므로 토큰 하나만 소비해야 함, 실제 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("요약 배치 크기는 메모리 상한 db.MaxDigestBatchSize=%d여야 함, 실제 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 배치 크기는 회차 요청 예산보다 훨씬 커야 한다. 비슷한 규모면
	// 메시지 수와 배치 취약점 수를 다시 하나의 값으로 혼동한 것이다.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("요약 배치 크기 %d가 회차 요청 예산 %d에 제한되면 안 됨. "+
			"요청 예산은 임대에서 역산한 요청 횟수이며 배치 취약점 수와 단위가 다름",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease는 상수 관계 불일치를 방지한다.
//
// 채널별 회차 상한 notifyMaxSendsPerChannelPerTick은 임대 시간에서 역산한다.
// 직렬 전송 최악 시간이 임대보다 짧아야 뒤 항목 전송 전 만료를 막는다.
// 다중 인스턴스는 만료 항목을 재획득하여 중복 전송할 수 있다. 세 상수가 떨어져 있어
// 하나만 바꾸면 오류 없이 관계가 깨질 수 있으므로 여기서 검증한다.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("채널 한 회차 최악 시간 %v는 임대 %v보다 짧아야 함. "+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"세 상수 중 하나를 바꾸면 나머지 둘도 확인해야 함",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
