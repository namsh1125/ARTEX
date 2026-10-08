package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel은 대화형 카드를 사용하는 Feishu/Lark 사용자 정의 봇입니다.
//
// 플랫폼 특성:
//   - 서명 알고리즘이 DingTalk과 다르므로 feishuSign 설명에 주의하세요.
//   - DingTalk처럼 업무 오류가 HTTP 200 본문에 들어갑니다(code != 0).
//   - 카드 header 색상 템플릿을 심각도에 매핑해 목록에서도 등급을 알 수 있게 합니다.
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// Feishu 사용자 정의 봇은 초당 약 5회이며 분당 한도는 100회입니다.
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook URL의 마지막 부분이 봇 고유 식별자이므로 자격 증명입니다.
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// Webhook URL을 바꾸면 새 주소의 서명 키를 다시 명시해야 합니다.
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("유효하지 않은 Webhook 주소: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 서명 매개변수는 메시지와 같은 계층이며 secret이 있을 때만 포함합니다.
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 일부 Feishu hook 버전의 필드 이름도 함께 지원합니다.
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Feishu 응답 해석 실패: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Feishu 오류 %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Feishu 오류 %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign은 Feishu 공식 규칙으로 서명을 계산합니다.
//
// 실수하기 쉬운 부분: 공식 예시는 다음과 같습니다.
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// 즉 key = timestamp + 줄바꿈 + secret이고 message는 비어 있습니다.
// key=secret, message=stringToSign인 DingTalk 알고리즘과는 반대이므로
// 다른 구현을 따라 하면 서명 검증이 실패합니다(19021).
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate은 취약점 등급을 카드 header 색상에 매핑합니다.
// 알 수 없는 등급은 low의 blue와 혼동하지 않도록 grey를 사용합니다.
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes는 카드 크기의 보수적 한도입니다. 초과하면 전체를 거부하므로
// JSON 포장 비용을 포함해 공식 한도보다 충분히 낮게 설정합니다.
const feishuMaxCardBytes = 24000

// feishuCard는 대화형 카드와 실제 포함한 항목 수를 반환합니다.
// markdownBody처럼 카드에 들어간 항목만 전달 완료로 표시하도록 kept를 제공합니다.
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 항목 단위로 채운 뒤 헤더를 만듭니다. 나머지 N개는 다음 메시지에 포함된다는 안내의
		// N은 실제로 들어간 개수를 기준으로 해야 합니다.
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("플랫폼에서 전체 보기", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("상세 보기", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines는 단일 취약점의 lark_md 본문을 렌더링합니다.
//
// lark_md도 링크와 강조를 해석하므로 외부 필드는 markdownText로
// 한 줄로 만들고 이스케이프해야 취약점 제목이
// 클릭 가능한 외부 링크로 바뀌지 않습니다.
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**상태 변경**: %s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**유형**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**자산**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**요약**: %s", s)
		}
	}
	return out
}

// feishuBatchLine은 요약 카드의 항목 하나를 렌더링합니다.
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
