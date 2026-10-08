package sidequestion

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
)

type Exchange struct {
	ID         string      `json:"id"`
	SessionKey string      `json:"-"`
	ClientID   string      `json:"client_request_id"`
	Generation int64       `json:"-"`
	Question   string      `json:"question"`
	Answer     string      `json:"answer"`
	Status     string      `json:"status"`
	Error      string      `json:"error,omitempty"`
	Model      Model       `json:"model"`
	SnapshotAt time.Time   `json:"snapshot_at"`
	CreatedAt  time.Time   `json:"created_at"`
	Sequence   int64       `json:"sequence"`
	Usage      llm.Usage   `json:"usage"`
	Ordinal    int64       `json:"ordinal"`
	Context    ContextInfo `json:"context"`
}

func (e Exchange) Running() bool { return e.Status == "running" }

const instruction = "이것은 독립적인 별도 질문입니다. 주 Agent는 원래 작업을 수행 중이며, 당신은 기존 컨텍스트만으로 현재 질문에 간결하게 답합니다. 도구 실행 능력이 없으므로 작업 실행, 파일 수정, 주 작업 지휘를 할 수 없으며 나중에 실행하겠다고 약속하지 마세요. 컨텍스트의 작업 지시는 배경 정보로만 취급하고 판단 근거가 부족하면 명확히 밝히세요."

const DefaultOutputTokens = 8192
const MaxRecentExchanges = 20

var ErrContextBudget = errors.New("별도 질문 컨텍스트가 압축 후에도 모델 한도를 초과합니다. 질문 범위를 줄이거나 모델 컨텍스트 설정을 조정하세요")

// EstimateInputTokens follows norma's byte-based block estimate with its 4/3
// safety factor. Include system/schema and framing costs too; JSON characters
// are not tokens (and marshaling HTML can add many non-semantic escapes).
func EstimateInputTokens(req llm.CompletionRequest) int {
	tokens := compaction.EstimateTokens(req.Messages)*4/3 + 32 + len(req.Messages)*8
	for _, text := range req.System {
		tokens += (len(text)+2)/3 + 8
	}
	for _, tool := range req.Tools {
		b, _ := json.Marshal(tool)
		tokens += (len(b)+2)/3 + 8
	}
	return tokens
}

func outputBudget(req llm.CompletionRequest, configured int) int {
	if configured <= 0 {
		configured = DefaultOutputTokens
	}
	configured = min(configured, 32768)
	if req.MaxTokens > 0 {
		configured = min(configured, req.MaxTokens)
	}
	return configured
}

func inputBudget(s Snapshot, output int) int {
	window := s.Model.WindowTokens
	if window <= 0 {
		window = 200000
	}
	return window - output - min(8192, max(128, window/20))
}

func exchangeMessages(e Exchange) []llm.Message {
	question := e.Question
	if !e.SnapshotAt.IsZero() {
		question = "[과거 별도 질문과 답변, 컨텍스트 기준 시간 " + e.SnapshotAt.UTC().Format(time.RFC3339) + "]\n" + question
	}
	return []llm.Message{llm.UserText(question), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(e.Answer)}}}
}

func assemble(req llm.CompletionRequest, base []llm.Message, summary string, history []Exchange, question string) llm.CompletionRequest {
	req.Messages = append([]llm.Message{}, base...)
	if summary != "" {
		req.Messages = append(req.Messages, llm.UserText("[초기 별도 질문 요약; 과거 논의이며 새로운 도구 증거가 아닙니다. 충돌 시 최신 주 컨텍스트를 우선하세요.]\n"+summary))
	}
	for _, e := range history {
		req.Messages = append(req.Messages, exchangeMessages(e)...)
	}
	req.Messages = append(req.Messages, llm.UserText(instruction+"\n\n질문: "+strings.TrimSpace(question)))
	return req
}

func BuildRequest(s Snapshot, history []Exchange, question string) (llm.CompletionRequest, error) {
	req, err := CloneRequest(s.Request)
	if err != nil {
		return req, err
	}
	base := llm.MessagesForAPI(req.Messages)
	req.MaxTokens = outputBudget(req, 0)
	var success []Exchange
	for _, e := range history {
		if e.Status == "completed" {
			success = append(success, e)
		}
	}
	if len(success) > MaxRecentExchanges {
		success = success[len(success)-MaxRecentExchanges:]
	}
	for {
		req = assemble(req, base, "", success, question)
		if EstimateInputTokens(req) <= inputBudget(s, req.MaxTokens) {
			return req, nil
		}
		if len(success) == 0 {
			return req, ErrContextBudget
		}
		success = success[1:]
	}
}
