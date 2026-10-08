package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit는 WeCom 그룹 봇 markdown content의 바이트 한도(문자 수 아님)입니다.
// 여섯 채널 중 가장 엄격하며 TruncateBytes가 필요한 주된 이유입니다.
const weComMarkdownLimit = 4096

// weComChannel은 WeCom 그룹 봇을 구현합니다.
//
// 플랫폼 특성:
//   - URL의 key만으로 인증하고 서명을 지원하지 않아 webhook 주소가 전체 자격 증명입니다.
//   - markdown content는 4096바이트를 넘으면 잘리는 대신 전체 거부됩니다. 한글/한자는 보통 3바이트라
//     천여 자만 쓸 수 있으므로 클라이언트에서 잘라야 합니다.
//   - 분당 20개 제한도 클라이언트에서 적용합니다.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom은 Webhook URL의 key만 자격 증명이며 서명을 지원하지 않으므로
// 전체 주소 외에 마스킹할 필드가 없습니다.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// Webhook 하나가 목적지이자 자격 증명이므로 주소 변경 후 남는 별도 자격 증명은 없습니다.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("유효하지 않은 Webhook 주소: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 50개 항목과 접두사로 된 긴 요약은 4096바이트를 쉽게 넘습니다.
	// 플랫폼 거부로 전체를 잃기보다 여기서 잘라 앞의 몇 항목이라도 전달합니다.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("WeCom 응답 해석 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009는 요청 제한이며 이동 시간 구간이 지나면 백오프 재시도가 유효하므로
		// 명시적으로 재시도 가능으로 분류합니다. 클라이언트 rate_per_min이 과도하다는 뜻이며
		// 재시도는 보완책일 뿐 실제 해결은 채널 제한값을 낮추는 것입니다.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom 요청 제한 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000은 유효하지 않은 webhook key로 재시도로 해결되지 않는 영구 실패입니다.
		return 0, Permanent(fmt.Errorf("WeCom 오류 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
