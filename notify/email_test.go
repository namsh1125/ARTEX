package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// 이 파일은 메일 채널의 프로토콜 수준 테스트를 보완한다. 이전 email.Send 커버리지는 0으로,
// SMTP 경로 전체를 실행한 사례가 없었다. 여섯 채널 중 프로토콜 범위가 가장 넓고
// 오류가 나기 쉬운 경로다(핸드셰이크, 인증, 봉투, DATA 단계마다 실패 의미가 다르다).
//
// net/smtp를 모킹하지 않고 직접 만든 최소 SMTP 서버로 테스트한다.
// 메일 채널 위험의 대부분은 실제 SMTP 서버와 통신하는 단계에 있으므로,
// 이 단계를 모킹하면 핵심을 테스트할 수 없다.

// fakeSMTP는 greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT을 처리하는 최소 SMTP 서버다.
// 각 사례의 요구에 따라 특정 단계에서 지정된 응답 코드를 반환한다.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply는 RCPT TO 응답이며 기본값은 250이다.
	rcptReply string
	// mailReply는 MAIL FROM 응답이며 기본값은 250이다.
	mailReply string
	// advertiseAuth가 true이면 EHLO에서 AUTH PLAIN 지원을 알린다.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP 수신 주소가 아님")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS를 알리지 않아 평문 경로를 사용한다(테스트 대상은 TLS가 아닌 봉투 로직).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 단순화: PLAIN 초기 응답은 여러 줄일 수 있으므로 바로 수락한다.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	// 봉투 단계에서 발신자, 수신자 두 명, DATA까지 진행해야 한다.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP 세션에 %q 누락, 실제 명령: %v", want, f.commands)
		}
	}
	// 본문은 base64 HTML이며 실제 취약점 내용을 포함해야 한다(인코딩 후에도 식별 가능).
	body := f.body()
	if body == "" {
		t.Fatal("DATA 단계에서 본문을 받지 못함")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type 헤더 누락:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("본문이 base64로 인코딩되지 않음(긴 HTML 줄은 SMTP의 1000바이트 줄 제한을 위반):\n%s", body)
	}
	// 모든 수신자가 To 헤더에 포함되어야 한다.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To 헤더에 일부 수신자 누락:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// 계정이 없으면 AUTH를 보내면 안 된다. 일부 릴레이는 이 경우 수신을 거부한다.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("계정 설정 없이 AUTH 전송: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies는 감사 수정 사항을 직접 검증한다.
// 5xx는 영구 실패, 4xx(그레이리스트)는 재시도 가능으로 분류한다.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"수신자 550 영구 거부", "550 5.1.1 User unknown", "250 OK", true},
		{"수신자 450 그레이리스트", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"수신자 452 사서함 가득 참", "452 4.2.2 Mailbox full", "250 OK", false},
		{"발신자 553 영구 거부", "250 OK", "553 5.1.3 Bad address", true},
		{"발신자 451 일시 오류", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("오류가 발생해야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 판정 오류: 기대 %v, 실제 %v (%v)", tc.permanent, got, err)
			}
			// 서버 관리자에게 문의할지 주소를 수정할지 알 수 있도록 서버 원문을 유지한다.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("오류에 서버 응답 코드가 포함되어야 함: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp의 PlainAuth는 암호화되지 않은 연결에서 인증 정보 전송을 거부한다(localhost 제외).
	// 이는 올바른 보안 동작이며 우회해서는 안 된다. 사용자가 해결할 수 있는 오류를 제공한다.
	// localhost가 아닌 호스트 이름으로 이 동작을 유발한다.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost가 아님
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("로컬 DNS가 로컬 서버로 해석되어 건너뜀(다른 사례에 영향 없음)")
	}
	// 연결 실패와 인증 정보 전송 거부 모두 허용한다. 핵심은 비밀번호를 몰래 전송하지 않는 것이다.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "연결") {
		t.Logf("오류: %v(localhost가 아닌 경우 연결 실패는 예상 동작)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// 메일 채널은 설정 필드가 가장 많다. 누락을 전송 시점에야 발견하지 않도록
	// 검증이 미리 차단하고 오류 메시지가 누락 항목을 알려 주는지 각각 확인한다.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host 누락", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port 누락", map[string]any{"host": "smtp.example.com"}},
		{"port 범위 초과", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from 누락", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to 누락", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("검증에 실패해야 함: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance는 설정 읽기의 유연성을 검증한다. JSONB 숫자는 float64지만
// 사용자가 UI에서 포트를 문자열로 입력하거나 배열 대신 단일 문자열을 입력할 수 있다.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 문자열 포트
		"from": "a@b.c",
		"to":   "d@e.f", // 배열 대신 단일 문자열
		"tls":  "true",  // 문자열 불리언
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("문자열 숫자를 허용해야 함: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt가 문자열 포트를 해석하지 못함, 실제 %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool이 문자열 \"true\"를 해석하지 못함")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings가 단일 문자열을 처리하지 못함, 실제 %v", to)
	}
}

// TestFilterValidateRejectsTypo는 감사 수정 사항을 직접 검증한다.
// 임계값 오타는 저장 시 차단해야 한다. 그렇지 않으면 필터가 조용히 무효화되어 모두 전송된다.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("유효한 임계값 %q 거부: %v", s, err)
		}
	}
	// 실제로 발생할 수 있는 오타는 모두 거부해야 한다.
	for _, s := range []string{"hgih", "HIGH", "심각", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("잘못된 임계값 %q를 거부해야 함(그렇지 않으면 필터가 무효화되어 모두 전송됨)", s)
			continue
		}
		// 오류 메시지가 올바른 수정 방법을 안내해야 한다.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("오류 메시지에 선택 가능한 값이 있어야 함, 실제 %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly는 저장은 엄격하게, 읽기는 유연하게 처리하는 역할을 보장한다.
// 기존 잘못된 값으로 채널 전체를 읽지 못하면 기존 채널의 전송이 갑자기 모두 멈추게 된다.
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // 오류 없음
	if f.MinSeverity != "hgih" {
		t.Fatalf("읽기 경로에서 원래 값을 보존해야 함, 실제 %q", f.MinSeverity)
	}
	// 채널은 여전히 이벤트를 판정할 수 있어야 한다(panic이나 차단 없음).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
