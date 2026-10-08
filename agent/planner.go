package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【계획 할 일(이전 실행에서 작성했으며 깨우기 사이에도 유지됨)】: \n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("이 목록에 따라 진행하세요. 선행 단계가 완료되었거나 필요한 fact가 존재하는 다음 단계에만 의도를 내리세요. TodoWrite로 목록을 갱신하고 fact로 충족된 단계는 completed로 표시하세요. 목록의 pending/in_progress 단계를 중복으로 내리지 마세요.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 요약).
//	"goal"    — the human (via 주 Agent의 set_goals) added one OR MORE goals in a
//	            single call (Goals = 이번에 추가한 목표 텍스트 1개 이상; set_goals는 일괄 처리 지원).
//	"goal_deleted" — the human deleted a goal from 개요의 목표 관리 (Detail = 삭제된 목표 텍스트).
//	"goal_edited"  — the human edited a goal from 개요의 목표 관리 (OldGoal→NewGoal 텍스트).
//	"cancelled" — the human deleted intent IntentID (Detail = 삭제 이유). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 전용: 삭제 전 캡처한 의도 요약(영구 삭제 후에는 노드 조회 불가)
	Goals    []string // Kind=="goal" 전용: 이번 set_goals로 추가한 목표 텍스트(하나 이상)
	OldGoal  string   // Kind=="goal_edited" 전용: 수정 전 목표 텍스트
	NewGoal  string   // Kind=="goal_edited" 전용: 수정 후 목표 텍스트
	Hints    []string // Kind=="hint" 전용: 이번 add_hint로 추가한 힌트 텍스트(하나 이상)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【이번 실행을 유발한 실제 변경 사항(먼저 읽고 방향 추가 여부를 결정)】: ")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 Agent)가 목표를 추가했습니다: %s — 새로 달성해야 할 목표입니다. 대응하는 의도가 없다면 탐색 방향을 추가하세요.", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 Agent)가 %d개 목표를 추가했습니다: %s — 모두 새로 달성해야 할 목표입니다. 대응하는 의도가 없는 목표마다 탐색 방향을 추가하세요.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 Agent)가 전략 힌트를 추가했습니다: %s — 탐색 그래프에 연결되었습니다. 대응하는 의도가 없다면 이에 따라 탐색 방향을 조정/추가하세요.", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 Agent)가 전략 힌트 %d개를 추가했습니다: %s — 모두 탐색 그래프에 연결되었습니다. 각각에 따라 탐색 방향을 조정/추가하세요.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 사용자가 목표를 삭제했습니다: %s — 해당 목표는 제거되었습니다. 나머지 목표/방향을 다시 판단하고 삭제된 목표에 의도를 내리지 마세요.", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 사용자가 목표를 '%s'에서 '%s'(으)로 바꿨습니다. 새 목표에 맞춰 탐색 방향을 조정하고 기존 방향이 더 이상 적절하지 않으면 할당을 중단하세요.", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 Worker가 발견 사항을 보고했습니다: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 삭제 시 캡처한 Summary를 우선 사용합니다(영구 삭제 후에는 intentSummary로 노드 조회 불가).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 사용자가 의도 #%d를 삭제했습니다. 내용: %s, 삭제 이유: %s. 해당 의도는 더 이상 실행되지 않습니다. 이에 따라 다시 계획하세요.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 Worker가 종료되었습니다. 결론: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; 이 의도에서 새로 생성한 사실 ID: %s ", fids))
			}
		}
	}
	b.WriteString("\n(전체 상세는 node_detail / get_worker_output / list_findings로 조회할 수 있습니다.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(출력 조회 실패)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(이 work에는 아직 출력 기록이 없습니다)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(잘림, 전체 내용은 get_worker_output 참조)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【이번 실행의 현황(graph_overview 사전 조회 결과이며 직접 호출한 반환값과 같습니다. 상세가 필요하면 node_detail/list_facts 등을 호출하세요)】: \n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate artifact rules
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `당신은 사이버 보안 플랫폼의 허가된 모의 침투 테스트 시스템에서 계획자 역할을 맡으며 그래프가 바뀔 때마다 자주 깨어납니다. 현황 확인 → 목표 판정 → 아직 다루지 않은 새 방향이 실제로 있을 때만 탐색 의도를 추가합니다. 당신은 실행자가 아닌 계획자입니다. 이번 실행의 산출물은 의도 생성/명확화 또는 목표 판정이어야 하며 plan 안에서 실제 작업을 수행하면 안 됩니다.

작업 목표: {{.Goal}}

**이번 실행에서 생성할 의도 수(먼저 판단)**:
- **필수 조건(최우선)**: 목표가 미달성이고 open 또는 running 의도가 하나도 없으면(frontier_open=0, running_intents가 비어 있음) 이번 실행에서 목표를 향한 의도를 반드시 하나 이상 생성해야 합니다. 실행 중이거나 대기 중인 work가 없는데 의도도 없으면 작업이 멈춥니다. 알려진 방향이 recent_done에만 있어도 아래 done/exhausted/blocked 판단에 따라 새 의도나 이어서 수행할 의도를 내리세요.
- 그 외에는 의도 0개도 정상 결과이지만 정당한 이유가 있어야 합니다. 적게 내리는 것이 안전하다는 기본 판단은 안 됩니다. ① 이미 처리 중: 생각한 방향이 open/running 의도로 다뤄지고 있습니다(표현만 바꾼 중복 의도는 심각한 오류). ② 의존성 대기: 다음 단계가 실행 중인 work의 아직 나오지 않은 산출물에 의존합니다(강제로 내리면 선행 결과가 없어 헛돌므로 다음 그래프 갱신 때까지 기다림).
- 반대로, 기존 의도로 다루지 않고 실행 중인 work에도 의존하지 않는 새 방향이 있거나 목표가 미달성이고 범위 안에 미테스트 영역이 있으면 의도를 내리세요. 의도 0개를 기본 선택으로 삼지 마세요.

**깨어날 때마다 따를 판단 과정**:

1. 전체 현황이 아래에 첨부되어 있습니다(graph_overview 반환값이므로 재호출 불필요): task(원래 제목+목표/루트 노드), 자산 수, goals+상태, open/running/recent_done 의도, sites_without_endpoints(엔드포인트 없는 사이트로 탐색 후보), facts(탐색 사실 수, 취약점과 별개), recent_facts({id,summary,confidence?}).
   - 범위: 탐색 노드(goals/의도/facts/findings)는 이 작업만 포함합니다. 자산 그래프는 모든 작업이 공유하며 자산 수는 전역에서 범위 내 자산의 수입니다. 이 작업만의 자산이 아니므로 무관한 자산은 무시하세요.
   - 계보: 각 의도의 parents는 어떤 사실/의도에서 파생했는지, yields는 어떤 사실/발견 사항을 생성했는지 나타냅니다. recent_facts에는 from_intent가 있습니다. 이를 통해 사실의 출처 방향과 새 방향을 종합할 수 있는지 이해하세요.
   - 부정적/불확실한 관찰(recent_facts의 포트 닫힘/인젝션 불가 등)은 Worker의 관찰이며 확정 결론이 아닙니다. 채택 전 node_detail(id)로 evidence를 확인하세요. 근거가 탄탄하고 confidence=observed이며 수단을 모두 검토한 경우에만 잠정적으로 막힌 방향으로 보세요. 근거가 없거나 외관/단 한 번의 시도에 의존하거나 confidence=inferred면 미확인으로 취급하세요. 범위 내이고 다른 의도가 다루지 않으면 재검증 의도를 내려 확인하거나 반박하세요. 같은 부정 방향은 최대 한 번만 재검증하고 결과가 여전히 부정적이며 근거가 타당하면 결론을 존중해 더 내리지 마세요.
   - 더 자세한 정보가 필요할 때만 호출하세요: list_facts(최신순 페이지 조회, 기본 20개, q 필터, before 페이지 이동, total/has_more 포함), list_findings(전체 취약점), node_detail(id)(전체 증거/상세, 목록과 recent_facts는 요약만 제공), list_assets(q 검색, type/company_id/task_id 필터, 페이지 조회 또는 id/ids 직접 조회), asset_neighbors. 자산은 전역 공유이므로 기본적으로 전체를 가져오지 마세요.

2. 목표 판정(핵심 책임): goals에 목표와 상태가 있습니다. 발견 사항/사실로 증명된 미달성 목표는 prove_goal(goal_id, evidence_id, reason)로 met 표시하세요. 마지막 미완료 목표를 표시하면 시스템이 작업 전체를 자동 완료합니다. 마무리는 각 prove_goal로만 이루어지며 별도의 일괄 완료 수단은 없습니다.
   - 정량적 인수 기준 확인(조기 완료 금지): 커버리지 X%, flag N개, 특정 권한 획득 등 정량 조건이 있으면 prove_goal 전에 위 graph_overview의 실측값(coverage.pct, findings_total 등)을 반드시 확인하세요. 기준 미달이면 prove_goal을 호출하지 말고 부족한 부분을 위한 의도를 추가하세요. 대체로 달성했다거나 핵심을 확보했다는 이유로 미리 met 표시하지 마세요. 예: 커버리지 100% 요구, 실측 40%면 미달성이므로 추가 테스트 의도를 내립니다.

3. 선택적 초기 경량 탐색: 작업 초기에 fact가 거의 없고(recent_facts가 거의 비어 있음) 현황만으로 초기 의도를 구체화할 수 없을 때만 Bash 등으로 매우 적은 읽기 전용 탐색(예: curl 1–2회로 홈페이지/지문 확인)을 하세요. 허용되는 산출물은 더 정확한 의도 설명 한 문장뿐입니다. 취약점 발견/검증/악용이나 엔드포인트/디렉터리/매개변수 열거는 Worker의 일이므로 의도로 내리세요. 세 가지 경계:
   - Worker가 만든 fact가 이미 있으면(facts>0 또는 recent_facts가 비어 있지 않음) 직접 탐색하지 마세요. 기존 fact로 판단하고 새 의도를 내리거나 종료하세요. 단서를 깊이 조사하려면 직접 curl하지 말고 Worker에 의도를 내리세요.
   - 시작 시에도 탐색은 최대 3회이며 초기 의도를 명확히 하는 용도로만 사용하세요. 빠른 방향 결정이 아니라 심층 확인(엔드포인트/디렉터리/ID 개별 열거, 디코딩 체인, 동일 API 반복 탐색, 인젝션/권한 우회/취약점 테스트 검증)을 하고 있다면 즉시 멈추고 의도로 작성하세요.
   - 기존 사실/현황으로 판단할 수 있으면 탐색할 필요가 없습니다.

4. 추가할 새 방향 결정: 절제란 기존 의도를 중복하지 않는 것이며 가능한 적게 내리라는 뜻이 아닙니다. 목표가 미달성이면 마무리 여부보다 목표에 더 가까워질 깊이 있고 아직 다루지 않은 방법을 찾으세요. 의도는 정해진 유형/메뉴가 아닌 열린 탐색 방향입니다. 사실, 자산, 목표를 종합해 방향을 판단하고 open + running + recent_done과 각각 비교하세요.
   - open/running이 이미 다루는 방향은 생성하지 마세요.
   - recent_done에 있었다면 먼저 state로 중단 이유를 구분한 뒤 결정하세요.
     · done(정상 완료): 이미 다뤘으므로 그대로 다시 내리지 마세요. 막힌 방향인지는 state가 아니라 yields의 fact 결론으로 판단하세요. 실질적으로 새로운 원리(새 사실/자산/매개변수/명확히 다른 방법)가 있을 때만 다시 내리고 summary에 이전과의 차이를 명시하세요. 표현 변경이나 막연한 재시도는 해당하지 않습니다.
     · exhausted(예산 소진으로 중간 종료, 일부만 기록) / blocked(모델/네트워크 실패로 거의 탐색하지 못함): 정보가 불완전하므로 get_worker_trace / get_worker_output으로 실제 진행과 막힌 지점을 확인하세요. 돌파 직전 예산이 소진됐다면 이전 진행에서 이어가고, 순수 외부 장애로 실행하지 못했다면 같은 방향을 다시 내리며, 매번 같은 곳에서 막히면 방법/방향을 바꾸세요. 근거는 항상 state 자체가 아니라 trace의 실제 진행입니다.
   - 어떤 의도도 다루지 않은 완전히 새로운 방향이면 생성하세요.
   - 알려진 모든 방향을 open/running 의도가 다룬다면 생성하지 않고 종료해 진행을 기다리세요. recent_done만 남고 open/running이 없으며 목표가 미달성이면 위 필수 조건에 따라 새 방향이나 이어서 수행할 의도를 내리세요.
   - 깊이를 커버리지보다 우선하세요. coverage는 최소 기준/인수 항목이지 탐색 목표 자체가 아닙니다. RCE/권한 상승/데이터 유출로 이어질 수 있는 고가치 진입점을 찾으면 커버리지를 맞추려고 자산마다 얕게 훑기보다 그 경로를 깊이 검증하는 의도를 우선하세요.
   - 경로의 다양성을 유지하고 너무 일찍 하나로 좁히지 마세요. 목표 미달성 상태에서 의도가 한 경로에 몰려 있고 본질적으로 다른 미검토 방향(다른 진입점/자산 유형/악용 체인)이 있다면 같은 경로의 동의어 의도보다 그 방향을 추가하세요. 이미 다뤄지는 방향이면 생성하지 마세요. 원리가 다른 경로 2–3개(예: 업로드 체인, 인증 우회)를 유지하다가 목표에 가까워졌다는 증거가 나온 경로에 집중하는 것이 이상적입니다. 단, 다양성은 항상 최상위 작업 제약을 따라야 합니다. 제약으로 제외된 진입점/포트/호스트/작업은 본질적으로 달라도 의도를 생성하지 마세요.

   직렬 악용 체인은 단계별로 내리고 병렬로 쪼개지 마세요. ①→②→③처럼 실제 선행 산출물에 의존하면 한꺼번에 병렬 할당하지 마세요. TodoWrite에 전체 체인을 단계별 할 일로 기록하고 이번에는 선행 조건이 충족된 단계(보통 첫 단계)만 내리세요. fact가 나온 뒤 다음 깨우기 때 다음 단계를 내리고 충족된 항목을 completed로 표시하세요. 같은 일을 둘로 나누지 마세요(트리거 지점 확인과 해당 지점 트리거는 같은 단계). 서로 의존하지 않는 병렬 차원(예: 무관한 엔드포인트들 열거)에만 여러 의도를 병렬로 사용하세요.

5. 제출: 선별한 새 방향은 add_intent 한 번으로 일괄 제출하세요(intents 배열, 가치가 가장 높은 최대 4개, 여러 번 나누어 호출하지 않음).
   - summary: 대상의 전체 주소 + 할 일 + 이유를 자연어 한 문장으로 설명하세요. 고정 분류에 맞추지 마세요. 기존 의도와의 중복 판단은 주로 이 설명을 사용합니다.
   - asset_ids: 이 방향에서 테스트/공격할 대상 자산 ID(list_assets에서 얻은 0개 이상)를 가능한 한 전달하세요. 구체적인 사이트/API/매개변수/호스트를 다룬다면 반드시 전달해 커버리지 중복을 막고 자산 관계를 연결하세요. 여러 자산이면 모두 전달하고 구체적인 자산 없는 전역 정찰만 비워 두세요.
   - parent_ids: 이 방향의 근거가 된 상위 노드(선택, 0개 이상). 여러 사실을 결합했다면 모두 전달하고 상위 의도/발견 사항에서 파생했다면 그 ID도 전달하세요. 최상위 새 방향이면 비워 두세요.

중복하거나 억지로 채우지 마세요. 다만 목표가 미달성이고 아직 다루지 않은 더 깊은 방법이 있으면 의도를 내리세요. 간결하고 집중적으로 효율 있게 진행하세요.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 도메인 도구 + 기본 도구 모음(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets를 프롬프트에서 제외합니다.
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 핵심 현황(방금 완료한 의도 + 사전 조회한 전체 그래프)은 이번 user 입력(input)에 넣고
	// system에는 정적인 계획 본문만 둡니다. system이 매번 안정적으로 유지되어 캐시에 유리하지만 긴 턴에서는
	// 현황이 compaction으로 압축될 수 있습니다(planner 턴은 보통 짧아 위험이 낮음). situational은 아래 input에 합칩니다.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 작업 수준 deadline / 종료 모드(ctx로 전달, taskclock.go 참고). 마지막 실행에서는 작업 시간 초과
	// planner 마무리 프롬프트를 이번 작업 지시로 user 입력에 situational과 함께 넣어
	// 마지막 목표 판정만 수행하고 새 의도를 만들지 않게 합니다.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【작업 최종 마무리(이번 실행의 특별 지시이며 위의 일반 계획 과정보다 우선)】: " + resolveTaskTimeoutWrapup("planner")
	}
	// 이 작업의 디렉터리 <workDir>/tasks/<taskID>를 먼저 만듭니다.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 작업 제약이 있으면 시스템 프롬프트에 넣어 탐색 경계를 정합니다.
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 자체의 실행 시간 한도는 없습니다. deadline이 있으면 MaxDuration을 남은 시간으로 제한해
	// 작업 시한에 마무리합니다(시간 초과는 작업 시간 초과 문구, 단계 초과는 실행별 문구 사용).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 사용하며 프록시 CA로 MITM 재서명 HTTPS 인증서를 검증합니다.
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 웹 검색(선택 사항). ddgs는 키가 필요 없고 brave-free는 BraveKey, tavily는 TavilyKey가 필요합니다.
		// WebSearchProxy는 별도 송신 프록시(http/https/socks5)로 트래픽 기록용 MITM 프록시와 무관합니다. 비어 있으면 직접 연결합니다.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 하위 명령은 기본적으로 프록시를 사용하고 CA를 신뢰합니다.
		WorkingDir:            taskDir,                              // 이 작업의 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=무제한, deadline이 있으면 deadline까지 남은 시간
		Compaction:            compactionConfig(p.compactionWindow()),
		// 깨우기 사이에 계획 할 일을 공유해 직렬 체인을 유지합니다(session은 새로 만들지만 store는 유지).
		Todos: p.todoFor(ts.ID()),
		// 이번 실행의 단계 한도에 도달하면 SDK가 마무리합니다. 결정된 내용을 add_intent,
		// prove_goal, TodoWrite로 반영하며 계획 자체를 멈추지는 않습니다. planner는 이후에도 반복해서 깨어납니다.
		// 작업 deadline으로 제한된 clamped 상태에서는 PromptByReason을 사용합니다(wrapupSettlementForTask 참고).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 프로필에서 비스트리밍을 선택하면 Provider.Complete를 사용합니다.
		MaxTokens:    p.maxTokens(),    // 0이면 한도를 전송하지 않고 서버 기본값을 따릅니다.
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 실험 기능: 활성화하면 noa가 컨텍스트 압축을 담당하며 <workDir>/noa/<SessionID>에 아카이브를 영구 저장합니다.
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 현황(방금 완료한 의도 + 전체 그래프)을 이번 user 입력(input)에 합칩니다.
	// user 입력에는 지시와 깨우기 간 할 일도 포함합니다(todo는 재생성 가능한 모델의 계획 메모이므로 user에 두어도 됨).
	// 도입문은 실제 변경 여부로 나눕니다. 변경이 있으면 아래 실제 변경 블록을 가리키고, 없으면
	// (주기 점검 / hint / 재개 등) 그래프가 바뀌었다고 하지 않고 실행 중인 의도도 점검하도록 합니다.
	lead := "실제 변경이 발생했습니다(아래의 이번 실행을 유발한 실제 변경 사항 참조). 이에 따라 다음 단계를 계획하세요: "
	if len(triggers) == 0 {
		lead = "이번 깨우기는 주기 점검(하트비트) 또는 구체적 변경 신호 없이 발생했습니다. 그래프가 바뀌었다고 단정하지 마세요. 실행 중인 의도도 확인해 오랫동안 진전이 없거나 방향이 어긋나면 steer_work로 조정하고, 방향 전체가 잘못되었다면 kill_work로 중단하세요. 이후 목표를 판정하고 방향 추가 여부를 결정하세요: "
		// 하트비트/변경 없는 깨우기에서 open 또는 running 의도가 하나도 없으면 탐색이 멈춘 상태입니다.
		// planner에 명확히 알려 이번 실행에 새 방향을 추가하도록 하며 실행 의도 확인만 하고 헛돌지 않게 합니다.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "이번 깨우기는 주기 점검(하트비트)이며 현재 open 또는 running 의도가 하나도 없습니다. 실행 중인 Worker도 대기 중인 방향도 없어 탐색이 멈췄습니다. 이번 실행에서 목표를 향해 나아가며 그래프의 기존 의도와 중복되지 않는 새 의도를 하나 이상 반드시 생성하세요(의도 0개 금지). 먼저 아래 현황으로 목표 달성 여부를 판단하고 미달성이면 즉시 방향을 추가하세요: "
		}
	}
	input := lead + situational + "\n\n위 현황으로 목표를 판정하세요. 목표가 실제로 달성되었다면(목표 성과 확보/목표 취약점 확인) prove_goal로 하나씩 표시하세요. 필수 조건: 목표가 미달성이고 현재 open 또는 running 의도가 하나도 없으면(frontier_open=0, running_intents가 비어 있음) 이번 실행에서 목표를 향한 의도를 반드시 하나 이상 생성하세요. 기다릴 work도 대기 방향도 없는 상태에서 의도 0개는 작업 정지입니다. open/running 의도가 진행 중이거나 목표를 달성한 경우에만 새 의도를 생성하지 않아도 됩니다." +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration은 실행 시간 도달 시 도구를 중단하고 살아 있는 ctx에서 즉시 마무리합니다. 단일 턴 정체로
	// 마무리를 건너뛰지 않으므로 외부 강제 ctx 보완이 필요 없습니다. ctx는 pause / kill / shutdown만 담당합니다.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
