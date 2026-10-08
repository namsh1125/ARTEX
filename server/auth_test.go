package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting은 키 없음과 읽기 실패에서 모두 빈 값을 반환하며 error만 다르다.
// 비밀번호 handler가 오류를 미설정으로 취급하면 DB 장애 중 초기화가 열려
// authInit이 인증 없는 요청으로 기존 관리자 비밀번호를 덮어쓰도록 허용하고
// authStatus는 UI를 /setup으로 보내 같은 작업을 유도한다.
//
// 두 테스트는 연결 풀을 닫아 읽기를 실패시키고 두 handler가 안전하게 거부하는지 확인한다.
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// 연결 풀을 닫아 이후 GetSetting이 sql.ErrNoRows가 아닌 오류를 반환하게 한다.
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (읽기 실패를 미초기화로 취급하면 /setup에서 비밀번호를 덮어쓸 수 있음); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("읽기 실패 시 initialized를 응답하면 안 됨, 실제 %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (읽기 실패를 허용하면 인증 없이 기존 비밀번호를 덮어쓸 수 있음); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("읽기 실패 시 토큰을 발급하면 안 됨: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"빈 값", "", true},
		{"7자", "1234567", true},
		{"8자", "12345678", false},
		{"한자 8개는 바이트가 아닌 문자 수로 계산", "密码密码密码密码", false},
		{"한자 3개는 9바이트지만 3문자", "密码强", true},
		{"72바이트", strings.Repeat("a", 72), false},
		{"73바이트는 bcrypt 상한 초과", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
