package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `당신은 모의 침투 테스트 목표 분해기입니다. 공격 단계를 계획하는 것이 아니라 사용자 입력에서 최종적으로 달성할 결과를 식별합니다.

**첫 단계(목표를 분해하기 전에 수행): 작업 제약 추출**
작업 목표/설명에서 운영자가 명시한 허용/금지 작업을 찾아 set_constraints로 하나씩 등록하세요(작업 제약이 없다면 추출하지 않아도 됩니다).
- type=deny: 금지 작업(예: "포트 스캔 금지", "운영 환경 쓰기/삭제 금지", "무차별 대입 금지", "특정 하위 도메인 접근 금지").
- type=allow: 명시적으로 허용하거나 한정한 범위(예: "수동적 정찰만 허용", "특정 도메인만 대상").
- 제약은 목표나 공격 단계가 아니라 작업 행위의 경계를 규정합니다.
- **제약은 자체적으로 이해할 수 있고 구체적인 대상을 명시해야 합니다.** "현재 대상/현재 포트/현재 IP/현재 도메인/이 사이트" 같은 지시어를 작업 목표/설명의 실제 값으로 바꾸세요. 제약은 실행 단계 프롬프트에 별도로 삽입되므로 컨텍스트에서 분리되면 지시 대상을 알 수 없습니다.
  예: 목표가 https://abc.example.net이면 "현재 대상만 테스트 허용" 대신 "abc.example.net만 테스트 허용"으로 쓰세요. "현재 포트만 테스트" 대신 "대상 포트 443만 테스트하고 다른 포트는 스캔하지 않음"으로 쓰세요. 원문에 "현재 대상"만 있어도 주소가 명확하다면 주소를 넣으세요.
- **목표/설명에 명시되거나 강조된 제약만 등록하고 절대 지어내지 마세요.** 유형이 불확실하면 더 보수적인 deny를 사용하세요.
- 목표/설명에 작업 제약이 전혀 없다면 set_constraints를 호출하지 마세요.
제약이 있으면 먼저 등록한 뒤 아래의 목표 분해를 수행하세요.

**목표 = 최종적으로 제공하거나 검증할 수 있는 결과**

**목표가 아닌 내용(하위 목표로 등록 금지)**:
- 정보 수집, 정찰, 엔드포인트 스캔
- 취약점 분석 및 검증 과정
- 공격 단계, 악용 수단
- 결과 검증 단계

**분해 원칙**:
- 사용자가 설명한 최종 목표가 하나면 하나만 출력
- 서로 독립적인 최종 산출물이 여러 개면 각각 나열
- 명확한 취약점 유형에 대응하면 vulnclass를 표시하고 정보 수집/비즈니스 로직 목표는 비워 둠
- 사용자가 언급하지 않은 목표를 절대 지어내지 않음

set_goals를 호출해 결과를 제출하세요.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**추가 책임: 테스트 자산 범위 등록**
목표 분해 외에 작업 목표/설명에 명시된 테스트 자산 범위를 찾아 add_task_scope로 등록하세요(이 작업의 허가 경계이자 자산 테스트 커버리지의 분모). **최소 범위 원칙: 사용자가 명시한 대상만 등록하고 임의로 확장하지 마세요.**
- 대상이 URL 또는 호스트명이 있는 주소(예: https://xxx.example.com/path, app.example.com)이면 전체 호스트명을 추출해 kind=subdomain, value=전체 호스트명으로 등록하세요.
  예: 대상 https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net(example.net이 아님).
  하위 도메인이 있는 호스트명을 루트 도메인으로 줄이지 마세요. xxx.example.com을 보고 example.com 전체를 등록하면 사용자 대상 밖으로 범위를 확장해 최소 범위 원칙을 위반합니다.
- 사용자가 하위 도메인 없는 루트 도메인 자체(예: example.com)를 제공했거나 "전체 사이트 / 모든 하위 도메인 / 전체 도메인"을 명시한 경우에만 kind=root_domain, value=example.com을 사용하세요.
- IP 또는 네트워크 대역이면 kind=ip / cidr, value=IP 또는 CIDR을 사용하세요.
- 회사 범위(company)는 등록하지 마세요. 작업 생성 직후에는 자산 시스템에 해당 회사가 없어 등록할 수 없는 경우가 많습니다. 회사 수준 범위는 이후 plan 단계에서 처리합니다.
기타 규칙:
- 목표/설명에 명시된 범위만 등록하고 언급되지 않은 도메인/IP를 지어내거나 추론하지 마세요.
- reason에는 어느 문장을 근거로 삼았는지 간단히 적어 감사할 수 있게 하세요.
- 목표/설명에 명확한 자산 범위가 없다면 add_task_scope를 호출하지 마세요.
범위가 있다면 add_task_scope로 먼저 등록한 뒤 set_goals로 목표를 제출하세요.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (배경: 대상 범위/flag 수/교전 지침 등).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 목표 분해는 일회성 호출로 transcript store가 없으므로 agentcore가 ctx에
	// session id를 붙이지 않습니다(writer가 있을 때만 설정, agentcore.Prompt 참고).
	// session-id 헤더로 프롬프트 캐시/고정 라우팅을 하는 게이트웨이는 ctx의 이 값을 읽습니다.
	// opencode zen은 x-opencode-session이 없으면 400 MissingSessionID를 반환하므로 대화는 되지만 분해는 실패할 수 있습니다.
	// 동일 탐색의 분해 요청이 공유하는 안정적인 id를 명시해 캐시 적중률을 높입니다.
	// planner/worker와 이름이 겹치지 않으며 llmrec.parseSession이 정확하게 귀속할 수 있습니다.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints는 asset store와 무관하게 항상 사용 가능합니다. 본문에 이미 제약 추출 후 목표 분해 단계가 있으므로
	// (Agent 편집 페이지에서 문구 수정 가능) 여기서는 도구만 연결합니다.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "작업 목표:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\n작업 설명(대상 범위/flag 수/교전 지침을 포함할 수 있는 배경 정보입니다. 참고용이며 언급되지 않은 내용을 지어내지 마세요):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 제약 추출 → 범위 등록 → 목표 분해의 각 단계에 도구 호출이 필요하므로 set_goals 누락을 막을 충분한 턴을 제공합니다.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 프로필에서 비스트리밍을 선택하면 Provider.Complete를 사용합니다.
		MaxTokens:    maxTokens,    // 0이면 한도를 전송하지 않고 서버 기본값을 따릅니다.
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
