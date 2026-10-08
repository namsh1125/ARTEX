package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "모델이 이번 실행을 정상 종료했지만 텍스트 요약을 남기지 않았습니다. 사실과 자산은 이번 도구 호출 기록을 기준으로 확인하세요",
	harness.ReasonMaxTurns:          "단계 한도(MaxTurns)에 도달했습니다. SDK가 마무리하며 사실과 자산을 기록했고 의도는 실패가 아닌 exhausted로 표시됩니다. 계획자가 방향을 바꾸어 이어갈 수 있습니다",
	harness.ReasonTimeout:           "단일 실행 시간 한도(MaxDuration)에 도달했습니다. 실행 중인 도구를 중단하고 즉시 마무리해 확인된 사실과 자산을 기록합니다. 의도는 exhausted로 표시됩니다",
	harness.ReasonModelError:        "모델/API 호출 실패(네트워크, 인증, 요청 제한, 공급자 5xx 등). 재시도를 소진하면 의도가 blocked로 표시됩니다. 전송 계층 장애로 실제 탐색이 거의 수행되지 않았으므로 get_worker_trace로 실행 과정을 확인한 뒤 같은 방향 재할당 또는 방법 변경을 결정하세요",
	harness.ReasonBlockingLimit:     "컨텍스트 길이가 강제 한도에 도달해 요청 전송 전에 차단되었습니다. 의도의 범위를 좁히거나 도구 반환값을 압축하세요",
	harness.ReasonPromptTooLong:     "프롬프트가 너무 길고 컨텍스트 압축 재시도를 소진해 계속 실행할 수 없습니다",
	harness.ReasonImageError:        "현재 모델이 이번 멀티모달 내용을 지원하지 않습니다. 시각 지원 모델로 바꾸거나 도구의 이미지 반환을 피하세요",
	harness.ReasonStopHookPrevented: "Stop 훅이 종료를 막았으며 이후 계속하지 못했습니다. 작업 Guard 규칙이 지나치게 엄격한지 확인하세요",
	harness.ReasonHookStopped:       "도구 또는 훅이 실행을 중단했습니다(예: 범위 밖 대상, 비활성화된 명령). 마지막 tool_result의 차단 설명을 확인하세요",
	harness.ReasonAbortedStreaming:  "모델의 스트리밍 출력 생성 단계에서 실행이 취소되었습니다",
	harness.ReasonAbortedTools:      "도구 실행 단계에서 실행이 취소되었습니다",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "취소 이유를 가져오지 못했습니다"
		}
		stage := "실행 중"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "모델 출력 단계"
		case harness.ReasonAbortedTools:
			stage = "도구 실행 단계"
		}
		sum = "(실행 중단: " + short + "; 중단 지점: " + stage + progressSuffix(term, tr) + ", 미완료)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(실행 예산 한도(" + string(reason) + ") 도달, 마무리하며 사실 기록 완료" + progressSuffix(term, tr) + "; 텍스트 요약 없음)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(텍스트 요약 없음, 최종 상태 " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **최종 상태**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **중단 이유** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **중단 이유**: 가져올 수 없습니다. 취소 측에서 context.WithCancelCause로 명명된 이유를 첨부하지 않았을 수 있습니다\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **하위 계층 오류**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **취소 전 생성된 부분 출력**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **실행 완료**: 모델 턴 %d회\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **이번 실행 소요 시간**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **누적 토큰**: 입력 %d / 출력 %d / 캐시 읽기 %d / 캐시 쓰기 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **도구 호출**: 이번 실행은 도구 호출 전에 종료되었습니다\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **중단 당시 실행 중인 도구**: `%s`(%s 경과, **결과 미반환**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **중단 전 마지막 도구**: `%s`(정상 반환됨)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "실행 context가 취소되었지만 하위 계층에서 Terminal 이벤트가 생성되지 않았습니다"
	}
	return "알 수 없는 최종 상태입니다. harness에 TerminalReason이 추가되었을 수 있으므로 reasonHint를 보완하세요"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d회", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", 실행 경과 " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
