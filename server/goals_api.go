package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 개요의 목표 관리 수동 CRUD API. set_goals 도구와 같은 goal 노드에 저장하지만
// UI에서 사람이 직접 편집한다. 추가/수정은 admitTask resume으로 작업을 되살려
// 종료→running, 중지 해제, 필요 시 대기열 등록한다. 삭제는 되살리지 않는다(제품 결정).
// 각 변경은 의도 CRUD처럼 beginTaskOperation/decInflight로 작업 삭제 경합을 방지한다.

// listGoals는 text/vulnclass/state를 분리한 작업 목표를 목표 관리 카드용으로 반환한다.
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal은 목표를 작업 루트 spawns 아래에 저장하고 목표 추가 트리거로 planner를 깨운 뒤
// 작업을 되살려 새 목표 기준으로 달성 여부를 재판정하게 한다.
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제 중이어서 목표를 추가할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "목표 내용은 비워 둘 수 없습니다")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 사용자 목표 추가 트리거를 기록하고 planner 깨우기
	s.reviveTask(t)              // 완료/일시 중지 작업을 실행 상태로 되돌려 계속 진행
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "목표 저장 후 읽기 실패")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal은 목표 text/vulnclass를 수정하고 old→new 변경 트리거로 planner를 깨운 뒤
// 작업을 되살려 새 목표에 맞게 방향을 조정하도록 한다.
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제 중이어서 목표를 수정할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "목표 내용은 비워 둘 수 없습니다")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "목표가 없습니다")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 사용자 목표 변경 트리거 기록 및 planner 깨우기
	s.reviveTask(t)                   // 추가와 동일하게 작업을 되살려 새 목표로 재판정
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "목표 갱신 후 읽기 실패")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal은 목표와 간선/연결점을 연쇄 물리 삭제한 뒤 삭제 트리거로 planner를 깨워
// 남은 목표를 재판정하게 한다. 제품 결정에 따라 작업 자체는 되살리지 않는다.
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제 중이어서 목표를 삭제할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "목표가 없습니다")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 목표 삭제 트리거 기록 및 planner 깨우기(작업은 되살리지 않음)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
