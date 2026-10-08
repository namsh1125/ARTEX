// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType은 추론 활성화 필드(thinking.type)를 독립적으로 제어합니다.
	//   "" = 전송하지 않음(기본, 미지원 모델 호환); "disabled" = 명시적 비활성화;
	//   "enabled" = 활성화. ReasoningEffort와 완전히 독립적입니다. 일부 API는 thinking 필드 없이
	//   강도 매개변수만으로 추론을 활성화하므로 각각 따로 설정할 수 있습니다.
	ThinkingType string
	// ReasoningEffort는 추론 강도 필드를 독립적으로 제어합니다.
	//   "" = 전송하지 않음(기본); "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도.
	//   OpenAI는 최상위 reasoning_effort, Anthropic은 output_config.effort로 매핑합니다.
	ReasoningEffort string
	// Stream은 프로필의 스트리밍(SSE) 사용 여부입니다. true(기본)=스트리밍, false=
	// 실제 비스트리밍(stream:false로 전체 JSON을 한 번에 받아 Provider.Complete 사용). 비스트리밍은
	// 일부 게이트웨이의 잘못된 SSE 구현(빈 프레임, 추론 필드 유실)을 피하지만 실행 중 실시간 진행과
	// 토큰 수 표시를 잃습니다. agentcore.Options.NonStreaming = !Stream으로 매핑합니다.
	Stream bool
	// MaxTokens는 응답 하나의 출력 토큰 한도입니다. 0이면 필드를 보내지 않고 서버 기본값을 따릅니다
	// (기존 동작). ContextWindowK는 모델 전체 용량으로 로컬 압축 임계값 계산에만 쓰고 요청에는
	// 포함하지 않습니다. MaxTokens는 요청마다 전달하며 agentcore.Options.MaxTokens로 매핑합니다.
	MaxTokens int
	// MaxTokensField는 MaxTokens의 요청 필드명을 선택하며 format=openai에만 적용합니다.
	//   "" = max_tokens(기본); "max_completion_tokens" = 새 필드.
	// OpenAI 추론 모델(o 시리즈/GPT-5)은 후자만 인식하며 max_tokens를 받으면
	// unsupported_parameter를 반환합니다. 많은 호환 게이트웨이는 전자만 지원하므로 자동 추론하지 않고 사용자가 엔드포인트에 맞춰 선택합니다.
	MaxTokensField string
	// SessionHeaderKey가 비어 있지 않으면 각 LLM 요청에 이 이름의 사용자 정의 HTTP 헤더를 넣습니다.
	// 값은 현재 대화의 session id입니다(chat=conv-<id>, worker=exp<x>-worker-i<intent>
	// 등, WorkerSessionID 참고). session-id 헤더로 프롬프트 캐시/고정 라우팅을 하는 게이트웨이에 사용합니다.
	// 비어 있으면 보내지 않습니다. transcript.WithSessionID가 요청 context에 넣은 값을 RoundTripper가
	// 읽어 채우므로 같은 공유 provider에서도 대화마다 다른 헤더 값을 보낼 수 있습니다.
	SessionHeaderKey string
	// Retry는 해석된 재시도 매개변수입니다(프로필 재정의 → 전역 정책 → 내장 기본값,
	// server에서 해석). 세 계층의 의미는 RetryConfig 참고. 제로 값이면 내장 기본값을 그대로 사용합니다.
	Retry RetryConfig
}

// RetryConfig는 LLM 설정별 재시도 매개변수입니다. 각 계층의 횟수 의미는 동일합니다.
// 0=내장 기본 횟수, 음수=해당 계층 재시도 비활성화, 양수=지정 횟수. 간격은
// 0=기존 지수 백오프, 양수=지정 고정 간격입니다.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK 연결 재시도(스트림 시작 전 연결 재설정/시간 초과/429/5xx),
	// llm.Config.MaxRetries / RetryInterval에 직접 매핑. 기본 3회, 0.5초부터 지수 증가(최대 8초).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK 빈 응답 재시도(완료했으나 content block이 없음, openai
	// 형식만 적용). llm.Config.EmptyResponseRetries / EmptyResponseInterval에 매핑합니다.
	// 기본 2회이며 동일한 지수 백오프를 사용합니다.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: 같은 provider의 안전 구간 재시도. SDK 위에 추가한 계층으로
	// 호출자에게 아직 출력을 전달하지 않았을 때만 스트림 중단/과부하/스트림 내부 429를 재시도합니다. SDK에서는 보이지 않으며
	// server/task_llm.go가 사용합니다. 기본 2회, 0.5초부터 지수 증가(최대 4초).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// 기본은 스트리밍입니다. ARTEX_LLM_STREAM=false/0/off로 명시적으로 끄면 비스트리밍을 사용합니다.
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 기본 스트리밍이며 호출자가 프로필에 따라 재정의합니다.
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 추론 활성화와 강도는 각각 전달합니다(비어 있으면 해당 필드 생략). 서로 독립적이므로
	// thinking.type만, effort만, 둘 다 또는 둘 다 없이 보낼 수 있습니다.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 출력 한도의 필드명 선택(비어 있으면 max_tokens). 한도 값은 여기서 정하지 않고 매 턴
	// agentcore.Options.MaxTokens로 전달합니다. provider는 어떤 키에 넣을지만 결정합니다.
	lc.MaxTokensField = c.MaxTokensField
	// 재시도 매개변수는 SDK와 같은 의미(횟수 0=기본/음수=끄기, 간격 0=지수 백오프/양수=고정)로 그대로 전달합니다.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 비어 있으면 직접 연결하며 HTTP_PROXY/HTTPS_PROXY 환경 변수로 대체하지 않습니다.
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "테스트 클릭" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP 요청을 보내지 못했습니다(설정 해석 또는 연결 단계 실패)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 시도 %d/%d HTTP %d\n응답 본문: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(비어 있음)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(잘림, 전체 %d바이트)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 원본 전송 메시지를 캡처합니다. 연결 테스트에서는 게이트웨이가 실제 반환한 상태 코드와 본문이 중요하지만
	// norma가 응답을 StreamEvent로 디코딩하면 사라집니다. quotaAwareTransport가 context의
	// Capture를 찾아 HTTP 시도마다 상태 코드와 본문을 기록합니다.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 연결 테스트는 agentcore 대화 루프를 거치지 않는 일회성 호출이므로 context에
	// session id를 설정하는 곳이 없습니다. SessionHeaderKey를 설정한 엔드포인트(예: opencode zen은
	// x-opencode-session이 없으면 400 MissingSessionID 반환)에서는 대화는 정상인데
	// 연결 테스트는 400을 반환할 수 있습니다. 일회용 임의 session id를 넣어 실제 대화와 같은
	// 헤더 전송 로직을 사용합니다. SessionHeaderKey가 없는 엔드포인트는 읽지 않으므로 영향이 없습니다.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens는 충분해야 합니다. 추론 모델(예: deepseek-v4-pro)은 답하기 전에 긴 추론을 생성합니다.
	// 실제 측정에서는 ping 하나에도 약 2900토큰을 사용했습니다. 32만 주면 추론 중에 출력 한도에
	// 도달해 잘리며(finish=length), 연결은 성공(err=nil)해도 화면에는 중단/length/resume처럼
	// 불완전하게 표시됩니다. OK를 정상 출력하도록 충분한 예산을 제공합니다(finish=stop).
	// EscalateMaxTokens는 false로 유지해 잘림에 따른 한도 증가 재시도와 resume 반복 낭비를 피합니다.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"연결 테스트입니다. 추론이나 설명 없이 OK 두 글자만 출력하세요."},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // 프로필의 실제 송수신 모드로 연결을 테스트합니다.
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil만으로는 부족합니다. 요청은 성공했지만 추론이 예산을 소진하거나 정책이 본문을 제거하거나
	// 호환 계층이 content를 유실해 아무 텍스트도 나오지 않을 수 있습니다. 대화에서는 답하지 않는 설정을
	// 테스트가 성공으로 보고하지 않도록 보이는 본문이 없으면 실패로 판정합니다.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("모델 응답 내용이 없습니다(요청은 성공했지만 텍스트를 반환하지 않았습니다)")
	}
	return lat, reply, nil
}
