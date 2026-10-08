package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate
// artifact output rules tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `당신은 허가된 모의 침투 테스트 시스템의 주 Agent이며 사람 운영자와의 인터페이스입니다. 직접 탐색하거나 의도를 자율적으로 연속 생성하지 않습니다(계획자의 역할). 당신의 책임:

1. 관찰: graph_overview / list_findings / list_facts / list_assets / get_worker_output으로 현재 진행 상황에 대한 질문에 답합니다.
2. 조정(사람의 의도를 시스템에 반영):
   - "방향 변경/특정 취약점 강조/특정 영역 집중"을 원하면 add_hint로 힌트를 남깁니다(계획자가 다음에 읽음).
   - "특정 대상을 즉시 테스트"하려면 add_intent로 우선순위가 높은 의도(priority 8-10)를 직접 추가합니다. 시스템이 완료된 작업을 자동으로 실행 상태로 되돌리고 Worker가 해당 의도를 수행하면 다시 완료 상태가 됩니다.
     **작업 목표가 모두 달성된 경우**(graph_overview의 goals가 모두 met): 의도를 내리기 전에 새롭게 달성할 결과가 암묵적으로 포함되어 있는지 판단하세요. 포함되어 있으면 추측한 목표를 한 문장으로 설명하고 정식 목표로 등록할지 물으세요. 원하면 set_goals로 등록합니다(일반 계획 단계로 진입해 계획자가 자율적으로 진행). 원하지 않거나 일시적인 확인만 원하면 add_intent로 해당 의도만 내립니다(Worker 실행 후 완료 상태로 돌아가며 자율적으로 계속하지 않음). 명백한 일회성 확인이고 새 목표가 없다면 매번 묻지 말고 바로 add_intent를 사용하세요.
   - "실행 중인 의도(work)의 방향을 실시간 수정(X 중단, Y 집중)"하려면 steer_work를 사용하세요(중단이나 진행 손실 없이 Worker의 다음 동작 전에 적용). 먼저 get_worker_output으로 현재 수행 내용을 확인하세요. 방향 전체가 잘못되었다면 add_intent로 새 의도를 내리세요.
   - "달성할 최종 목표 추가"를 원하면 set_goals로 보충하세요. 시스템이 목표를 작업 그래프에 기록하고 완료/일시 중지 작업을 자동으로 실행 상태로 되돌립니다(계획자가 이후 달성 여부를 다시 판단). 사람이 재개를 누를 필요가 없습니다.
   - "테스트 제약 추가/변경(현재 포트만 테스트, 무차별 대입 금지, 수동적 정찰만 허용 등)"은 set_constraints로 등록하세요(type=allow 허용 / type=deny 금지). 다음 계획 주기에 planner/worker 프롬프트에 주입되어 탐색 경계를 정하며 개요의 제약 관리에서도 추가/삭제/수정할 수 있습니다.
3. 자연스러운 말로 간결하게 응답하고 수행한 내용을 설명하세요.

현재 작업 목표: {{.Goal}}

발견 사항을 지어내지 말고 도구가 반환한 실제 데이터만으로 답하세요.`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 공통 깨우기: 전용 콜백이 없는 쓰기 작업에 사용하며 디바운스합니다.
	tsx.SetResumeTask(resume)     // set_goals 목표 추가 → 완료/일시 중지 작업을 running으로 복귀
	tsx.SetNotifyGoal(notifyGoal) // set_goals 목표 추가 → planner에 사용자 목표 추가 트리거 기록
	tsx.SetNotifyHint(notifyHint) // add_hint 힌트 추가 → planner에 사용자 전략 힌트 추가 트리거 기록
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// 도메인 도구 + 기본 도구 모음(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets를 프롬프트에서 제외합니다.
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// 이 작업의 디렉터리 <workDir>/tasks/<taskID>를 먼저 만듭니다.
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 사용하며 프록시 CA로 MITM 재서명 HTTPS 인증서를 검증합니다.
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// 웹 검색(선택 사항). ddgs는 키가 필요 없고 brave-free는 BraveKey, tavily는 TavilyKey가 필요합니다.
		// WebSearchProxy는 별도 송신 프록시(http/https/socks5)로 트래픽 기록용 MITM 프록시와 무관합니다. 비어 있으면 직접 연결합니다.
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash 하위 명령은 기본적으로 프록시를 사용하고 CA를 신뢰합니다.
		WorkingDir:            mainDir,                              // 이 작업의 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // 대화별 임시 할 일(TodoWrite), 계획용이며 종료 시 폐기
		// 단계 한도 도달 시 SDK가 마무리하며 사용자에게 진행 요약을 출력합니다. 프롬프트와 마무리 턴 수는 관리 화면에서 편집 가능(기본 10턴).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // 프로필에서 비스트리밍을 선택하면 Provider.Complete를 사용합니다.
		MaxTokens:    m.maxTokens(),    // 0이면 한도를 전송하지 않고 서버 기본값을 따릅니다.
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 실험 기능: 활성화하면 noa가 컨텍스트 압축을 담당하며 <workDir>/noa/<SessionID>에 아카이브를 영구 저장합니다.
	// session id는 transcript와 동일하게 구간을 구분해 아카이브와 복원을 맞춥니다.
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
