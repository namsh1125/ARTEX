package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 내장 Auto용 플랫폼 운영 도구: skill, 사용자 도구, MCP 생성·수정. host 도구로
// tools에 초기 등록하고 auto에 기본 연결하며 hostTools로 주입한다. 기존 DB/파일 시스템 로직 재사용.

func (s *Server) platformTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolCreateSkill(),
		s.toolUpdateSkillFile(),
		s.toolCreateCustomTool(),
		s.toolUpdateCustomTool(),
		s.toolCreateMCP(),
		s.toolUpdateMCP(),
		s.toolDeleteAssetsByHost(),
	}
}

// platformToolKeys are the tool keys the Auto agent gets bound by default.
var platformToolKeys = []string{
	"create_skill", "update_skill_file",
	"create_custom_tool", "update_custom_tool",
	"create_mcp", "update_mcp",
	"delete_assets_by_host",
}

// ---- assets ----

// toolDeleteAssetsByHost hard-deletes every asset tied to one host (exact match).
// Platform-level (not a per-task tool): operates on the global, cross-task asset database.
func (s *Server) toolDeleteAssetsByHost() actool.CoreTool {
	return wrTool("delete_assets_by_host",
		"host가 정확히 일치하는 자산을 삭제합니다. 해당 도메인/하위 도메인과 그 아래 service/endpoint를 삭제합니다.\n"+
			"소문자 변환과 공백 제거 후 정확히 대조하며 부분 일치/와일드카드가 아닙니다.\n"+
			"루트 도메인(example.com)은 하위 도메인과 서비스/엔드포인트까지 삭제합니다. 하위 도메인(a.example.com)이나 IP는 해당 host와 그 서비스/엔드포인트만 삭제합니다.\n"+
			"⚠️ 여러 작업이 공유하는 전역 자산 DB에서 물리 삭제하며 되돌릴 수 없습니다.",
		objSchema(map[string]any{
			"host": strParam("삭제할 도메인/하위 도메인/IP. 정확 일치, 예: example.com, a.example.com, 1.2.3.4"),
		}, "host"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			as := s.assetStore()
			if as == nil {
				return actool.Errorf("자산 DB가 초기화되지 않았습니다"), nil
			}
			var a struct {
				Host string `json:"host"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Host) == "" {
				return actool.Errorf("host는 비워 둘 수 없습니다"), nil
			}
			counts, err := as.DeleteByHost(a.Host)
			if err != nil {
				return actool.Errorf("삭제 실패: " + err.Error()), nil
			}
			var total int64
			for _, n := range counts {
				total += n
			}
			return jsonResult(map[string]any{
				"host":            a.Host,
				"deleted":         total,
				"deleted_by_type": counts,
			})
		})
}

// ---- skills ----

func (s *Server) toolCreateSkill() actool.CoreTool {
	return wrTool("create_skill",
		"agentskills.io 규격의 SKILL.md로 새 skill을 만듭니다. name에는 소문자/숫자/하이픈을 사용하세요.",
		objSchema(map[string]any{
			"name":         strParam("skill 이름(소문자로 시작, 영문자/숫자/하이픈)"),
			"description":  strParam("필수 skill 설명: 기능과 사용 시점"),
			"instructions": strParam("Markdown 본문 설명(선택)"),
		}, "name", "description"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, Description, Instructions string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("잘못된 skill 이름입니다(소문자로 시작, 영문자/숫자/하이픈만, 최대 64자)"), nil
			}
			if strings.TrimSpace(a.Description) == "" {
				return actool.Errorf("description은 필수입니다"), nil
			}
			path := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(path); err == nil {
				return actool.Errorf("skill이 이미 있습니다: " + a.Name), nil
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var b strings.Builder
			b.WriteString("---\n")
			fmt.Fprintf(&b, "name: %s\n", a.Name)
			fmt.Fprintf(&b, "description: %s\n", a.Description)
			b.WriteString("---\n")
			if strings.TrimSpace(a.Instructions) != "" {
				b.WriteString(a.Instructions)
			} else {
				fmt.Fprintf(&b, "## %s\n\n1. \n2. \n3. \n", a.Name)
			}
			if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
				_ = os.RemoveAll(path)
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill created: " + a.Name), nil
		})
}

func (s *Server) toolUpdateSkillFile() actool.CoreTool {
	return wrTool("update_skill_file",
		"skill 안의 파일 하나를 작성/덮어씁니다(기본 SKILL.md). 내용 수정이나 스크립트/참조 추가에 사용합니다.",
		objSchema(map[string]any{
			"name":    strParam("skill 이름"),
			"file":    strParam("상대 경로(선택, 기본 SKILL.md, 예: scripts/run.py)"),
			"content": strParam("파일 전체 내용"),
		}, "name", "content"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, File, Content string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("skill 이름이 유효하지 않습니다"), nil
			}
			skillPath := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(skillPath); os.IsNotExist(err) {
				return actool.Errorf("skill이 없습니다: " + a.Name), nil
			}
			rel := strings.TrimSpace(a.File)
			if rel == "" {
				rel = "SKILL.md"
			}
			clean, msg := skillRelPath(rel)
			if msg != "" {
				return actool.Errorf("잘못된 경로: " + msg), nil
			}
			full := filepath.Join(skillPath, clean)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill file written: " + a.Name + "/" + clean), nil
		})
}

// ---- custom tools ----

type customToolToolInput struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Exec        json.RawMessage `json:"exec"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Deferred    bool            `json:"deferred"`
	Enabled     *bool           `json:"enabled"`
}

func customToolSchema(keyDesc string) map[string]any {
	return objSchema(map[string]any{
		"key":         strParam(keyDesc),
		"description": strParam("모델에 전달할 설명"),
		"kind":        strParam("shell | command | script(Python 전용) | http. shell은 bash에서 바로 호출할 수 있음을 알리는 환경 선언이며 exec/schema가 필요 없습니다. 나머지는 exec가 필요합니다"),
		"exec":        map[string]any{"type": "object", "description": "실행 명세(shell 제외): command→{command}; script→{code}; http→{method,url,headers,body,proxy,use_recording_proxy}"},
		"schema":      map[string]any{"type": "object", "description": "인자 JSON Schema(shell/command/script는 생략 가능, http는 properties를 포함하여 필수)"},
		"agents":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "연결할 에이전트 key(선택)"},
		"deferred":    map[string]any{"type": "boolean", "description": "지연 로드 여부(shell에는 무효, 드물게 쓰는 command/script/http 도구만 활성화)"},
		"enabled":     map[string]any{"type": "boolean", "description": "활성화 여부(기본 true)"},
	}, "key", "kind")
}

func toDBTool(a customToolToolInput) *db.Tool {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	return &db.Tool{
		Key: a.Key, Description: a.Description, Schema: a.Schema, Agents: a.Agents,
		Enabled: enabled, Kind: a.Kind, Exec: a.Exec, Deferred: a.Deferred,
	}
}

func (s *Server) toolCreateCustomTool() actool.CoreTool {
	return wrTool("create_custom_tool", "중요: 플랫폼에 없는 도구를 설치하면 이 도구로 등록하여 플랫폼에서 호출할 수 있게 하세요. 사용자 정의 도구(shell/command/script/http)를 만듭니다. shell은 bash 환경 선언으로 key+description+agents만 필요하며 exec/schema는 필요 없습니다.",
		customToolSchema("도구 key(소문자로 시작, 영문자/숫자/밑줄)"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			a.Key = strings.TrimSpace(a.Key)
			if !reToolKey.MatchString(a.Key) {
				return actool.Errorf("key는 소문자로 시작하고 소문자/숫자/밑줄만 포함해야 합니다"), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind는 shell / command / script / http여야 합니다"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http 도구는 비어 있지 않은 인자 JSON Schema가 필요합니다"), nil
			}
			if exist, _ := s.m.pg.GetTool(a.Key); exist != nil {
				return actool.Errorf("key가 이미 있습니다: " + a.Key), nil
			}
			if err := s.m.pg.CreateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool created: " + a.Key), nil
		})
}

func (s *Server) toolUpdateCustomTool() actool.CoreTool {
	return wrTool("update_custom_tool", "key로 기존 사용자 정의 도구를 수정합니다.",
		customToolSchema("수정할 사용자 정의 도구 key"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			existing, _ := s.m.pg.GetTool(a.Key)
			if existing == nil || existing.System {
				return actool.Errorf("사용자 정의 도구만 수정할 수 있습니다: " + a.Key), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind는 shell / command / script / http여야 합니다"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http 도구는 비어 있지 않은 인자 JSON Schema가 필요합니다"), nil
			}
			if err := s.m.pg.UpdateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool updated: " + a.Key), nil
		})
}

// ---- MCP ----

type mcpToolInput struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url"`
	Enabled   *bool           `json:"enabled"`
	Insecure  *bool           `json:"insecure"`
}

func mcpSchema(withID bool) map[string]any {
	props := map[string]any{
		"name":      strParam("MCP 서버 이름"),
		"transport": strParam("stdio | http / sse"),
		"command":   strParam("stdio 시작 명령(예: npx)"),
		"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "명령 인자 배열"},
		"env":       map[string]any{"type": "object", "description": "환경 변수 {KEY:VALUE}"},
		"url":       strParam("http/sse URL"),
		"enabled":   map[string]any{"type": "boolean", "description": "활성화 여부(기본 true)"},
		"insecure":  map[string]any{"type": "boolean", "description": "http TLS 인증서 검증 생략(자체 서명 인증서면 true, 기본 false)"},
	}
	required := []string{"name", "transport"}
	if withID {
		props["id"] = map[string]any{"type": "integer", "description": "수정할 MCP 서버 ID"}
		required = []string{"id", "name", "transport"}
	}
	return objSchema(props, required...)
}

func (a mcpToolInput) toDB() *db.MCPServer {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	insecure := false
	if a.Insecure != nil {
		insecure = *a.Insecure
	}
	return &db.MCPServer{
		ID: a.ID, Name: a.Name, Transport: a.Transport, Command: a.Command,
		Args: a.Args, Env: a.Env, URL: a.URL, Enabled: enabled, Insecure: insecure,
	}
}

func (s *Server) toolCreateMCP() actool.CoreTool {
	return wrTool("create_mcp", "MCP 서버(stdio/http/sse)를 만듭니다. 생성 후 도구는 에이전트별 가시성 권한을 부여해야 합니다.",
		mcpSchema(false),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			a.ID = 0
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Transport) == "" {
				return actool.Errorf("name / transport는 필수입니다"), nil
			}
			id, err := s.m.pg.SaveMCP(a.toDB())
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp created: id=%d name=%s", id, a.Name)), nil
		})
}

func (s *Server) toolUpdateMCP() actool.CoreTool {
	return wrTool("update_mcp", "ID로 기존 MCP 서버를 수정합니다.",
		mcpSchema(true),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			if a.ID == 0 {
				return actool.Errorf("id는 필수입니다"), nil
			}
			if _, err := s.m.pg.SaveMCP(a.toDB()); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp updated: id=%d", a.ID)), nil
		})
}
