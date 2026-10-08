package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg는 따옴표와 줄바꿈이 있는 단일 메시지를 만듭니다. 제목/요약의 큰따옴표와 줄바꿈은
// 템플릿 보간에서 잘못된 JSON을 만들기 쉬운 입력입니다.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `로그인 "SQL 인젝션" 위험`,
			VulnClass: "SQL 인젝션",
			Severity:  "high",
			Summary:   "매개변수 id\n필터 누락으로 인젝션 발생",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg는 요약 메시지 묶음을 만듭니다.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "취약점" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "반사형 크로스 사이트 스크립팅",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost는 모의 수신 서버를 시작해 받은 본문/헤더를 검증 함수에 전달합니다.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("요청 본문이 유효한 JSON이 아닙니다: %v\n원문: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("상세 링크가 있으면 actionCard여야 함, 실제 %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("상세 링크 누락: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("요약 메시지는 markdown이어야 함, 실제 %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "최근 30분") {
			t.Errorf("요약 본문에 시간 구간 누락: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent는 HTTP 200이지만 errcode가 0이 아닌 경우를 검증합니다.
// 중국 IM 플랫폼은 errcode를 확인하지 않으면 전송 실패를 성공으로 기록할 수 있습니다.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode가 0이 아니면 오류여야 합니다")
	}
	if !IsPermanent(err) {
		t.Fatalf("키워드 불일치는 설정 오류이므로 영구 실패여야 함, 실제 %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("오류에 플랫폼 오류 코드가 포함되어야 함, 실제 %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("잘린 뒤 UTF-8이 유효하지 않아 WeCom이 전체를 거부합니다")
		}
	})
	// 4096바이트를 반드시 넘는 긴 한국어 요약 묶음을 만듭니다.
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("본문 %d바이트가 WeCom 한도 %d를 초과했습니다", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("본문이 비어 있습니다")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009는 이동 시간 구간의 요청 제한이므로 재시도 가능해야 함, 실제 %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000은 유효하지 않은 key로 재시도로 해결되지 않아 영구 실패여야 함, 실제 %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("대화형 카드여야 함, 실제 %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high 등급은 orange 색상이어야 함, 실제 %v", header["template"])
		}
		// secret이 있으면 서명 매개변수가 필수이며 없으면 Feishu가 19021로 거부합니다.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("서명 매개변수 누락: %v", body)
		}
		// 카드 요소에는 취약점 상세 URL을 가리키는 버튼이 있어야 합니다.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("카드에 상세 페이지 버튼이 없습니다")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret이 없으면 서명 매개변수도 없어야 함, 실제 %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML 해석 모드를 사용해야 함, 실제 %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// 제목/요약은 테스트 대상 또는 모델 출력에서 온 신뢰할 수 없는 내용입니다.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML을 이스케이프하지 않아 주입이 가능합니다: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔터티 예상, 실제 %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("&가 이스케이프되지 않았습니다: %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429는 재시도 가능해야 함, 실제 %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403은 설정 문제로 영구 실패여야 함, 실제 %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// 기본 템플릿은 제목에 따옴표/줄바꿈이 있어도 유효한 JSON을 보장합니다.
	// 단순한 "title": "{{.Title}}" 보간 대신 {{json .}}를 사용해야 합니다.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 높음] 로그인 "SQL 인젝션" 위험` {
			t.Errorf("제목 복원 오류: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items 수는 1이어야 함, 실제 %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "매개변수 id\n필터 누락으로 인젝션 발생" {
			t.Errorf("요약 복원 오류: %v", it["summary"])
		}
		// 숫자는 문자열이 아닌 JSON 숫자여야 합니다(json:",string" 등에 주의).
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id는 숫자여야 함, 실제 %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("사용자 정의 헤더 누락: %v", r.Header)
		}
		if body["msg"] != "3개" {
			t.Errorf("사용자 정의 템플릿 렌더링 오류: %v", body["msg"])
		}
		if body["first"] != "취약점1" {
			t.Errorf("range 추출 오류: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d개" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON이 아닌 렌더링 결과는 템플릿 오류이므로 영구 실패여야 함, 실제 %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("설정 묶음 %d는 거부되어야 합니다: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("메일 조립 실패: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From 헤더 오류:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To 헤더 오류:\n%s", msg)
	}
	// 한국어 제목은 RFC 2047로 인코딩해야 클라이언트에서 깨지지 않습니다.
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("제목이 RFC 2047로 인코딩되지 않았습니다:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("제목 디코딩 실패: %v", err)
	} else if !strings.Contains(dec, "SQL 인젝션") {
		t.Fatalf("디코딩한 제목 내용 오류: %q", dec)
	}

	// 본문은 base64이며 디코딩하면 유효한 HTML이어야 합니다.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("메일 헤더/본문 구분자가 없습니다")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("본문 base64 디코딩 실패: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("본문이 HTML이 아닙니다: %.80s", html)
	}
	// 제목은 텍스트 위치에 그대로 나타납니다. HTML 텍스트의 큰따옴표는 유효하며 이스케이프가 필요 없습니다.
	// 그대로 유지되는지 검사해 불필요한 추가 이스케이프로 따옴표가
	// &quot;로 표시되지 않게 합니다.
	if !strings.Contains(html, `"SQL 인젝션"`) {
		t.Fatalf("제목의 텍스트 위치 따옴표는 그대로 유지해야 합니다: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection은 메일 본문에서 실제로 방어해야 할 주입을 검증합니다.
// 취약점 제목/요약은 테스트 대상과 모델 출력에서 온 신뢰할 수 없는 내용입니다. 텍스트는
// & < >를, 속성은 따옴표까지 이스케이프해야 태그 삽입이나 href 조기 종료를 막을 수 있습니다.
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("제목이 이스케이프되지 않아 태그 주입 가능: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔터티 예상: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("&와 >가 이스케이프되지 않았습니다: %s", html)
	}
	// 상세 링크는 관리자가 설정하는 public_base_url이지만 속성 위치의 따옴표는 여전히
	// 이스케이프해야 href를 조기에 닫고 이벤트 처리기를 주입하지 못합니다.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 속성 이스케이프 오류: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("속성 위치의 따옴표는 이스케이프해야 합니다: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("%s 헤더를 찾지 못했습니다", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 검증 오류는 설정자에게 직접 표시되므로 누락된 필드를 명확히 알려야 합니다.
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "포트"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "수신자"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("채널 %s 미등록", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s 설정 %v는 검증 실패해야 합니다", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s 오류에 %q가 포함되어야 함, 실제 %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification은 SMTP 4xx/5xx의 의미 구분을 검증합니다.
// 4xx까지 영구 실패로 처리하면 그레이리스트 서버의 모든 알림이 첫 시도 후 failed가 됩니다.
// 그레이리스트는 자동 재시도가 가장 필요한 상황입니다.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 코드를 얻지 못하면 재시도 가능으로 처리해 일시적 오류를
		// 영구 실패로 오판하지 않습니다.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("수신자 거부", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("응답 %q: permanent=%v 예상, 실제 %v", tc.reply, tc.permanent, got)
		}
		// 분류와 무관하게 조사할 수 있도록 원문을 유지합니다.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("응답 %q 원문 유실: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 여섯 채널이 모두 있어야 하며 누락하면 UI 드롭다운에서 조용히 사라집니다.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("채널 수 %d 예상, 실제 %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("채널 %s 미등록", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("채널 %s의 Kind()와 등록 키 불일치", k)
		}
	}
	if ValidKind("nope") {
		t.Error("미등록 유형은 검증을 통과하면 안 됩니다")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("영구 실패로 인식해야 합니다")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("하위 오류를 전달해야 합니다: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil)은 nil을 반환해야 합니다")
	}
	if IsPermanent(nil) {
		t.Fatal("nil은 영구 실패가 아닙니다")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
