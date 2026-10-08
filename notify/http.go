package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets는 루프백/링크 로컬 주소로 알림 전송을 허용할지 결정합니다.
//
// 기본은 거부입니다. IM 봇이나 공용 메일 서버가 보통 사용하지 않으며 같은 호스트의
// 관리 포트나 클라우드 메타데이터 엔드포인트처럼 민감한 서비스에 접근할 수 있습니다
// (169.254.169.254에서 인스턴스 자격 증명 조회 가능). 관리자 설정이더라도 XSS/CSRF로
// 빌린 세션이나 같은 JWT를 공유한 사용자가 주소를 바꾸면 응답을 읽을 수 있습니다.
// doJSON은 4xx/5xx 본문 앞 200바이트를 last_error에 저장하고 전달 이력 API가
// 다시 표시하므로 부분 읽기 수단이 됩니다.
//
// 다만 127.0.0.1:25의 postfix 같은 로컬 SMTP 릴레이는 흔한 구성이므로
// 무조건 허용하지 않고 명시적인 설정을 제공합니다.
// ARTEX_NOTIFY_ALLOW_LOCAL=1이면 허용합니다.
//
// AllowLocalTargetsEnv로 공개해 이 패키지와 server의 127.0.0.1 httptest
// 모의 수신 서버 테스트가 명시적으로 켤 수 있게 합니다.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP는 기본 전송 금지 대역인지 확인합니다.
//
// 루프백, 링크 로컬(클라우드 메타데이터 포함), 미지정, 멀티캐스트만 거부합니다.
// RFC1918 사설망은 내부 Mattermost/SMTP 릴레이 등의 정상 사용을 위해 허용합니다.
// 모두 차단하면 실제 배포에서 기능을 쓸 수 없으므로 의도적으로 구분합니다.
// 민감한 대상은 보호하면서 정상 배포까지 막지 않아야 합니다.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4 매핑 IPv6(::ffff:127.0.0.1)는 IPv4로 환원해 검사 우회를 막습니다.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial은 http.Transport 다이얼러의 Control 훅이며 연결을
// 만드는 시점에 대상 주소를 확인합니다.
//
// 설정 저장 때만이 아니라 최종 연결 단계에서 검사해야
// 검증 시 공인 IP였으나 연결 시 내부 IP로 바뀌는 DNS 리바인딩과
// 리디렉션을 통한 우회를 막습니다. 다른 호스트 이동을 막더라도 같은 호스트의
// 이동은 경로를 다른 곳으로 바꿀 수 있습니다.
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소 %q를 해석할 수 없습니다", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬/링크 로컬 주소 %s로의 전송을 거부합니다(로컬 서비스 전송이 필요하면 %s=1 설정)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport는 기본 Transport에 연결 보호만 추가합니다.
// Clone으로 연결 풀, HTTP/2, 시간 제한, proxy 등의 기본 최적화를 유지해
// 검사 하나 때문에 다른 동작이 바뀌지 않게 합니다.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient는 모든 채널이 공유하는 전달 클라이언트입니다.
//
// 대상 트래픽용 전역 송신 프록시(server의 GlobalProxy)는 불안정한 터널일 수 있어
// 알림 가용성이 대상 네트워크 상태에 종속되지 않도록 재사용하지 않습니다.
// IM은 직접 연결하며 15초보다 느린 상대는 장애로 보고 제한합니다.
//
// 다른 호스트로의 리디렉션은 거부합니다. 알림은 고정 엔드포인트가 일반적이며
// DingTalk access_token, WeCom key, Telegram bot token이 URL에 있으므로
// 다른 호스트로 따라가면 자격 증명을 넘기게 됩니다.
// 끝에 슬래시를 붙이는 등 같은 호스트의 이동은 허용합니다.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리디렉션 횟수가 너무 많습니다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("다른 호스트로의 리디렉션 거부(%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit는 비정상 서버의 큰 응답 읽기를 제한합니다. 전달 이력에는
// 오류 코드와 짧은 설명만 필요합니다.
const respBodyLimit = 8 << 10

// doJSON은 요청 한 번을 보내고 길이를 제한한 응답 본문을 반환합니다.
//
// payload가 nil이면 GET 등 본문이 필요 없는 요청에 빈 본문을 보냅니다.
// headers는 일반 Webhook의 사용자 정의 헤더로 그대로 추가합니다.
//
// 핵심 책임은 오류 분류입니다. 네트워크 실패와 5xx/408/429는 재시도 가능,
// 나머지 4xx는 영구 실패이며 403 반복은 같은 오류 로그만 늘립니다.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// 직렬화 실패는 로컬 유형 오류이므로 재시도로 해결되지 않습니다.
			return nil, Permanent(fmt.Errorf("요청 본문 생성 실패: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// 잘못된 URL은 보통 입력 오류이므로 영구 실패입니다.
		// url.Parse 오류에 전체 주소가 있으므로 그대로 전달하지 않습니다.
		return nil, Permanent(fmt.Errorf("유효하지 않은 요청 주소: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 연결 거부, DNS 실패, 시간 초과는 주로 일시적이므로 백오프 재시도합니다.
		//
		// http.Client.Do의 *url.Error는 전체 URL을 포함하므로 오류의 민감 정보를 제거해야 합니다.
		// Error() 형식은 Op "전체URL": 하위 오류이며 URL에는
		// DingTalk access_token, WeCom key, Feishu hook id, Telegram bot token이 있습니다.
		// 그대로 내보내면 notification_deliveries.last_error의 평문 DB,
		// 채널 마스킹을 우회하는 전달 이력 API 응답,
		// 서버 로그와 테스트 전송 API의 502 응답에 자격 증명이 노출됩니다.
		return nil, fmt.Errorf("요청 실패: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답 읽기 실패: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 요청 제한과 408 시간 초과는 재시도하고 나머지 4xx는 설정/권한 문제로 재시도하지 않습니다.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대 서버 요청 제한 또는 시간 초과(HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대 서버 오류(HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대 서버가 요청을 거부했습니다(HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet은 응답을 짧은 한 줄 오류로 만듭니다. 줄바꿈과 많은 공백을
// last_error에 그대로 넣어 전달 이력 화면이 깨지지 않도록 합니다.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget은 주소를 오류용 scheme://host/… 형태로 만듭니다.
//
// 이 패키지의 유일한 주소 비식별화 방식이며 scheme과 host 외에는
// 모두 제거합니다. URL의 자격 증명 위치를 일반적이고 안전하게 판별할 수 없기 때문입니다.
//
//	DingTalk query 자격 증명       /robot/send?access_token=xxx
//	WeCom query 자격 증명         /cgi-bin/webhook/send?key=xxx
//	Feishu 경로 끝 자격 증명       /open-apis/bot/v2/hook/<hook_id>
//	Telegram 경로 중간 자격 증명   /bot<token>/sendMessage
//
// 유용한 부분만 남기려면 채널마다 예외가 필요하고 하나라도 누락하면 자격 증명이 유출됩니다.
// DNS/연결/인증서 문제는 host만으로 조사할 수 있으며
// 구체적인 봇은 채널 설정의 마스킹된 끝자리로 식별합니다.
//
// 해석 실패 시 고정 문구를 반환하고 원문은 절대 노출하지 않습니다.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(주소 해석 불가)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError는 전송 오류에서 주소를 제거하고 하위 원인만 남깁니다.
//
// *url.Error는 {Op, URL, Err}이고 Error()에 URL을 포함합니다.
// Err를 직접 읽어 Error()를 우회하면 URL 인코딩/이스케이프 변형을 놓치기 쉬운
// 사후 문자열 치환보다 안정적입니다.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// 리디렉션 정책 오류 등 *url.Error가 아니어도 주소가 있을 수 있어 비식별화합니다.
	return redactURLsInText(err.Error())
}

// redactURLsInText는 텍스트의 HTTP(S) 주소를 비식별 형태로 바꿉니다.
//
// 구조화된 필드를 얻을 수 없는 리디렉션/외부 라이브러리 오류의 보완 처리입니다.
// http/https 접두사만 인식하고 주소에 들어갈 수 없는 공백과 따옴표로 구분합니다.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
