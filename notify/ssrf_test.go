package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 이 파일은 다음 두 보안 강화 사항을 검증한다.
//   ① 전송 주소가 서버를 경유하여 내부망이나 클라우드 메타데이터에 접근하면 안 됨(SSRF)
//   ② 주소 검증 오류가 주소에 포함된 인증 정보를 노출하면 안 됨
//
// 여러 테스트가 127.0.0.1의 httptest 수신 서버를 사용하지만 기본 보호 동작은 이를 차단한다.
// 따라서 TestMain에서 AllowLocalTargetsEnv를 활성화하고 아래 SSRF 사례마다
// 명시적으로 해제하여 기본 거부 동작을 검증한다.

func TestMain(m *testing.M) {
	// 일반 사례는 로컬 테스트 수신 서버에 연결하도록 허용한다. SSRF 사례에서 일시적으로 해제한다.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault는 SSRF 방어의 핵심을 검증한다.
// 기본 설정에서 루프백 전송은 연결 계층에서 거부해야 한다.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 예외 허용 해제 = 기본 동작
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("기본적으로 루프백 주소 전송을 허용하면 안 됨")
	}
	if hit {
		t.Fatal("요청이 로컬 서비스에 도달함: 보호 기능이 작동하지 않음")
	}
	// 로컬 SMTP 릴레이는 유효한 설정이므로 오류에 명시적 허용 방법을 안내한다.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지에 명시적 허용 방법이 있어야 함: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn은 명시적으로 허용하면 사용할 수 있는지 검증한다.
// 로컬 postfix나 내부 릴레이 같은 정상 배포를 일괄 차단하면 안 된다.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("명시적 허용 후 전송할 수 있어야 함: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // 클라우드 메타데이터 엔드포인트: 이 함수의 주된 존재 이유
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped 주소는 원래 형태로 변환 후 판정해야 우회를 막을 수 있음
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s를 거부해야 함", s)
		}
	}
	// RFC1918 사설망은 의도적으로 허용한다. 자체 Mattermost나 SMTP 릴레이는 일반적인 정상 용도다.
	// 이 절충을 검증하여 사설망 차단이 추가되면 실패하도록 한다.
	// 기존 배포를 조용히 중단시키지 않고 의식적으로 정책을 결정하게 한다.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s를 허용해야 함(사설망은 일반적인 정상 전송 대상)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets는 설정 단계의 사전 안내를 검증한다.
// 차단 대상 IP 리터럴은 첫 전송 실패를 기다리지 않고 저장 시 거부해야 한다.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s를 설정 단계에서 거부해야 함", raw)
		}
	}
	// 공인 주소와 사설 주소는 정상 통과한다(사설망은 연결 단계에서도 차단하지 않음).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s가 검증을 통과해야 함: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials는 이전 수정에서 누락되어 감사로 발견된 경로를 검증한다.
//
// url.Parse 실패 시 반환하는 *url.Error의 Error()는 전체 원래 주소를 포함한다. 이전에는
// http.Client.Do 오류만 마스킹하고 이 경로를 놓쳤다. 당시 추가한 영구 실패 경로 테스트의
// file://, gopher://, ftp://는 모두 url.Parse가 성공하고 scheme 분기로 들어가므로
// 모두 통과해도 이 경로의 안전성을 보장할 수 없었다.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 잘못된 퍼센트 이스케이프
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // 숫자가 아닌 포트
		"http://[::1?access_token=" + leakProbeToken,                  // 짝이 맞지 않는 괄호
	}
	for _, raw := range cases {
		// 먼저 입력이 실제로 url.Parse를 실패시키는지 확인한다. 이 확인이 없으면 테스트가
		// 눈치채지 못한 채 다른 경로로 들어갈 수 있다(이전의 잘못된 보장도 이 때문에 발생).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q는 해석에 실패해야 함. 그렇지 않으면 대상 경로를 검증하지 못함", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q는 검증에 실패해야 함", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// 채널 계층의 래퍼도 주소를 노출하지 않는지 확인한다.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("잘못된 주소는 검증에 실패해야 함")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault는 SMTP 채널의 연결 보호를 검증한다.
//
// 이전 메일 채널은 보호 없는 net.Dialer를 사용해 SSRF 방어의 유일한 허점이었다. host를
// 169.254.169.254나 127.0.0.1로 설정하면 직접 연결되었고 smtp.NewClient 핸드셰이크 실패 시
// 상대의 응답 한 줄이 오류에 포함되어 last_error와 전송 기록 API로 노출되었다. 이는 다른 채널에서
// 이미 차단한 부분적인 블라인드 읽기 수단이며 연결 거부와 시간 초과 차이로 포트도 탐지할 수 있었다.
//
// TestMain은 다수의 127.0.0.1 테스트 수신 서버를 위해 AllowLocalTargetsEnv를 전역 활성화한다.
// 이 사례에서는 직접 해제해야 한다. 그렇지 않으면 보호 여부와 무관하게 통과하며,
// 이것이 기존 테스트가 허점을 발견하지 못한 이유다.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 예외 허용 해제 = 기본 동작
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("기본적으로 루프백 주소에 메일을 전송하면 안 됨")
	}
	// Control 훅에서 차단하므로 연결 자체가 성립하지 않고 EHLO도 전송되지 않아야 한다.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 세션이 성립함: 보호 기능이 작동하지 않음")
	}
	// 로컬 postfix 릴레이는 정상 설정이므로 오류에 명시적 허용 방법을 안내한다.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지에 명시적 허용 방법이 있어야 함: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn은 명시적 허용 후 정상 전송을 검증하는 짝 테스트다.
// 내부 자체 SMTP와 로컬 릴레이는 일반적인 배포이므로 보호 기능이 일괄 차단하면 안 된다.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("명시적 허용 후 로컬 SMTP로 전송할 수 있어야 함: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO가 없어 실제 세션이 성립하지 않음")
	}
}
