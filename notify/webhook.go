package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel은 URL, 메서드, 헤더, JSON 템플릿을 지정할 수 있는 일반 Webhook 어댑터입니다.
// Slack / Mattermost / Discord / 자체 시스템마다 구현을 따로 만들 필요 없이
// 설정 가능한 템플릿으로 지원합니다.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 일반 Webhook에는 공식 제한이 없어 0(기본 무제한)을 반환하며 상대 서버에 맞춰 사용자가 설정합니다.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url과 headers는 토큰과 인증 정보가 자주 포함되므로
// API 응답에서 둘 다 마스킹합니다.
// 헤더 하나를 바꾸려면 전체 헤더를 다시 입력해야 하지만(마스킹 값은 기존 값 유지)
// 브라우저에 자격 증명을 노출하지 않기 위한 의도적인 선택입니다.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 목적지는 url입니다. 바꿀 때 headers를 다시 명시하지 않으면 기존 Authorization이
// 새 주소로 전송되어 마스킹을 우회할 수 있습니다.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate은 템플릿 미지정 시 사용하는 단순한 JSON 본문으로
// JSON을 받아 저장하는 대부분의 자체 수신 서버를 지원합니다.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData는 사용자 템플릿에 공개하는 컨텍스트입니다.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt은 수신 서버가 기록할 이번 전달 시간(RFC3339)입니다.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel은 처리 대기 → 수정됨 같은 읽기 쉬운 상태 변경 설명이며 다른 이벤트에서는 비어 있습니다.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL이 없습니다")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("유효하지 않은 대상 URL: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드 %s(GET/POST/PUT/PATCH 사용 가능)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 구문 오류: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET은 본문을 보내지 않습니다. 내용을 query에 넣는 것은 템플릿 기능과 GET 의미에 맞지 않아
	// GET은 요청 도착만으로 훅을 유발하는 수신 서버에 적합합니다.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// 렌더링한 JSON 문자열을 json.RawMessage로 변환해 그대로 보내며
		// 이중 이스케이프로 사용자 구조가 JSON 문자열 안에 다시 포장되지 않게 합니다.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿 결과가 유효한 JSON이 아닙니다"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 재정의는 허용하되 headers 이후 적용해 명시적 설정을 우선합니다.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 일반 Webhook은 사용자 서버가 수신하고 body_template이 크기를 결정하므로 본문을 자르지 않으며
	// 전체 묶음을 전달 완료로 간주합니다.
	return len(m.Items), nil
}

// renderWebhookBody는 사용자 또는 기본 템플릿으로 요청 본문을 렌더링합니다.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 구문 오류: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 렌더링 실패: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate은 템플릿을 해석합니다.
//
// missingkey=zero는 누락된 map 키를 오류 대신 제로 값으로 만듭니다. 여기의 컨텍스트는 구조체이며
// 주로 .Items가 비었을 때 range 오류를 피하려는 목적입니다. 실제로 주의할 값은 nil .Items입니다.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs는 템플릿에 공개하는 보조 함수입니다.
var webhookTemplateFuncs = template.FuncMap{
	// json은 임의 값을 JSON으로 직렬화합니다.
	//
	// 선택적인 편의 기능이 아니라 필수입니다. 없으면 {{.Title}}을 직접 보간하게 되고
	// 취약점 제목에 따옴표/줄바꿈이 있을 때 본문이 잘못된 JSON이 되어 수신자가 거부합니다.
	// 오류는 JSON 해석 실패라고만 표시되어 제목의 따옴표가 원인임을 알기 어렵습니다.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons는 JSON 조각을 다른 JSON 문자열 값에 넣기 위한 한 단계 문자열 이스케이프입니다.
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 바깥 따옴표는 제거하고 호출자가 추가 여부를 결정합니다.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
