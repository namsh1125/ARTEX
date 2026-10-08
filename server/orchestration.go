package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// P2 작업 간 오케스트레이션 도구(docs/跑分编排 §2 P2). 임의 작업 Store의 Manager,
// 일시 중지용 Engine, 작업 생성 흐름이 필요한 host 도구여서 server 계층에 둔다.
// 읽기 도구는 임시 ToolSet을 만들어 해당 도구를 Call하여 기존 작업별 도구를 대상 Store로
// 연결하고 같은 로직을 재사용한다. spawn/pause 제어는 Manager/Engine을 직접 호출한다.
// 트래픽 도구처럼 tools 테이블에 초기 등록하고 에이전트에 연결해야 보인다.

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // Auto용 플랫폼 운영 도구(skill/도구/MCP 생성·수정)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 로드 실패: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id는 필수입니다"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("작업이 없습니다: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 전용 콜백 없는 쓰기 작업의 공통 깨우기. 읽기 도구에서는 no-op
	tsx.SetNotifyHint(t.NotifyHint) // add_hint: 사용자 전략 힌트 추가 트리거 기록 후 planner 깨우기
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"모든 작업의 ID/설명/목표/상태/실행 시간/부모/LLM 설정을 조회합니다. 오케스트레이션 에이전트가 전체 상황, 오래 정체된 작업, 사용 중인 LLM을 파악할 때 사용합니다. 실행 시간은 실행 중이면 생성부터 현재, 종료 상태면 생성부터 마지막 활동까지의 초입니다. llm_profile은 planner/worker 설정 이름이며 (활성 설정)은 전역 활성 설정을 따릅니다.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(활성 설정)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(삭제됨)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"사용 가능한 LLM profile의 ID, 이름, 모델, 형식, 현재 활성 여부를 조회합니다. ID를 spawn_task의 llm_profile_id로 전달해 하위 작업 전용 LLM을 지정할 수 있습니다(예: 정찰은 저렴한 모델, 악용은 강력한 모델). API Key는 포함하지 않습니다.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"독립 하위 작업을 만들고 탐색 엔진을 시작하여 task_id를 반환합니다. 문제 하나나 대상 하나를 별도 작업으로 맡길 때 사용합니다. 선택적 parent_ref에 현재 오케스트레이션의 부모 작업 ID를 넣어 부모·자식 관계를 연결합니다.",
		objSchema(map[string]any{
			"description":            strParam("작업 설명(짧은 제목)"),
			"goal":                   strParam("작업 목표(달성할 내용)"),
			"parent_ref":             strParam("선택: 부모 작업 ID(부모·자식 연결)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("선택: 읽기 전용으로 상속할 원본 작업 ID 목록(최대 %d개). 이미 확인된 자산/결론을 시작점으로 참조합니다. 단순 부모 포인터인 parent_ref와 달리 내용을 상속합니다.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "선택: 하위 작업 planner/worker의 LLM 설정 ID(list_llm_profiles 참고). 생략하면 부모 작업을 상속하고 이후 전역 활성 설정 사용"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "선택: 작업 시간 제한(초). 기한에 정상 마무리를 시작하고 timeout 상태로 종료합니다. 생략 또는 0은 무제한"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "선택: planner 하트비트 간격(초). 이전 계획 종료/작업 시작 후 이 시간 동안 트리거가 없으면 계획을 실행하여 교착 복구와 실행 worker 감독을 수행합니다. 생략 또는 0은 기본 600초(10분)"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "선택: 단순 작업에서 활성화하면 생성 시 설명+목표로 초기 의도를 전달하여 worker가 첫 planner를 기다리지 않고 테스트합니다. 기본 false는 계획 후 실행하는 표준 흐름입니다."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "이름 없는 작업"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal은 필수입니다"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// HTTP 작업 생성과 같은 규칙으로 원본 작업 수, ID 유효성/중복/존재를 검증한다.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("연결 작업은 최대 %d개 선택할 수 있습니다", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("연결 작업 ID가 유효하지 않거나 중복됩니다"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("연결 작업 #%d가 없습니다", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 설정 #%d가 없거나 API Key가 설정되지 않았습니다", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// HTTP createTask와 같은 launchTask 생성 후 흐름을 사용한다:
			// seed + UI에 보이는 백그라운드 목표 분해(0번째 턴/LLM 단계/개별 goal) + engine.Run.
			// seed_first_intent 기본 false는 계획 후 실행. 단순 작업은 활성화하여 work 하나를 즉시 전달 가능.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "지정 작업의 planner/worker 루프를 일시 중지합니다.",
		objSchema(map[string]any{"task_id": strParam("일시 중지할 작업 ID")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("작업이 없습니다: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "task_id로 지정한 작업의 탐색 그래프 개요를 읽습니다(graph_overview와 같은 자산 수/frontier/취약점/커버리지 등).",
		objSchema(map[string]any{"task_id": strParam("작업 ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "task_id로 지정한 작업의 확인된 취약점을 읽습니다(flag/PoC 포함, 항목별 id/task_id/intent_id/vulnclass/severity/요약/상태).",
		objSchema(map[string]any{"task_id": strParam("작업 ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "지정 작업에 전략 힌트를 추가합니다(해당 planner가 다음 의도 생성 시 읽음).\n"+
		"★일괄 처리 우선: 여러 힌트를 hints 배열로 한 번에 제출하세요. 동일 길이/순서의 ids를 반환하며 실패 항목은 0입니다. 단일 힌트는 hints 없이 최상위 text를 지정하세요.",
		objSchema(map[string]any{
			"task_id":      strParam("작업 ID"),
			"hints":        map[string]any{"type": "array", "description": "우선 사용: 힌트 배열. 항목 필드는 최상위와 동일(text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("힌트 내용"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[단일] 힌트 내용"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "연결할 현재 작업의 자산 ID(선택, 0개/1개/여러 개)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"지정 작업의 work(의도) 실행 과정을 확인합니다. get_task_worker_trace(task_id, intent_id)로 단계 요약을 읽고 step_ids=[...]를 추가해 전체 내용을 가져오세요. 한 번에 최대 5개이며 초과하면 앞 5개만 반환합니다.",
		objSchema(map[string]any{
			"task_id":   strParam("작업 ID"),
			"intent_id": map[string]any{"type": "integer", "description": "의도 ID(해당 작업의 work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "선택: 전체 내용을 읽을 단계 ID. 최대 5개만 반환하며 초과분은 omitted_step_ids에 표시"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "지정 작업에서 실행한 work(의도)와 단계 수를 조회하여 검토할 work를 찾습니다(이후 get_task_worker_trace 사용).",
		objSchema(map[string]any{"task_id": strParam("작업 ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "지정 작업의 모든 work 실행 기록을 키워드로 검색하여 일치 단계 요약과 intent_id를 반환합니다.",
		objSchema(map[string]any{"task_id": strParam("작업 ID"), "q": strParam("검색 키워드")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"지정 작업의 탐색 그래프 노드 전체 내용(취약점/사실/의도/목표의 요약과 상세/증거/PoC)을 읽습니다. id는 report_finding 응답이나 list_task_findings의 id 같은 탐색 노드 ID입니다. 보고서 작성 전에 취약점 전체 증거를 가져오세요.",
		objSchema(map[string]any{
			"task_id": strParam("작업 ID"),
			"id":      map[string]any{"type": "integer", "description": "탐색 그래프 노드 id(자산 id가 아님)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"등록된 취약점의 상세 Markdown 보고서를 작성/갱신하며 기존 내용을 전체 교체합니다. finding_id는 report_finding의 finding recorded: <id> 숫자를 사용하세요. 취약점 개요, 영향과 위험, 재현 단계, 증거/PoC, 수정 권고를 포함하는 것이 좋습니다.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "대상 취약점 ID(report_finding이 반환한 id)"},
			"report":           strParam("상세 보고서 전체 내용, Markdown 형식"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic의 증거 version. 보고서가 새로운 증거 변경을 덮어쓰는 것 방지"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 숫자 또는 숫자 문자열 해석 재사용
			if nodeID <= 0 {
				return actool.Errorf("finding_id가 유효하지 않습니다"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d의 취약점 기록이 없습니다(먼저 report_finding으로 등록하세요)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (플랫폼 운영용).
	// SeedTool은 최초 삽입만 적용하므로 기존 행은 seedAutoDefaultBindings에서 연결을 보완한다.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // worker의 list_facts/list_companies/list_worker_traces 기본 연결 해제(한 번)
	s.seedWorkerReadbackRebind()  // 기존 잘못된 해제 수정: search_all_worker_traces/get_worker_trace/node_detail 재연결(한 번)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // 작업 제약 추출 단계를 포함한 새 goals 기본 프롬프트 버전 추가(한 번)
	s.reseedMainAgentPrompt()         // 목표 달성 후 add_intent 시 정식 목표 등록 여부 질문 추가(한 번)
	s.reseedPlannerPrompt()           // 의도 0개의 정당한 사유와 정량 인수 확인을 반영한 planner 프롬프트(한 번)
	s.reseedWorkerPrompt()            // 부정 결론의 증거 기준 추가(한 번)
	s.seedReporterAgent()             // 보고서 작성 에이전트/도구 연결/finding 트리거 초기 생성(한 번)
	s.upgradeReporterTriggerMessage() // 기존 reporter가 evidence_version을 반환하도록 이전(한 번)
	s.seedFindingTrafficTools()       // 사용자 설정을 보존하며 선택 증거 인자와 읽기 전용 도구 추가
	s.seedFindingWorkflowTools()
	// pentest 기본 도구 연결은 마이그레이션이 필요 없다. 새 초기화에서 BuiltinToolSeeds가
	// list_assets/insert_assets/report_finding/list_findings/list_companies를
	// pentest와 함께 생성한다(기존 DB가 없는 기능이므로 마이그레이션 생략).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 일부 내장 에이전트 도구도 코드 기본값으로 갱신한다.
	// goal_met: 기존 설명의 계획 턴 종료라는 오해가 planner에게 빈 턴 종료 수단으로
	// 받아들여져 시작하자마자 작업 전체를 완료로 오판할 수 있었다.
	// insert_assets: 작업 관련 여부와 커버리지 포함을 결정하는 related 인자를 추가했다.
	// SeedTool은 최초 삽입만 하므로 기존 스키마에 새 인자를 반영해야 한다.
	// list_facts: 페이지 조회용 limit/before/q 추가. 기존 빈 스키마를 갱신하지 않으면
	// 도구 관리에 인자 없음으로 표시되고 모델도 설명을 받지 못한다.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform 도구 스키마를 코드 기본값으로 갱신했습니다(일회성)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] planner의 goal_met 연결 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt는 goals를 현재 코드 기본값으로 갱신한다. 제약을 먼저 추출하고
// 목표를 분해하는 단계가 추가되었지만 최초 삽입 전용 SeedPromptIfEmpty는 기존 v1에 반영하지 못한다.
// ResetPromptToDefault로 새 버전을 추가하고 전환하며
// 기존 사용자 정의 버전은 이력에서 복구할 수 있다. settings flag로 한 번만 수행한다.
// 이후 기본값 변경 시 flag를 올린다. 새 DB는 이미 최신 기본값이므로 처리하지 않는다.
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 새 DB에 에이전트가 없으면 seedPrompts가 최신 기본값을 생성하므로 이전 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 새 DB의 현재 버전은 이미 코드 기본값이므로 중복 버전을 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals 프롬프트 새 기본값 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] goals 프롬프트에 제약 추출 단계를 포함한 새 기본 버전 추가(일회성)")
}

// reseedMainAgentPrompt는 mainagent를 현재 코드 기본값으로 갱신한다. 모든 목표 달성 후
// add_intent 직접 할당 시 정식 목표 등록 여부를 묻는 안내가 추가되었지만 최초 삽입 전용
// SeedPromptIfEmpty는 기존 버전을 갱신하지 못한다. ResetPromptToDefault로 새 버전을 추가하고 전환한다.
// 기존 사용자 버전은 이력에서 복구 가능하며 settings flag로 한 번만 수행한다. 새 DB는
// 이미 최신 기본값이므로 생략한다. reseedGoalsPrompt와 동일 구조다.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 새 DB는 seedPrompts가 최신 기본값을 생성하므로 이전 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 현재 버전이 코드 기본값이면 중복 버전을 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent 프롬프트 새 기본값 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] mainagent 프롬프트에 목표 달성 후 목표 등록 질문을 포함한 새 기본 버전 추가(일회성)")
}

// reseedPlannerPrompt는 planner를 현재 기본값으로 갱신한다. 기본문을 간소화하고 절제를 중복 제거로
// 한정하며 깊이 우선, 목표 미달성·실행 의도 없음 시 의도 생성 의무, 부정 결론 재검토 상한을 추가했다.
// 기본값이 실질 변경되면 아래 flag(현재 v2)를 올린다. 최초 삽입 전용 SeedPromptIfEmpty 대신
// ResetPromptToDefault로 새 버전을 추가하고 전환하며 기존 사용자 버전은
// 이력에서 복구할 수 있다. settings flag로 한 번만 수행하고 최신 기본값이 있는 새 DB는 생략한다.
// reseedGoalsPrompt와 동일 구조다.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 새 DB는 seedPrompts가 최신 기본값을 생성하므로 이전 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 현재 버전이 코드 기본값이면 중복 버전을 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner 프롬프트 새 기본값 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] planner 새 기본 버전 추가(간소화/중복 제거/깊이 우선/부정 재검토 상한, 일회성)")
}

// reseedWorkerPrompt는 worker 기본값을 갱신한다. record_fact에서 부정 결론에 관찰과 잠정 해석을
// 쓰라는 문구를 제거하고 confidence(observed/inferred)와 의도 수단 소진 여부를 분리해 계획자 오해를 줄였다.
// facts 분할도 완전히 독립적이고 합칠 수 없는 드문 경우로 제한했다. flag v3으로 기존 DB를 갱신한다.
// 최초 삽입 전용 SeedPromptIfEmpty 대신 새 버전을 추가·전환하며 기존 버전은 이력에서 복구 가능하다.
// settings flag로 한 번만 수행하고 새 DB는 생략한다. reseedGoalsPrompt와 동일 구조다.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 새 DB는 seedPrompts가 최신 기본값을 생성하므로 이전 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 현재 버전이 코드 기본값이면 중복 버전을 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker 프롬프트 새 기본값 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] worker 새 기본 버전 추가(컨텍스트 조회를 list_assets/list_findings로 정리, list_facts/node_detail/asset_neighbors 제거, 일회성)")
}

// reporterToolCallMessage는 자동 연결 설정과 무관하게 보고서 전에 get_finding_traffic을
// 한 번 읽도록 요구한다. 이 도구는 캡처 설정과 무관한 읽기 전용이므로 수동 연결 증거도 읽을 수 있다.
// 자동 연결 활성 시에만 읽도록 하면 기본 비활성 상태에서 evidence_version을 전달하지 않아
// SetFindingReportVersionByNodeID가 legacy 의미로 -1을 저장한다. 그러면 상세와 Markdown 내보내기에
// 증거 변경·보고서 갱신 필요 표시가 계속 남고 UI에서 지울 방법이 없다.
const reporterToolCallMessage = "방금 report_finding으로 취약점이 등록되었습니다. 응답 JSON에서 finding_id(독립 취약점 기록 ID)와 finding_node_id(탐색 노드 ID)를 읽으세요. " +
	"먼저 get_finding_traffic(finding_id)으로 현재 증거 목록과 version을 읽으세요(빈 목록도 정상이며 보고서는 그대로 작성). " +
	"실행 지침에 자동 연결이 활성화되어 있으면 읽기 전에 이번 취약점 트래픽을 검증하여 연결하세요. 노드 상세에는 finding_node_id를 사용하세요. " +
	"마지막에 update_finding_report(finding_id=finding_node_id, report, evidence_version=실제로 읽은 버전)으로 저장하세요. " +
	"evidence_version을 빠뜨리면 보고서가 계속 갱신 필요로 표시됩니다. 두 ID를 혼동하지 마세요."

// 0.3.8 이하의 이전 트리거 메시지. 원문과 정확히 같은 기록만 이전하며 사용자 수정은 보존한다.
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage는 기존 DB에 남은 기본 reporter 트리거를 새 버전으로 갱신한다.
// seedReporterAgent는 reporter_agent_seed_v1과 새 에이전트 생성 시에만 트리거를 작성하므로
// 업그레이드 DB는 새 문구를 받지 못한다. seedFindingTrafficTools가 evidence_version을 추가해도
// reporter에게 사용을 안내하지 못하므로 수정되지 않은 문구만 한 번 갱신한다.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 한 번만 시도
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] 트리거 읽기 실패: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // 사용자 수정 또는 finding 트리거가 아니면 유지
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 트리거 메시지 갱신 실패: %v", err)
			return
		}
		log.Printf("[reporter] evidence_version 조회 및 반환을 안내하도록 트리거 메시지 갱신")
	}
}

// seedReporterAgent는 UI에서 편집/삭제 가능한 builtin=false 보고서 작성 에이전트를 만든다.
// update_finding_report와 작업 조회 도구를 연결하고 report_finding 호출 트리거로
// 취약점 등록마다 상세 보고서를 작성한다. settings flag로 한 번만 생성하여 사용자 삭제 후 재생성하지 않는다.
// 오케스트레이션 도구가 위에서 SeedTool로 저장되어 연결할 수 있다.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // 사용자가 직접 만든 key가 있으면 덮어쓰지 않음
	}
	a, err := s.m.pg.CreateAgent("reporter", "보고서 작성",
		"취약점 발견 시 자동으로 증거와 실행 기록을 조회하여 상세 Markdown 보고서를 작성하고 저장합니다.")
	if err != nil {
		log.Printf("[reporter] 에이전트 생성 실패: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] 프롬프트 초기 생성 실패: %v", err)
	}
	// parallel + none: 취약점마다 별도 보고서를 작성하고 여러 finding을 동시에 처리한다.
	// 기본 all이면 여러 finding이 한 실행으로 합쳐져 병렬 의미가 없으므로 merge는 none이어야 한다.
	// maxParallel=5로 동시 보고서 세션과 순간 LLM 호출을 제한한다.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 트리거 실행 정책 설정 실패: %v", err)
	}
	// 보고서 쓰기와 증거/실행 기록/상황 읽기 도구 연결.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] 도구 연결 실패: %v", err)
	}
	// report_finding 호출 시 트리거. finding recorded: <id> 응답에 finding ID가 있고
	// 작업 ID도 트리거 메시지에 포함된다.
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] 트리거 생성 실패: %v", err)
	}
	log.Printf("[reporter] 보고서 작성 에이전트와 finding 트리거 초기 생성 완료")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // 기본 연결을 worker에서 planner로 전환
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope 기본 연결 실패: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope 연결 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] worker의 %s 연결 해제 실패: %v", k, err)
			return // 오류 시 flag를 저장하지 않고 다음 시작에 재시도
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: node_detail 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 기록/상세 도구 재연결 실패: %v", err)
		return // 오류 시 flag를 저장하지 않고 다음 시작에 재시도
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 기존 자산 도구 이름 교체, insert_assets/add_company_scope 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Auto의 플랫폼 운영에 필요한 자산 조회/등록과 기업 범위 관리 도구.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
