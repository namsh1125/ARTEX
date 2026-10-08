package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel은 DingTalk 사용자 정의 봇을 구현합니다.
//
// 구현에 반영한 플랫폼 특성:
//   - 봇당 분당 20개 제한이며 초과분은 HTTP 200이어도 조용히 버릴 수 있으므로
//     클라이언트에서 제한해야 합니다(DefaultRatePerMin 참고).
//   - 보안 설정은 서명/사용자 키워드/IP 허용 목록 중 하나입니다. 메시지 내용에 의존하지 않는
//     서명만 지원하며 셋 다 꺼진 기본 webhook도 지원합니다.
//   - 성공/실패 모두 HTTP 200이며 본문의 errcode로 구분합니다. 확인하지 않으면
//     전송 실패를 성공으로 기록하게 됩니다.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk Webhook URL은 access_token을 포함하므로 전체를 자격 증명으로 마스킹합니다.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 대상은 Webhook URL 자체이며 변경 시 새 주소의 서명 키를 다시 명시해야 합니다.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("유효하지 않은 Webhook 주소: %w", err)
	}
	return nil
}

// Send는 메시지를 전달합니다. 단일 항목에 상세 링크가 있으면 버튼 있는 ActionCard, 아니면 markdown을 사용합니다.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown에 명확한 바이트 한도는 없지만 증거 필드의 비정상적 증가를 막기 위해 제한합니다.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "상세 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk 업무 오류는 200 응답 안에 포함됩니다.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk 응답 해석 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000은 서명 검증 실패, 310000은 키워드 불일치로 모두 설정 오류이며
		// 재시도로 해결되지 않습니다.
		return 0, Permanent(fmt.Errorf("DingTalk 오류 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL은 공식 서명 규칙으로 webhook에 timestamp와 sign을 추가합니다.
//
// 서명 문자열 = timestamp + 줄바꿈 + secret, HMAC-SHA256 키도 secret이며
// 결과를 base64 후 URL 인코딩합니다. timestamp는 밀리초입니다. secret이 비면
// 서명을 끈 봇을 지원하도록 원래 주소를 반환합니다.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// url.Parse 오류에 access_token 포함 전체 주소가 있으므로 err를 그대로 전달하지 않습니다.
		return "", fmt.Errorf("Webhook 주소 해석 실패: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL은 주소/프로토콜을 검증하고 IP 리터럴 대상의 내부 주소 여부를 확인합니다.
//
// 두 가지 유의점:
//
//  1. 오류 메시지의 민감 정보를 제거해야 합니다. url.Parse의 *url.Error.Error()에는
//     원본 주소 전체와 자격 증명(DingTalk access_token, WeCom key, Telegram bot token,
//     Feishu hook id)이 포함됩니다. err를 그대로 반환하면 잘못된 주소 형식 오류만으로
//     자격 증명이 테스트 API의 400 응답, 전달 기록 last_error,
//     서버 로그와 전달 이력 API에 노출됩니다.
//
//  2. IP 리터럴은 즉시 내부 주소 여부를 확인하고 도메인은 연결 단계에서 확인합니다.
//     blockInternalDial이 최종 검증 지점이며 DNS 리바인딩도 막습니다. 여기서 확인하는 이유는
//     첫 전송 실패까지 기다리지 않고 설정 저장 시 안내하기 위함입니다.
//
// 프로토콜 제한은 방어적 조치입니다. file:///gopher:// 등은 http.Client의 예기치 않은
// 동작을 유발할 수 있으므로 scheme 검사로 차단합니다.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없습니다(%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http/https만 지원합니다. 입력: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("호스트명이 없습니다")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬/링크 로컬 주소 %s로의 전송을 거부합니다(로컬 서비스 전송이 필요하면 %s=1 설정)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
