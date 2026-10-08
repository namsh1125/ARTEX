package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit는 Telegram sendMessage text 필드의 문자 수 한도입니다.
const telegramTextLimit = 4096

// telegramChannel은 Telegram Bot API를 구현합니다.
//
// 플랫폼 특성:
//   - 인증은 URL 경로(/bot<token>/sendMessage)에 있으며 서명이 필요 없습니다.
//   - MarkdownV2 대신 HTML을 사용합니다. MarkdownV2는 특수 문자 18개를 이스케이프해야 하고
//     하나라도 빠지면 전체를 거부하지만 HTML 텍스트는 & < > 세 개만 처리하면 됩니다.
//   - 업무 오류도 HTTP 200에 포함되므로 ok 필드로 판정합니다.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram은 개인 대화 초당 약 1개, 그룹 분당 20개이므로 보수적인 값을 사용합니다.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token이 전체 자격 증명이며 chat_id는 수신자일 뿐 Token 없이는 전송할 수 없어 비밀이 아닙니다.
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url은 Token을 받을 API 엔드포인트를 결정하므로 변경 시 Token을 다시 명시해야 합니다.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token이 없습니다")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID가 없습니다")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("유효하지 않은 API 주소: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram 응답 해석 실패: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429는 요청 제한으로 백오프 후 재시도가 유효합니다. 400 매개변수 오류, 401 token 오류,
	// 403 차단, 404 chat 없음은 설정 문제라 재시도로 해결되지 않습니다.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 요청 제한: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 오류 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint는 sendMessage 주소를 만듭니다. base_url이 비면 공식 API,
// 값이 있으면 자체 Bot API 리버스 프록시를 사용합니다(중국 네트워크에서 자주 필요).
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// Bot Token이 포함되므로 err뿐 아니라 addr도 그대로 노출하지 않습니다.
		return "", fmt.Errorf("API 주소 조합 실패(API 주소: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML은 HTML 본문과 실제 포함한 항목 수를 반환합니다(Channel.Send 참조).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram 한도는 문자 수이므로 묶음도 runeSize로 측정합니다.
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">플랫폼에서 전체 보기</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>상태 변경</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>유형</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>자산</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>요약</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">상세 보기</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes는 제목과 잘림 안내에 예약할 문자 수입니다.
const telegramReservedRunes = 160

// telegramBatchLine은 요약 항목 하나를 렌더링하며 호출자가 한꺼번에 이스케이프합니다.
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle은 요약 제목에 전체 묶음 수가 아니라 실제 포함한 수를 표시해
// 사용자가 머리말 숫자를 전부라고 오해하지 않게 합니다.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("취약점 요약 · 총 %d개", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("(앞 %d개 표시, 나머지 %d개는 다음 메시지에 계속)", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("최근 %d분 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape는 HTML 텍스트를 이스케이프합니다.
// Telegram이 인식하는 세 엔터티를 처리하며 기존 &amp;도 다시 이스케이프합니다.
// 원본 문자를 표시하고 사용자의 HTML 주입을 막기 위한 의도된 동작입니다.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr는 HTML 속성 값을 이스케이프하며 텍스트 처리 외에 따옴표도 다룹니다.
// URL의 따옴표가 href를 조기에 닫아 뒤 내용을 주입 지점으로 만들지 않도록 합니다.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
