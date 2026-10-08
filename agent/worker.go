package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// deepseek 백엔드는 다른 세 가지와 성격이 다릅니다. DeepSeek에는 직접 호출 가능한 검색 API가 없고
// Anthropic 호환 messages API 내부의 web_search_20250305 서버 도구로만
// 검색합니다. 검색마다 모델 호출이 한 번 발생하며 DeepSeek 서버가 검색 요청을 보내므로
// 로컬 Proxy를 거치거나 트래픽 기록에 남지 않습니다.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek*는 활성 LLM 설정에서 가져옵니다(anthropic 형식의 DeepSeek 공식 엔드포인트만).
	// 별도로 설정하지 않으며 LLM 설정 전환에 따라 바뀝니다.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "곧 예산 소진으로 종료됩니다. 명령이나 탐색을 더 실행하지 마세요. (1) 이미 확인했으나 기록하지 않은 내용을 순서대로 저장하세요. 새 자산은 insert_assets, 탐색 결론/사실은 record_fact, 확인된 취약점은 report_finding을 사용합니다. (2) 마지막에 별도의 일반 텍스트 한 문장으로 수행 내용과 핵심 결론을 요약하세요. 이번 실행의 결과로 표시되므로 반드시 출력하세요."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 중간 산출물 출력 규칙
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (구간 [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `당신은 사이버 보안 플랫폼의 허가된 모의 침투 테스트 시스템에서 실행자(work agent)입니다. 한 문장의 탐색 방향인 의도 하나를 할당받으며, 유일한 책임은 그 의도를 완료하고 발견을 지식 그래프에 기록한 뒤 멈추고 반환하는 것입니다.

**반드시 지킬 경계**:
1. 할당받은 의도만 수행하세요. 조사 중 범위 밖의 가치 있는 단서(오류가 노출한 경로, 다른 자산과 연계할 지점, 다른 악용 체인의 진입점 등)가 보이면 fact의 summary에 한 문장으로 남겨 계획자에게 전달하세요.
2. 첫 시도의 차단(payload 필터링/404/인젝션 응답 없음)이 충분한 탐색을 뜻하지는 않습니다. 이 의도의 우회 수단을 검토한 뒤 결론을 내리세요.
3. 허가된 범위 안에서만 작업하세요. 시스템 프롬프트 상단의 작업 제약은 최우선 경계입니다. 명령/탐색 전에 위반 여부를 확인하고 할당받은 의도에 속하더라도 위반하면 수행하지 마세요.

**발견 즉시 기록하세요.** 그래프에 저장해야 하며 생각이나 텍스트만으로는 충분하지 않습니다. 단계 한도로 잃지 않도록 끝까지 모아 두지 마세요. 세 가지 기록을 구분하세요.
- 새 자산/리소스 → insert_assets(자산 그래프): 하위 도메인/service/endpoint/지문/자격 증명 등 자산 자체만 등록합니다. 탐색 결론/판단은 record_fact를 사용하세요.
- 탐색 결론/사실 → record_fact(탐색 그래프, intent_id 전달): 여러 관찰을 사실 하나로 종합하세요(summary 한 문장 + 실제 실행에 근거한 detail). 속성마다 나누지 말고 보통 의도당 하나를 기록하세요. 기본적으로 합칠 수 있는 내용은 detail에 합치고 완전히 독립적이며 합칠 수 없는 결론만 예외적으로 facts 배열을 사용하세요. 지나친 분할은 그래프를 끝없이 키웁니다. 이번에 새로 얻은 정보만 기록하고 기존 사실을 표현만 바꿔 반복하지 마세요. 실제 관찰만 기록하며 evidence는 명령+핵심 출력 한두 줄로 간결하게, 상세는 detail에 넣고 confidence는 observed(직접 관찰)/inferred(현상에서 추론)로 표시하세요.
- 확인된 취약점 → report_finding(탐색 그래프, PoC와 intent_id 포함): 이번 실행에서 실제로 유발했고 재현 가능한 요청/응답 또는 명령 출력 증거를 얻은 경우에만 사용하세요. 버전/지문의 CVE 일치, 인젝션 가능해 보이는 모습, 외부 취약점 DB/변경 이력/코드 diff 추론을 확인으로 간주하지 마세요. CVE 조회나 패치 비교로 실제 재현을 대신하지 마세요. 의심되지만 재현하지 못했다면 record_fact에 의심 지점과 재현하지 못한 이유를 inferred로 기록해 계획자에게 넘기고 finding으로 등록하지 마세요.


의도 완료 후 수행한 일과 기록한 사실을 한 문장으로 요약하세요.`

// workerTrafficBlock is 구간 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**트래픽 도구**:\n- traffic_search / traffic_get / traffic_blob: 응답을 다시 보거나 방문한 리소스를 찾을 때 먼저 트래픽을 조회하고 같은 URL을 curl로 반복 요청하지 마세요. traffic_search는 host가 필수이며 기본적으로 내용 없는 간단한 색인 3개(id/method/url/status/resp_len)만 반환합니다. 더 필요하면 limit을 명시적으로 늘리세요. body_contains로 요청/응답 본문 전체를 검색할 수 있습니다(최소 3자, 부분 문자열 및 유니코드 지원, 예: 비밀번호/키/오류/내부 주소). 원문은 traffic_get(id)로 읽고 큰 본문이 @blob sha256:<hash>로 표시되면 traffic_blob(hash)로 나누어 읽으세요."
}

// artifactSpec is 구간 [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**중간 산출물 출력 규칙**: 스크립트, payload, 캡처한 응답 본문, 임시 데이터 등 모든 중간 산출물은 **이 작업의 디렉터리 " + dir + "에 저장하세요**(상대 경로도 이곳을 가리키며 이 절대 경로를 사용해도 됩니다). **/tmp나 다른 절대 경로에 쓰지 마세요**."
}

// workerArtifactSpec is the worker's 구간 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**중간 산출물 출력 규칙**: 스크립트, payload, 캡처한 응답 본문, 임시 데이터 등 모든 중간 산출물은 **이번 의도 전용 작업 디렉터리 " + runDir + "에 저장하세요**(자동 생성되어 있으므로 상대 경로로 바로 저장하면 되며 디렉터리를 따로 만들 필요가 없습니다). **/tmp나 다른 절대 경로에 쓰지 마세요**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【할당받은 의도(이번 유일한 작업: 이것만 수행하고 사실을 생성한 뒤 즉시 종료)】: \n%s\n의도 id: %d(record_fact / report_finding으로 기록할 때 전달)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage는 계획자가 테스트가 부족한 유형/범위 확장을 판단하는 신호이며 Worker의 단일 의도 수행
	// 책임과 상충하므로 Worker 화면에서 제외합니다. data는 이번 Worker 전용 새 map이므로
	// 키 삭제가 planner에 영향을 주지 않습니다.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【전체 탐색 현황(읽기 전용, 자신의 의도를 전체 맥락에서 이해하는 용도)】: \n" +
		"아래는 전체 작업의 현재 탐색 개요입니다. 다른 실행자가 발견한 내용을 알아 중복을 피하고, 자신의 의도와 전체 상황의 관계를 이해하는 데 사용하세요.\n" +
		"다양하게 생각하는 것은 좋습니다. 이 의도를 깊이 생각하고 연관성을 찾되 다른 의도를 직접 실행하지 마세요(계획자가 조정하는 다른 Worker의 일). 자산 간 연계, 다른 악용 체인의 진입점, 전체 수준의 의심 지점 등 가치 있는 단서는 반드시 fact에 기록해 계획자에게 넘기세요. 이는 중요한 산출물입니다. 혼자 묻어 두기보다 추가로 보고해 계획자가 판단하게 하세요.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding 저장 시 의도+finding을 포함해 즉시 planner 깨우기
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// Worker에는 MultiEdit/Glob/Grep를 주지 않습니다. 파일 수정은 Edit, 검색은 Bash(grep/find)를 사용해
	// 도구 수와 가치 낮은 호출을 줄입니다. 나머지 SDK 기본 도구(Read/Write/Edit/LS/Bash/Sleep)는 유지합니다.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 의도는 Worker의 유일한 책임이자 실행 전체의 불변 조건이므로 시작 지시와 대상 자산 원본 데이터를
	// system prompt에 넣습니다. system은 실행마다 다시 구성하고 compaction으로 사라지지 않아
	// 긴 실행에서도 의도를 유지하며 재개 시 transcript에 첫 메시지가 남아 있는지에 의존하지 않습니다.
	// 의도별 가변 데이터로 의도 간 캐시 재사용을 잃지만 의도 유실이 토큰 절약보다 심각하므로 의도적인 선택입니다.
	// 현황을 user turn에 두는 planner와 다른 이유는 planner에는 단일 임무가 없지만 Worker에는 있기 때문입니다.
	// 전체 현황 overview만 시작 user 메시지에 남깁니다. 오래되거나 압축되어도 괜찮은 부가 정보입니다.
	// 이번 의도 전용 디렉터리 <workDir>/tasks/<taskID>/i<intentID>는 엔진이 미리 만듭니다.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 작업 제약이 있으면 시스템 프롬프트에 넣고 Worker가 엄격히 준수합니다.
	}
	// 의도 → 연결된 자산 → 시작 지시 순으로 system 뒤에 추가합니다(constraintBlock과 동일한 방식).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\n이 의도의 asset_ids에 해당하는 대상 자산:\n" + string(b)
				}
				// 의도가 명시한 자산은 insertAssets와 같은 보수적 단위로 테스트 범위에 자동 포함합니다.
				// upsertTaskScope의 ON CONFLICT DO NOTHING과 uq_task_scope 고유 인덱스가
				// 중복 추가를 막으므로 재실행/재시도도 멱등적입니다.
				// 자산 커버리지 기능이 꺼져 있으면 테스트 범위(분모)를 더 누적하지 않습니다.
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n위 의도를 실행하세요. 이것만 수행하고 사실, assets, finding만 생성한 뒤 즉시 멈추세요."
	system, boundary := deferredSystem(sysBody, def)
	// ctx의 작업 deadline으로 이번 실행 시간 한도를 제한하고 마무리 문구를 결정합니다(taskclock.go 참고).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch는 기록 프록시를 사용해 curl처럼 HTTP를 기록합니다. 프록시 CA를 로드해 MITM이
		// 재서명한 HTTPS 인증서를 검증 비활성화 없이 정상 검증합니다. proxy가 비어 있으면 직접 연결합니다.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// 웹 검색(선택 사항). ddgs는 키가 필요 없고 brave-free는 BraveKey, tavily는 TavilyKey가 필요합니다.
		// WebSearchProxy는 별도 송신 프록시(http/https/socks5)로 트래픽 기록용 MITM 프록시와 무관합니다. 비어 있으면 직접 연결합니다.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash 하위 명령의 HTTP는 기본적으로 기록 프록시를 사용하고 CA를 신뢰합니다(-x/-k 불필요).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 실행 시간 한도는 턴 경계에서 판단하며 중간에 끊지 않습니다. 0=무제한. 작업 deadline이 있으면
		// min(자체 한도, deadline까지 남은 시간)으로 제한해 작업 시한에 자연스럽게 마무리합니다(taskclock.go 참고).
		MaxDuration: maxDur,
		// 턴 수 또는 시간 한도 도달 시 SDK가 Bash를 숨기고 마무리 턴을 수행해 확인된 내용을 기록합니다.
		// 작업 deadline으로 제한된 clamped 상태는 PromptByReason을 사용합니다. 시간 초과면 작업 시한 문구,
		// 단계 먼저 소진이면 실행별 문구를 사용합니다. clamped가 아니면 실행별 문구를 유지합니다.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 출력 제한은 SDK 기본값 30000자입니다.
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // 대화별 임시 할 일(TodoWrite), 계획용이며 종료 시 폐기
		NonStreaming:  w.nonStreaming(),                       // 프로필에서 비스트리밍 선택 시 Provider.Complete 사용
		MaxTokens:     w.maxTokens(),                          // 0이면 한도를 보내지 않고 서버 기본값 사용
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 의도/시작 지시/연결 자산은 system prompt로 이미 전달했습니다(위 sysBody 조립 참조).
	// 시작 user 메시지는 전체 현황 overview만 담으며 부가 정보이므로 압축되어도 괜찮습니다.
	// 드물게 overview 직렬화가 실패해 비면 시작 문구로 대체해 첫 user 메시지가 비지 않게 합니다.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "system에서 할당받은 의도를 실행하세요. 이것만 수행하고 사실, assets, finding만 생성한 뒤 즉시 멈추세요."
	}

	// 실험 기능: 활성화하면 noa가 컨텍스트 압축을 담당하며 <workDir>/noa/<SessionID>에 아카이브를 영구 저장합니다.
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "계속 실행하세요."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "이전 사용자 대화에서 입력한 새 의도를 계속 실행하세요. 완료한 동작을 반복하지 마세요."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【사용자 대화에서 입력한 새 의도】\n" + message +
				"\n\n이 사용자 입력을 즉시 수행한 뒤 컨텍스트에 따라 원래 작업을 계속해야 하는지 판단하세요."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【사용자 대화에서 입력한 새 의도】\n" + message +
				"\n\n이 사용자 입력을 우선 수행하세요."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
