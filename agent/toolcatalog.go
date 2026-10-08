package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 내장 도구를 순수 코드에서 열거 및 DB 재정의 가능한 목록으로 제공합니다.
//   - BuiltinToolSeeds(): 세 실행 Agent의 내장 도구 모음을 시드 레코드(key +
//     설명 + 매개변수 schema + 기본 연결 Agent)로 펼쳐 서버 시작 시 tools 테이블에 멱등적으로 초기화합니다.
//   - ToolResolve 훅: 실행 중 DB의 tools 행으로 조립된 도구를 Agent별 필터링하고
//     설명/schema를 덮어쓰며 기본 매개변수를 주입합니다. key/handler는 코드에 유지하고 DB는 설명과 기본값만 수정합니다.
// handler(Call 동작)는 항상 코드에서 가져오므로 DB가 바꿀 수 없습니다. 모델이 보는 설명과 기본 입력만 바뀝니다.

// ToolSeed는 내장 도구의 초기화 가능한 스냅샷입니다. key는 CoreTool.Name()으로 handler에 고정되며
// UI에서 읽기 전용입니다. Desc/Schema는 코드 정의, Agents는 기본 연결 Agent 목록입니다.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), 기본 키, 변경 불가
	Desc   string         // 최상위 설명(UI에서 재정의 가능)
	Schema map[string]any // 매개변수 JSON-Schema(구조는 읽기 전용, description/default는 UI에서 수정 가능)
	Agents []string       // 기본 연결 Agent 키(worker/planner/mainagent)
}

// builtinToolsByAgent는 읽기 전용 껍데기 ToolSet(nil stores)으로 각 실행 Agent의
// 도메인 도구 모음을 만듭니다. 생성자는 Spec에 클로저만 넣고 store를 역참조하지 않아 nil이 안전합니다.
// 여기서는 Name()/Description()/InputSchema()만 읽고 Call은 절대 실행하지 않습니다.
//
// SDK 공통 도구 actool.DefaultTools()(Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash)는 의도적으로 제외합니다. 모든 Agent가 항상 가지므로 연결 선택이 없고 설명 대부분이 Prompt()에
// 있어 Description()만 재정의하면 부분 적용으로 오해를 낳습니다. 시드/DB 행이 없으면 ToolResolve가
// 그대로 통과시켜 기존 동작을 유지합니다. ARTEX 자체 도메인 도구만 테이블에 등록해 관리합니다.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals(목표 분해기)는 기본적으로 set_goals + set_constraints를 연결해 목표와 작업 제약을 저장합니다.
		// mainagent와 같은 관리 도구를 공유하며 웹에서 설명/schema 수정 및 Agent별 선택이 가능합니다.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto는 기본적으로 취약점 보고 + 자산 관리 도구를 연결하고 다른 도메인 도구는 UI에서 선택할 수 있습니다.
		// 새 DB는 이 시드로 초기화하고 기존 DB는 seedAutoDefaultBindings로 마이그레이션합니다.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest(독립 침투 Agent)는 기본적으로 자산 조회/삽입, 취약점 보고/조회, 기업 조회를 연결합니다.
		// 새 DB는 이 시드로 초기화하고 기존 DB는 seedPentestDefaultBindings로 마이그레이션합니다.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound의 system 도구도 목록에는 등록되어 웹에서 보이고 Agent별 수동 연결이 가능합니다.
// 단, 기본 연결 Agent는 없으며 ToolResolve는 연결이 빈 도구를 모든 Agent에서 제외하므로 명시적 선택이 필요합니다.
// 일부 Agent의 기본 도구 모음에 남겨두는 이유(예: PlannerTools의 goal_met)는 시드가 도구를 만들어
// desc/schema를 얻고, 사용자가 수동 연결했을 때 런타임 기본 모음에 있어 ToolResolve가 유지할 수 있게 하기 위함입니다.
//
// goal_met는 개별 prove_goal을 건너뛰고 전체 작업 완료를 선언하므로 영향과 오판 위험이 크며
// 마지막 목표 prove_goal 시 자동 종료하는 기능과 겹칩니다. 따라서 기본 연결하지 않고 필요할 때 수동 연결합니다.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds는 각 Agent의 내장 도구를 중복 제거해 시드 목록으로 합칩니다. 여러 Agent에 있는
// 동명 도구(list_assets 등)는 하나로 합치고 Agents는 합집합을 취하며 defaultUnbound 도구는 연결을 비웁니다.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 목록에는 등록하고 수동 연결 가능하지만 기본 연결은 없음(다른 도구처럼 null 대신 [] 저장)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 기본 입력값 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only 기본값 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
