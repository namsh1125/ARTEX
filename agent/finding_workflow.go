package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**취약점 번호 규칙**: finding_id는 독립 취약점 레코드 ID이고 finding_node_id는 탐색 노드 ID입니다. list_findings / list_task_findings / node_detail / get_task_node_detail의 id는 탐색 노드 ID를 유지하므로 동일 응답의 finding_id에서 독립 번호를 읽으세요. get_finding_traffic / bind_finding_traffic은 독립 finding_id를 사용합니다. 기존 update_finding_report의 finding_id 매개변수에는 여전히 finding_node_id를 전달합니다. report_finding 첫 줄의 숫자를 증거 도구에 사용하거나 번호 오류 후 다른 숫자를 추측하지 마세요."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n기본적으로 보고서 Agent가 보고서 작성 전에 트래픽을 확인하고 연결합니다. 보고자는 evidence에 검증 명령, 핵심 출력, 확보한 실제 트래픽 ID와 용도를 남겨 보고서 Agent가 실행 기록과 대조하도록 하세요. 연결을 위해 추가로 패킷을 조회할 필요는 없습니다. 명시적 즉시 연결도 지원합니다. traffic_refs 또는 evidence_hint_id로 검증된 참조를 제출할 수 있으며, 후자는 이 작업의 지정된 hint에서 구조화된 참조를 읽습니다. 하나라도 유효하지 않으면 보고 전체가 실패합니다. TCP/패킷 없음의 경우 이 선택 매개변수가 필요 없습니다. 반환되는 finding_id와 finding_node_id는 각각 독립 레코드와 탐색 노드를 가리킵니다."
		case "add_hint", "add_task_hint":
			note = "\n확인된 취약점을 인계할 때 해당 힌트의 traffic_refs에 검증된 트래픽 ID, 용도, 설명, 순서를 유지하세요(단일 항목은 최상위, 일괄 항목은 해당 hints 원소). text에는 어떤 취약점을 증명하는지 설명하세요. 기존 트래픽 참조를 버리고 텍스트만 인계하지 마세요. 검증하지 않은 후보는 증거로 전달할 수 없습니다."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**트래픽 증거 인계(선택 사항)**: 자동 연결은 기본적으로 취약점 저장 후 보고서 작성 전에 보고서 Agent가 수행합니다. 보고자는 evidence에 검증 명령, 핵심 출력, 확보한 실제 트래픽 ID와 용도를 남기고 작업에서는 intent_id를 포함해 보고서 Agent가 추적할 수 있게 하세요. 연결을 위한 추가 패킷 조회는 필요 없습니다. Auto / Planner가 대신 보고할 때 실행자의 기존 참조를 버리지 마세요. add_hint / add_task_hint의 traffic_refs로 인계할 수 있으며 report_finding의 traffic_refs / evidence_hint_id를 통한 명시적 즉시 연결도 지원합니다. TCP나 패킷이 없는 경우에도 정상 등록하되 ID를 추측하거나 패킷을 채우기 위해 탐색을 반복하지 마세요."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\n플랫폼 대화에 작업 컨텍스트가 없으면 report_finding을 직접 호출하지 마세요. add_task_hint로 기존의 해당 작업에 인계하고 작업 Agent가 등록하도록 한 뒤 list_task_findings로 결과를 확인하세요."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n목표 달성을 판정하기 전에 이번 작업의 기존 증거를 보고/인계하세요. 증거 인계가 끝나지 않았는데 텍스트 취약점이 등록되었다는 이유만으로 작업을 종료하거나 Worker를 취소하지 마세요. 패킷이 없으면 기다리거나 강제로 캡처할 필요는 없습니다."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**보고서 작성 전 트래픽 자동 연결(활성화됨)**: 이번에 발생한 취약점의 트래픽을 확인하고 연결한 뒤 보고서를 작성하세요. 먼저 report_finding 응답 JSON 또는 get_task_node_detail / list_task_findings에서 정확한 finding_id와 finding_node_id를 얻으세요. 취약점 상세, 해당 의도의 실행 기록, 기존 증거 목록을 읽고 보고자가 인계한 실제 ID를 우선 사용하세요. 이번 검증이 HTTP이고 트래픽 도구를 사용할 수 있다면 traffic_search로 후보를 선별하고 traffic_get으로 각 요청/응답이 취약점을 실제로 뒷받침하는지 확인하세요. 도메인과 시간은 필터링 기준일 뿐 귀속을 증명하지 않습니다. 검증된 증거를 재현 순서대로 bind_finding_traffic(finding_id, traffic_refs)으로 연결하고 baseline / proof / verification / supporting 중 역할을 선택해 용도를 설명하세요. 이번 취약점만 처리하고 취약점을 중복 생성하거나 대상을 재탐색하지 마세요. 연결 후 get_finding_traffic을 다시 호출해 최신 version을 얻고 필요한 본문을 읽은 뒤 실제로 읽은 version을 update_finding_report의 evidence_version으로 전달하세요(그 도구의 finding_id 매개변수에는 여전히 finding_node_id 사용). 기존 연결은 중복 추가할 필요 없습니다. TCP, 미수집, 도구 사용 불가, 정확한 일치 없음의 경우 자동 연결을 건너뛰고 텍스트/명령 증거로 정상 작성하며 이유를 설명하세요. 트래픽을 채우려고 추측하지 마세요. 연결 실패 시 성공했다고 주장하지 말고 기존 증거를 유지하며 연결하지 못한 이유를 보고서에 밝히세요."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "선택 사항: 이 힌트의 구체적인 취약점에 해당하는 검증된 트래픽 참조. 순서를 유지합니다. 인계 후 report_finding의 evidence_hint_id로 이 참조를 전달할 수 있습니다.", "items": obj(map[string]any{"traffic_id": str("실제 트래픽 ID"), "role": str("baseline / proof / verification / supporting"), "note": str("이 트래픽이 뒷받침하는 결론")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d는 이 작업의 힌트 노드여야 합니다(상속된 힌트는 직접 연결에 사용할 수 없습니다)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
