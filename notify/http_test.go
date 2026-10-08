package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 이 파일은 doJSON의 HTTP 계층 오류 분류를 검증한다.
//
// 각 채널 어댑터는 플랫폼별 업무 오류 코드(DingTalk errcode,
// Feishu code, Telegram ok 필드)를 처리하고 HTTP 분류는 doJSON이 공통 처리하므로
// 두 방어 계층을 별도로 검증한다. HTTP 분류가 없으면 503 게이트웨이 오류를 영구 실패로
// 오인해 재시도를 포기하거나 403을 재시도 가능으로 오인해 세 차례 불필요하게 대기한다.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 성공은 오류 아님", 200, false},
		{"429 속도 제한 재시도 가능", 429, false},
		{"408 요청 시간 초과 재시도 가능", 408, false},
		{"500 서버 오류 재시도 가능", 500, false},
		{"502 게이트웨이 오류 재시도 가능", 502, false},
		{"503 서비스 사용 불가 재시도 가능", 503, false},
		{"400 매개변수 오류 영구 실패", 400, true},
		{"401 인증 실패 영구 실패", 401, true},
		{"403 접근 금지 영구 실패", 403, true},
		{"404 주소 없음 영구 실패", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx는 오류가 없어야 함: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx가 아니면 오류가 발생해야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d permanent 판정 오류: 기대 %v, 실제 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// 설정 오류인지 상대 서버 장애인지 알 수 있도록 오류에 상태 코드를 포함한다.
			// Go의 영어 StatusText 대신 숫자를 검증한다. 이 패키지는 프로젝트와 동일하게
			// 한국어 문구를 사용하며 숫자는 언어와 무관하게 안정적으로 검증할 수 있다.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("오류에 HTTP 상태 코드 %d가 있어야 함, 실제 %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet은 상대 서버의 오류 설명을 가져오는지 검증한다.
// 설명이 없으면 실패 사실만 알 수 있고 거부 이유는 알 수 없다.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류가 발생해야 함")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("오류에 상대 서버의 설명이 있어야 함, 실제 %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded는 snippet 형식을 제한한다.
// 상대 응답이 last_error와 프런트엔드 표에 그대로 들어가므로 여러 줄이나 과도한 길이는 표시와 페이로드를 해친다.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 줄바꿈, 탭, 5000자의 긴 내용을 포함하는 응답.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류가 발생해야 함")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("오류를 한 줄로 줄여야 함, 실제 %q", msg)
	}
	// snippet 최대 200자와 고정 접두사를 합쳐도 원래 응답보다 훨씬 짧아야 한다.
	if len(msg) > 400 {
		t.Errorf("오류가 너무 김(%d바이트), snippet으로 잘라야 함: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse는 읽기 한도를 검증한다. 상대가 비정상적으로 큰 응답을 보내도
// 전체를 메모리로 읽으면 안 된다(전송 기록마다 last_error 사본이 저장된다).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류가 발생해야 함")
	}
	if len(err.Error()) > 400 {
		t.Errorf("큰 응답의 읽기 길이를 제한하고 잘라야 함, 오류 길이 %d", len(err.Error()))
	}
}
