package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "사용자가 작업을 일시 중지했습니다",
		"사용자가 작업 제어 API(POST /api/tasks/{id}/control, action=pause)로 작업을 일시 중지했습니다. 이번 Planner/Worker 실행은 취소되며 실행 중인 의도는 frontier(open)로 돌아갑니다. 작업을 재개하면 다시 할당받아 처음부터 실행합니다")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "오케스트레이션 Agent가 작업을 일시 중지했습니다",
		"오케스트레이션 Agent가 pause_task 도구로 이 작업을 일시 중지했습니다. 이번 Planner/Worker 실행은 취소되며 실행 중인 의도는 frontier(open)로 돌아갑니다. 재개 후 다시 실행합니다")
	AbortTaskDeleted = cause("task_deleted", "작업이 삭제되었습니다",
		"작업 삭제 중입니다(DELETE /api/tasks/{id}). 삭제 장벽이 해당 작업의 실행 중인 Planner, Worker, 주 Agent를 취소했습니다. 이번 실행 결과는 더 이상 사용하지 않습니다")
	AbortPausedOnReload = cause("paused_on_reload", "백엔드가 작업의 일시 중지 상태를 복원했습니다",
		"백엔드 시작 시 데이터베이스에 저장된 작업의 일시 중지 상태를 복원했습니다. 이번 실행은 취소되었습니다. 정상적인 복원 단계에서는 실행 중인 Agent가 없습니다")
	AbortGoalMet = cause("goal_met", "계획자가 작업 목표 달성을 판정했습니다",
		"계획자가 작업 목표가 달성되었다고 판단해 작업을 done으로 바꾼 뒤 실행 중인 Worker를 취소했습니다. 이 의도들은 실패가 아닌 stopped로 표시됩니다")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "작업 시간 초과 후 마무리 대기 시간을 소진했습니다",
		"작업이 timeout에 도달한 후 실행 중인 Worker의 정상 종료를 기다렸지만 90초의 drain 유예 시간으로도 부족해 강제 취소했습니다. 의도는 exhausted로 표시되며 마무리 단계에 기록된 사실과 자산은 유지됩니다")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "계획자가 이 의도를 종료했습니다",
		"계획자가 kill_work로 이 의도를 종료했습니다. 일반적으로 방향이 잘못되었거나 계속할 가치가 없다는 뜻입니다. 의도는 stopped로 표시되며 자동으로 다시 할당되지 않습니다")
	AbortWorkPausedByUser = cause("work_paused_by_user", "사용자가 이 Worker 의도를 일시 중지했습니다",
		"사용자가 실행 중인 Worker를 일시 중지했습니다. 이번 호출은 취소되고 의도는 paused가 됩니다. 등록된 의도, 사실, 취약점, 활동 기록은 모두 유지되며 재개 시 처음부터 실행합니다")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "사용자가 이 Worker 의도를 삭제했습니다",
		"사용자가 실행 중인 Worker를 삭제했습니다. 이번 호출은 취소됩니다. Worker가 쓰기 영역을 벗어나면 서버가 선택한 삭제 방식에 따라 처리합니다. 논리 삭제는 삭제 표시만 하고 모든 산출물을 유지하며, 영구 삭제는 해당 의도와 오직 그 의도에만 의존하는 하위 노드를 함께 제거합니다")
	AbortWorkFinished = cause("work_finished", "Worker가 정상 종료하고 context를 해제했습니다",
		"Worker가 정상 종료하여 엔진이 detachWork에서 context 자원을 해제했습니다. 실행 중단이 아닙니다. 중단 메시지에 나타난다면 취소와 마무리 이벤트 사이에 경합이 발생한 것입니다")
	AbortPausedRaceGuard = cause("paused_race_guard", "작업 일시 중지 중 새 실행을 거부했습니다",
		"작업이 일시 중지된 동안 엔진은 새 실행 context 발급을 거부합니다. claim과 일시 중지 사이의 경합으로 Worker가 계속 시작되는 것을 방지합니다. 이미 할당된 의도는 frontier로 돌아갑니다")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "사용자가 이번 대화를 중지했습니다",
		"사용자가 중지를 눌러 이번 주 Agent 또는 대화 Agent 실행을 중단했습니다. 이미 생성된 활동 기록은 유지되며 다음 메시지를 보낼 수 있습니다")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "작업 일시 중지와 함께 주 Agent 대화를 중단했습니다",
		"사용자가 작업을 일시 중지하면서 실행 중인 주 Agent 대화도 함께 취소되었습니다. 이미 생성된 활동 기록은 유지되며 작업을 재개해도 이번 메시지를 자동으로 다시 실행하지 않습니다")
	AbortChatTurnFinished = cause("chat_turn_finished", "이번 대화가 정상 종료하고 context를 해제했습니다",
		"이번 대화가 정상 종료하여 서버가 해당 차례의 context 자원을 해제합니다. 실행 중단이 아닙니다. 중단 메시지에 나타난다면 취소와 마무리 이벤트 사이에 경합이 발생한 것입니다")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "백엔드 프로세스가 종료 중입니다",
		"백엔드 프로세스가 SIGINT 또는 SIGTERM을 받아 재시작, 업데이트 또는 종료 중입니다. 실행 중인 모든 Agent는 취소됩니다. 재시작 후 남아 있는 running 의도는 open으로 초기화되어 다시 실행됩니다")
	AbortRunHardTimeout = cause("run_hard_timeout", "단일 실행의 강제 시간 제한에 도달했습니다",
		"단일 실행이 소프트 실행 시간 한도와 추가 유예 시간을 초과했습니다. 모델 요청이나 도구가 오랫동안 반환하지 않아 정상적인 턴 경계 마무리를 수행하지 못했다는 뜻입니다. 중단 직전 마지막으로 반환하지 않은 도구 호출을 확인하세요")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "상위 context가 deadline에 도달했습니다",
			"상위 context가 deadline에 도달했지만 설정 측에서 WithTimeoutCause로 명명된 원인을 첨부하지 않았습니다: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "취소 측에서 명명된 원인을 첨부하지 않았습니다",
			"상위 context가 취소되었지만 취소 측에서 context.WithCancelCause로 명명된 원인을 첨부하지 않았습니다. agent/cancelcause.go에 원인을 등록하고 해당 취소 지점에 연결하세요", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
