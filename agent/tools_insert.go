package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates는 삽입할 자산의 도메인/IP/URL 후보 문자열을 추출해 자산 차단 규칙과 비교합니다.
// URL의 host를 분리해 분류하므로 URL만 있는 서비스/엔드포인트도 도메인/IP 규칙에 일치할 수 있습니다.
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel은 차단 안내에 사용할 삽입 대상 자산의 짧은 식별자를 반환합니다.
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(알 수 없음)"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"새로 발견한 자산을 일괄 등록합니다. 한 번에 여러 유형을 혼합할 수 있습니다(type 열거형 참조).\n"+
			"유형별 필수 필드: root_domain→domain; ip→ip(IPv4/IPv6만 허용, 호스트명 불가); subdomain→domain; app→app_name; service(HTTP)→url; service(비HTTP)→service_name+port(ip/domain 중 하나 이상); endpoint→url+method. 나머지는 각 필드 설명을 참고하세요.\n"+
			"auth/technologies/params는 기존 값을 덮어쓰지 않고 추가 병합(append)합니다.\n"+
			"반환: {results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id는 모델에 노출하지 않습니다. Worker 소속 작업은 프로그램이 SetTaskID로 확정합니다(handler 참고).
			"assets": map[string]any{
				"type":        "array",
				"description": "자산 배열, 각 원소는 자산 레코드 하나",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "자산 유형",
					},
					// root_domain / subdomain
					"domain":      str("루트 도메인 또는 하위 도메인(root_domain/subdomain 필수)"),
					"icp":         str("ICP 등록 번호(선택)"),
					"record_type": str("DNS 레코드 유형: A/AAAA/CNAME/MX 등(subdomain 선택)"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS 레코드 값 목록(subdomain 선택, 예: [\"1.2.3.4\",\"2.3.4.5\"])",
					},
					// ip
					"ip": str("IPv4/IPv6 주소만 허용하며 호스트명은 type=subdomain의 domain 필드에 입력하세요. ip 유형 필수, service/endpoint에서 IP 연결용으로 선택 입력 가능"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "이 IP에 연결된 도메인 목록(ip 유형 선택)",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "열린 포트 목록(ip 유형 선택)",
						"items": obj(map[string]any{
							"port":    intp("포트 번호"),
							"service": str("서비스 이름(http/ssh/mysql 등, 선택)"),
						}, "port"),
					},
					// app
					"app_name":    str("애플리케이션 이름(app 유형 필수)"),
					"bundle_id":   str("Bundle ID(app 유형 선택)"),
					"category":    str("애플리케이션 분류(선택)"),
					"description": str("애플리케이션 설명(선택)"),
					"app_icp":     str("애플리케이션 ICP 등록(선택)"),
					"company_id":  intp("소속 기업 id(app 유형 선택, app은 scope로 자동 귀속할 수 없어 명시해야 함. add_company_scope 반환 id 사용)"),
					// service (http)
					"url":         str("프로토콜과 포트를 포함한 전체 URL(HTTP 서비스 필수, service_type은 http로 자동 설정)"),
					"status_code": intp("HTTP 응답 상태 코드(200/301/403/404 등, 선택)"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP 응답 본문 바이트 수(선택)",
					},
					"page_title":   str("페이지 <title> 내용(선택)"),
					"favicon_mmh3": str("favicon MMH3 해시(선택)"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "지문/기술 스택 목록(예: [\"Nginx\",\"Vue\",\"Bootstrap\"], 선택)",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "발견한 인증 정보 목록, 각 항목은 type/username/password 등 포함(선택, 덮어쓰지 않고 추가)",
						"items":       map[string]any{"type": "object"},
					},
					// service(other, 비HTTP)
					"service_name": str("서비스 이름(ssh/mysql/redis 등, 비HTTP service 필수)"),
					"port":         intp("포트 번호(비HTTP service 필수)"),
					// endpoint
					"method": str("HTTP 메서드: GET/POST/PUT/PATCH/DELETE 등(endpoint 필수)"),
					"params": map[string]any{
						"type":        "array",
						"description": "요청 매개변수 목록, 각 항목은 location(query/body/header/path)/name/value/type 포함(선택, 덮어쓰지 않고 추가)",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets 비활성화: AssetStore가 초기화되지 않았습니다"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id는 프로그램이 확정하며(worker: SetTaskID) 모델 입력을 받지 않습니다. 누락/오입력으로
			// 자산이 작업에 귀속되지 않거나 잘못 연결되는 것을 막습니다. 작업 컨텍스트 없는 auto/pentest/chat은 t.taskID=0입니다.
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// 자산 제어 규칙은 한 번 로드하며 읽기 실패 시 판정을 건너뛰어 삽입을 막지 않습니다.
			// 차단 규칙 = 전역 ∪ 작업별 block, 허용 규칙 = 작업별 allow.
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// 자산 제어: 차단 후 허용을 확인하며 거부 자산은 Upsert 및 후속 부수 효과를 모두 건너뜁니다.
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("자산 %s %s, 삽입이 금지되었습니다", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent가 insert_assets로 등록"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker 의도 #%d가 insert_assets로 등록", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// 테스트 범위 자동 추가(source='auto')는 Worker가 최상위에 명시적으로 삽입한 항목에만
				// 유형별 보수적 범위를 적용합니다. 부수 효과로 파생된 자산은 거치지 않아 범위를 임의로 넓히지 않으며 taskID=0이면 무동작입니다.
				// 커버리지 설정과 무관합니다. task_scope는 작업 범위 경계이자 목록/조회 필터 기준이며
				// 커버리지 설정은 지표의 분모로 쓸지만 결정하고 범위 자체의 누적 여부는 결정하지 않습니다.
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"도메인/IP/CIDR/ICP 등록/기업 키워드를 회사의 자산 범위에 추가합니다. 도메인, 네트워크, ICP는 일치 자산을 자동 귀속하고 키워드는 Agent의 범위 참고용입니다.\n"+
			"회사명은 고유합니다. company가 없으면 생성하고 있으면 범위만 병합합니다.\n"+
			"scope는 한 줄에 하나씩 입력하며 루트 도메인/URL/IP/CIDR/ICP 등록/기업 키워드를 자동 인식합니다.\n"+
			"reason에 귀속 근거(whois/인증서/ASN 등)를 반드시 설명하세요.\n"+
			"보호 규칙: 최상위 도메인 자체나 지나치게 넓은 대역은 거부합니다(IPv4 /16-/32, IPv6 /32-/128). 잘못된 줄은 건너뛰고 errors에 반환합니다.",
		obj(map[string]any{
			"company": str("고유 회사명(없으면 생성, 있으면 재사용)"),
			"scope":   str("자산 범위, 한 줄에 하나: 도메인/URL/IP/CIDR/ICP 등록/기업 키워드"),
			"reason":  str("귀속 근거(증거/출처), 필수 입력"),
			"logo":    str("회사 아이콘 URL(선택, 새 회사 생성 시만 적용)"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope 비활성화: CompanyStore가 초기화되지 않았습니다"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company는 비워 둘 수 없습니다"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("회사 생성/조회 실패: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"현재 작업의 테스트 범위를 추가합니다. 이 작업의 허가 경계이자 자산 테스트 커버리지의 분모입니다.\n"+
			"지원 kind: company(회사 전체 자산) / root_domain(모든 하위 도메인 포함) / subdomain(정확한 단일 하위 도메인) / ip / cidr / icp / keyword.\n"+
			"Worker가 접한 호스트는 정확한 하위 도메인으로 자동 추가됩니다. 이 도구는 루트 도메인/회사 전체로 범위를 명시적으로 넓히거나 특정 하위 도메인/IP를 보충할 때 사용합니다.\n"+
			"value: company는 기존 회사명 또는 id, root_domain/subdomain은 도메인, ip/cidr는 IP 또는 대역, icp/keyword는 등록 번호 또는 기업 키워드입니다.\n"+
			"감사 가능한 근거를 reason에 반드시 적고 여러 항목은 entries 배열을 사용하세요.",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "일괄: [{kind, value}]. kind∈company/root_domain/subdomain/ip/cidr/icp/keyword.", "items": map[string]any{"type": "object"}},
			"kind":    str("[단일] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[단일] 회사명 또는 id / 도메인 / IP / CIDR / ICP / 키워드"),
			"reason":  str("추가 근거(감사용), 필수 입력"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope 비활성화: AssetStore가 초기화되지 않았습니다"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope에는 작업 컨텍스트가 필요합니다(현재 task 없음)"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 단일 모드
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"현재 및 직접 연결된 작업의 범위에서 사실 기준점이 아직 다루지 않은 자산을 조회합니다. 연결 범위는 읽기 전용이며 추가 테스트 여부는 직접 판단하세요.\n"+
			"자산 유형으로 필터링할 수 있습니다: root_domain/subdomain/service/app/endpoint/ip.\n"+
			"페이지: page는 1부터, page_size 기본 10. 반환 {assets:[{id,type,label}], total, page, page_size}. 작업 컨텍스트에서만 사용 가능합니다.",
		obj(map[string]any{
			"type":      str("자산 유형 필터(선택): root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("페이지 번호, 1부터 시작(기본 1)"),
			"page_size": intp("페이지당 개수(기본 10)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets 비활성화: AssetStore가 초기화되지 않았습니다"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets에는 작업 컨텍스트가 필요합니다"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"자산 DB를 DSL 검색 또는 id/ids 직접 조회하며 페이지 조회를 지원합니다. 현재 및 직접 연결된 작업의 테스트 범위에 속한 자산만 반환합니다.\n"+
			"DSL: field=value 부분 일치(ILIKE) | field==value 정확 일치 | field!=value 제외 | 숫자 필드 > >= < <= 지원 | 단독 단어=전체 텍스트 부분 일치. AND/OR 조합(AND 우선), 괄호 그룹 가능. 자산 유형은 DSL 대신 별도 type 매개변수에 넣으세요.\n"+
			"id/ids가 없으면 dsl은 필수입니다(조건 없는 전체 조회 금지).\n"+
			"필드: domain(루트/하위/서비스 도메인), root_domain, ip, url, page_title, icp, service_name, app_name, method(GET/POST 등), service_type(http|other), record_type(A/CNAME 등), technology(배열, =부분 ==정확), port/status_code/company_id(정수).\n"+
			"예: status_code>=400 AND technology=shiro ; (port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL 검색식(구문/필드는 도구 설명 참조). id/ids가 없으면 필수.`),
			"type":   str("자산 유형 필터: root_domain|ip|subdomain|app|service|endpoint(별도 필드이며 dsl과 함께 적용, type만으로는 조회 불가하고 dsl 필요)"),
			"id":     intp("단일 자산 id 직접 조회(선택, dsl/type과 동시 사용 불가)"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "여러 자산 id 직접 조회(선택, dsl/type과 동시 사용 불가)"},
			"limit":  intp("반환 한도, 기본 10(선택)"),
			"offset": intp("페이지 오프셋, 기본 0(선택)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets 비활성화: AssetStore가 초기화되지 않았습니다"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id/ids가 없으면 dsl을 비울 수 없습니다. 조건 없는 전체 자산 조회는 금지되므로 검색 조건을 입력하세요"), nil
			}
			if err != nil {
				return actool.Errorf("DSL 오류: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies (기업) with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"자산 DB의 기업/회사와 자산 범위(scope), 귀속 자산 수를 조회해 회사를 확인하고 "+
			"company_id를 얻습니다(insert_assets의 app 연결, list_assets의 company_id 필터에 사용). "+
			"search로 회사명을 대소문자 무시 부분 검색할 수 있으며 비워 두면 전체를 반환합니다.",
		obj(map[string]any{
			"search": str("회사명 부분 검색(선택, 대소문자 무시), 비워 두면 전체 반환"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies 비활성화: CompanyStore가 초기화되지 않았습니다"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("회사 조회 실패: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings 유지: 보고 전에 현재 작업의 확인된 취약점을 조회해 중복 보고를 피합니다.
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope는 Worker에 주지 않습니다. 기업 범위 정의는 계획/주 Agent/Auto 책임이며 Worker는 탐색만 실행합니다.
		t.insertAssets(), t.listAssets(),
		// work 간 조회: 다른 work의 관찰을 재사용해 중복 작업을 피합니다.
		// search_all_worker_traces: intent_id를 몰라도 키워드로 전체 단계 검색.
		// get_worker_trace: 특정 work의 단계 조회/검색/전체 내용 읽기.
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail: intent_id/노드 id를 확보한 Worker가 해당 노드 전체 상세를 조회.
		t.nodeDetail(),
		// 다음 도구는 여전히 Worker에 주지 않고 planner/main에만 제공합니다. 컨텍스트 읽기와 work 간 검토는
		// 계획 책임이며 Worker는 단일 의도 실행/기록만 담당합니다: list_facts / list_companies / list_worker_traces.
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work: 사용자가 실행 의도에 실시간 방향 수정 지시를 주입(중단/진행 손실 없음).
		t.steerWorkTool(),
		// set_goals: 사용자가 실행 중 새 최종 목표 추가(계획자가 달성 여부 재판정).
		t.setGoals(),
		// set_constraints: 사용자가 실행 중 allow/deny 제약을 추가/수정해 planner/worker 탐색 경계 제한.
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets: 현재 범위의 미테스트 자산을 유형/페이지별로 조회해 추가 테스트 판단.
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
