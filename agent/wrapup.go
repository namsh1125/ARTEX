package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 마무리 프롬프트(wrap-up / settlement prompt): Agent가 단계 소진(MaxTurns) 또는
// 시간 초과(run_seconds/MaxDuration)로 종료될 때 SDK settlement 단계에서 주입합니다.
// 이미 확인했으나 기록하지 않은 내용을 먼저 저장하고 한 문장으로 요약해 미완료 상태로 끝나지 않게 합니다.
//
// Agent별 마무리 프롬프트는 관리 화면에서 agents.wrapup_prompt로 재정의하며 비어 있으면
// 내장 기본값을 사용합니다. 본문만 편집 가능하고 비활성화 도구와 마무리 턴 예산은 코드 정책입니다.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Agent 키별 기본 마무리 프롬프트입니다. Worker는 기존 settleWrapUpPrompt(worker.go)를
// 재사용하고 planner/mainagent는 각각 별도 버전을 사용하며 사용자 정의 Agent는 공통 기본값을 사용합니다.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: Agent별 마무리 단계 자체의 턴 예산 기본값(관리 화면의 양수 값으로 재정의 가능).
// 저장할 충분한 단계를 보장하기 위해 모두 10턴이며 키가 없으면 genericWrapupTurns를 사용합니다.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "이번 계획 실행의 단계가 곧 소진됩니다. 이번 실행만 끝나는 것이며 시스템이 현황 변화에 따라 다시 깨우므로 작업 전체를 종료하거나 계획 전체를 마무리할 필요는 없습니다. 이번에 결정한 내용을 반영하되 마무리를 위해 의도를 억지로 만들지 마세요(의도 0개도 정상). (1) 지금 내릴 탐색 방향을 결정했다면 add_intent 한 번으로 일괄 제출하세요. (2) 발견 사항/사실로 달성된 목표는 빠짐없이 prove_goal로 met 표시하세요. (3) 단계별 직렬 악용 체인은 TodoWrite에 기록해 다음 실행에서 이어가세요. 이후 이번 실행을 종료하며 텍스트 요약은 필요 없습니다."

const mainAgentWrapUpDefault = "단계 한도가 곧 소진되어 이번 상호작용이 끝납니다. 새 탐색/작업을 시작하지 마세요. 별도의 일반 텍스트 한 문장으로 현재 진행 상황, 핵심 결론, 권장하는 다음 단계를 사용자에게 요약하세요."

const genericWrapUpDefault = "곧 예산 소진으로 종료됩니다. 완료했으나 저장하지 않은 결과를 먼저 기록한 뒤 별도의 일반 텍스트 한 문장으로 수행 내용과 핵심 결론을 요약하세요(이번 실행 결과로 표시됩니다)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- 작업 수준 시간 초과 마무리 문구(docs/任务级超时与收尾设计.md 참고) ----------
//
// 실행별 마무리와는 별개입니다. 실행별 문구는 이번 실행의 예산 소진을 의미하고 작업 시간 초과는
// 전체 작업 시한 도달을 의미합니다. 특히 planner에서 전자는 계속 계획하라는 뜻이고
// 후자는 계획을 멈추고 최종 판정을 하라는 뜻입니다. worker/planner에만 설정합니다.

// WrapupTaskTimeoutOverride / …TurnsOverride: 작업 시간 초과 마무리 문구와 턴 수의 DB 재정의
// (agents.task_timeout_wrapup_prompt / _max_turns에 연결, worker/planner만 적용).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "전체 작업이 시간 제한에 도달해 곧 종료됩니다(이번 실행만의 예산이 아니라 전체 탐색 시한입니다). 마지막 기회입니다. (1) 확인했으나 기록하지 않은 내용을 모두 저장하세요. 새 자산은 insert_assets, 탐색 결론/사실은 record_fact, 확인된 취약점은 report_finding을 사용합니다. (2) 새 명령/탐색을 시작하지 마세요. (3) 마지막에 별도의 일반 텍스트 한 문장으로 이 의도의 핵심 결론을 요약하세요."

const plannerTaskTimeoutDefault = "전체 작업이 시간 제한에 도달해 곧 종료됩니다(이번 실행이 아니라 전체 작업 종료). 현재의 모든 사실과 발견 사항으로 마지막 목표 판정을 수행하세요. 증거로 달성된 목표를 빠짐없이 prove_goal로 met 표시하세요. 새 의도는 더 이상 생성하지 마세요(생성해도 실행되지 않습니다). 판정 후 마무리하며 텍스트 요약은 필요 없습니다."

// TaskTimeoutWrapupDefault는 Agent의 작업 시간 초과 기본 마무리 문구를 반환합니다(관리 화면 예시/기본값 복원용).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 설정 없는 mainagent/chat은 빈 문자열 반환
}

// resolveTaskTimeoutWrapup: 비어 있지 않은 DB 재정의 > 내장 기본값. 빈 문자열이면 작업 시간 초과
// 문구가 없는 Agent(worker/planner 외)이므로 호출자는 실행별 문구로 대체해야 합니다.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 기본적으로 실행별 턴 수 재사용
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true → 실행이 작업 deadline으로 제한됨: Timeout이면 작업 시한 문구 사용.
//     MaxTurns면 작업 시간이 남아 있어도 단계부터 소진했으므로 실행별 문구 사용.
//   - clamped=false → 작업 시한까지 여유가 있으므로 두 reason 모두 실행별 문구 사용(wrapupSettlement와 동일).
//
// harness의 PromptByReason이 마무리 당시 실제 reason으로 선택해 생성 시점의 불일치를 방지합니다.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // 기본값(clamped가 아닐 때 두 reason 모두 사용)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // 작업 시한 도달
				harness.ReasonMaxTurns: perRun, // 단계 먼저 소진, 작업 시간은 남음
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
