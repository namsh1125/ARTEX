package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 추론만 있고 본문/도구가 없는 빈 턴의 판정과 재개. steerHooks.Stop 참고.

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "먼저 포트 스캔"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"추론만 있음", []llm.Message{llm.UserText("시작"), assistantThinking("생각하기")}, true},
		{"추론과 도구", []llm.Message{llm.UserText("시작"), toolUse}, false},
		{"추론과 본문", []llm.Message{assistantThinking("생각하기"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("결론")},
		}}, false},
		{"본문에 공백만 있음", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"완전히 빈 assistant 턴", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// 도구 결과는 user 역할이므로 직전 assistant까지 되돌아가 판정해야 한다.
		{"마지막 메시지가 도구 결과", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant 메시지 없음", []llm.Message{llm.UserText("시작")}, false},
		{"빈 기록", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks는 steerHooks가 inner의 결정을 존중하는지 검증하는 프로그래밍 가능한 HookRunner다.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("먼저 하위 도메인을 열거해야 한다")}

	t.Run("빈 턴에 재개 지시 삽입", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("빈 턴을 강제로 중지하면 안 됨")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("본문이나 도구가 있으면 개입 안 함", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("스캔을 마쳤으며 열린 포트가 없습니다")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("정상 종료를 빈 턴으로 오판: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("개입하지 않으면 횟수가 증가하면 안 됨, got %d", n)
		}
	})

	t.Run("상한 도달 후 종료 허용", func(t *testing.T) {
		const limit = 5 // 사용자가 빈 응답 재시도 횟수를 5로 지정
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d번째는 예산 내여야 함, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("상한 초과 후에도 삽입함: %v", blocking)
		}
	})

	// 빈 응답 재시도 -1은 비활성이며 emptyTurnNudgeLimit가 0으로 해석한다.
	t.Run("비활성 설정이면 개입 안 함", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("비활성인데 삽입함: %v", blocking)
		}
	})

	t.Run("inner 강제 중지 결정에 추가 안 함", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard가 종료 거부"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard가 종료 거부" || blocking != nil {
			t.Fatalf("inner 강제 중지 결과 변경: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner에 맡기면 예산을 소모하면 안 됨, got %d", n)
		}
	})

	t.Run("inner가 재개하면 추가 안 함", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard 재개 사유"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard 재개 사유" {
			t.Fatalf("inner 재개 메시지 변경: %v", blocking)
		}
	})

	t.Run("카운터 없으면 기존 동작 유지", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 향후 다른 호출자가 nudges를 빠뜨린 경우 등
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("카운터 없이는 삽입하면 안 됨: %v", blocking)
		}
	})
}
