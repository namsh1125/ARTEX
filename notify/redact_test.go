package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 이 파일은 채널 구현에서 발생하는 어떤 오류에도 인증 정보가 포함되지 않는다는 불변 조건을 검증한다.
//
// 기존 채널 테스트는 성공 경로와 플랫폼 업무 오류만 검증하고 전송 계층 실패는 놓쳤다.
// 연결 거부, DNS 실패, 시간 초과 같은 전송 오류가 특히 위험하다.
// http.Client.Do의 *url.Error는 전체 URL을 오류에 넣으며 이 기능의 일부 채널은
// 인증 정보를 URL에 담는다. 이 정보는 다음 네 경로로 유출될 수 있다.
//
//	notification_deliveries.last_error → DB에 평문 저장
//	GET /api/notify/deliveries 응답 → 채널 설정 마스킹을 우회하여 브라우저에 표시
//	서버 로그 → 외부로 전송되어 보관되는 경우가 많음
//	테스트 전송 API의 502 응답 → 프런트엔드에 직접 표시
//
// 따라서 단일 함수뿐 아니라 각 채널에서 반드시 실패하는 요청을 실제로 보내고
// 오류 텍스트에 인증 정보가 없는지 검증한다.

// credentialCases는 URL에 인증 정보를 넣는 모든 채널 형태를 다룬다.
// DingTalk/WeCom은 쿼리, Feishu는 경로 끝, Telegram은 경로 중간에 넣는다.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk access_token이 쿼리에 있음",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom key가 쿼리에 있음",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu hook id가 경로 끝에 있음",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token이 경로 중간에 있음",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk 서명 키",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken은 실제 인증 정보일 수 없는 센티널 값으로 오류 텍스트에서 검색한다.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials는 핵심 불변 조건이다.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 반드시 실패하는 대상: 127.0.0.1:1에 리스너가 없어 연결 거부 경로를 따른다.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "유출 탐지용 항목"}},
			})
			if err == nil {
				t.Fatal("도달할 수 없는 주소는 오류가 발생해야 함")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath는 영구 실패 경로를 검증한다.
// URL 검증 실패나 플랫폼 업무 오류도 외부로 전달되므로 인증 정보를 포함하면 안 된다.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 인증 정보를 포함하지만 형식이 잘못된 주소로 validateHTTPURL / url.Parse 경로를 유발한다.
		{"잘못된 DingTalk 주소", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"잘못된 WeCom 주소", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"잘못된 Feishu 주소", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"잘못된 Telegram API 주소", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"잘못된 일반 Webhook 주소", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("잘못된 설정은 오류가 발생해야 함")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("오류 텍스트에 인증 정보 %q 노출:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, 기대 %q", in, got, want)
		}
		// 마스킹 결과에 원래 주소의 경로나 쿼리 조각이 남으면 안 된다.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("마스킹 후 경로/쿼리 조각 %q가 남음: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 해석할 수 없는 입력은 원문을 절대 반환하지 않는다.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("해석할 수 없는 입력 %q가 %q로 표시됨", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL은 *url.Error 타입을 직접 검증한다.
// http.Client.Do가 반환하는 타입이며 정보 유출이 시작되는 지점이다.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("문제 진단을 위해 host를 유지해야 함, 실제 %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("문제 진단을 위해 근본 원인을 유지해야 함, 실제 %q", got)
	}
	// 진단에 필요한 Op(POST 또는 GET)도 유지한다.
	if !strings.Contains(got, "Post") {
		t.Errorf("작업 이름을 유지해야 함, 실제 %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback은 대체 경로를 검증한다. *url.Error가 아닌 사용자 정의 오류
// (예: 리디렉션 정책 오류)에 포함된 주소도 제거해야 한다.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("호스트 간 리디렉션 거부(a.example → http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("주소를 마스킹된 형태로 바꿔야 함, 실제 %q", got)
	}
	// 주소가 없는 텍스트는 그대로 유지한다.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("주소가 없는 텍스트를 바꾸면 안 됨")
	}
}

// TestCrossHostRedirectRefused는 URL 인증 정보가 호스트 간 리디렉션으로 유출되는 경로를 검증한다.
// httptest 서버 두 개가 127.0.0.1의 다른 포트에서 수신하므로 Host가 달라
// 호스트 간 리디렉션이 된다.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("호스트 간 리디렉션을 거부해야 함")
	}
	if hit {
		t.Fatal("리디렉션 대상에 접근함: 리디렉션으로 인증 정보가 유출됨")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed는 같은 호스트 리디렉션(예: 끝에 슬래시 추가)을 허용하는지 검증한다.
// 정상적인 작업 흐름까지 차단하면 안 된다.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 같은 호스트와 포트로 리디렉션.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("같은 호스트 리디렉션을 거부하면 안 됨: %v", err)
	}
}
