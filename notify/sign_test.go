package notify

import (
	"net/url"
	"testing"
	"time"
)

// 서명 기준값은 이 패키지 구현이 아닌 OpenSSL로 독립적으로 계산했다.
// 자체 구현을 사용하면 코드가 바뀌지 않았다는 사실만 검증하고 알고리즘의 정확성은 검증할 수 없다.
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("서명 실패: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("생성한 주소를 해석할 수 없음: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("서명 불일치\n기대 %s\n실제 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("타임스탬프는 밀리초 단위로 원래 값을 포함해야 함, 실제 %q", q.Get("timestamp"))
	}
	// 서명이 기존 쿼리 매개변수(access_token)를 덮어쓰면 안 된다.
	if q.Get("access_token") != "tok" {
		t.Errorf("기존 쿼리 매개변수 누락, 실제 %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("서명 불일치\n기대 %s\n실제 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer는 두 서비스의 알고리즘 차이를 보장한다. 매개변수 순서가 서로 반대다.
// DingTalk는 key=secret, Feishu는 key=서명 대상 문자열이므로 다른 서비스 구현을 복사하면 검증에 실패한다.
// 향후 리팩터링에서 두 알고리즘을 같은 함수로 합치지 않도록 보장한다.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk와 Feishu 서명이 같으므로 둘 중 하나의 알고리즘 구현이 잘못됨")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 서명을 활성화하지 않은 봇에는 timestamp/sign 매개변수를 추가하면 안 된다.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret이 없으면 주소를 바꾸면 안 됨, 실제 %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q를 허용해야 함: %v", s, err)
		}
	}
	// file:// 등은 http.Client 처리 범위를 벗어나므로 허용하면 안 된다.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q를 거부해야 함", s)
		}
	}
}
