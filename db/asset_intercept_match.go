package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 자산 차단 규칙의 일치/실행 계층이다. asset_intercept.go는 규칙 저장만 담당하고
// 여기서는 대상 자산의 도메인/IP/URL을 활성 규칙과 대조한다. 에이전트 도구(add_intent,
// insert_assets)가 의도 전달이나 자산 삽입 전에 호출하며 일치하면 거부한다.

// AssetInterceptKindLabel은 에이전트 안내 메시지에 사용할 kind의 한국어 레이블을 반환한다.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "도메인(정확히 일치)"
	case "exact_ip":
		return "IP(정확히 일치)"
	case "exact_url":
		return "URL(정확히 일치)"
	case "fuzzy_domain":
		return "도메인(부분 일치)"
	case "fuzzy_ip":
		return "IP(부분 일치)"
	case "fuzzy_url":
		return "URL(부분 일치)"
	case "cidr":
		return "CIDR 네트워크 대역"
	}
	return kind
}

// Reason은 읽기 쉬운 일치 이유를 반환한다. 예: 자산 차단 규칙 일치 [도메인(부분 일치): .gov.cn](비고).
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("자산 차단 규칙 일치 [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne은 활성 규칙 하나가 도메인/IP/URL 후보와 일치하는지 판정하고 실제 일치 값을 반환한다.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules는 도메인/IP/URL 후보와 처음 일치하는 활성 규칙과 실제 일치 값을 반환한다.
// insert_assets가 아직 저장되지 않은 assetInputItem 원본 입력을 대조할 때 사용한다.
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates는 저장된 자산에서 차단 규칙 대조용 도메인/IP/URL 후보를 추출한다.
// URL의 host를 분리하고 분류하여 URL만 있는 서비스 자산도 도메인/IP 규칙과 일치할 수 있게 한다.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
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

// InterceptLabel은 에이전트 안내 메시지용 짧은 자산 식별자를 반환한다.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("자산#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule은 규칙 집합에 활성 규칙이 있는지 확인한다.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision은 후보 집합에 차단 후 허용 순서로 규칙을 적용한 판정 결과다.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 자산 식별자를 제외한 거부 이유. Allowed=true이면 빈 값
}

// EvaluateAssetGate는 작업 수준의 접근 허용 여부를 판정한다.
//  1. 활성 blockRules 중 하나와 일치하면 거부한다(차단 이유).
//  2. 그렇지 않고 활성 allowRules가 있지만 모두 불일치하면 거부한다(허용 범위 밖).
//  3. 그 외에는 허용한다.
//
// allowRules가 비었거나 활성 항목이 없으면 허용 목록을 사용하지 않고 모두 허용한다.
// 허용 규칙 미설정으로 모든 자산이 차단되는 것을 방지한다.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "작업 허용 목록 범위 밖이므로 테스트할 수 없습니다"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit는 차단 규칙 일치 또는 허용 범위 밖으로 거부된 자산을 나타낸다.
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 읽기 쉬운 이유
}

// Describe는 자산 정보와 이유를 합친 읽기 쉬운 설명을 반환한다.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules는 *DB의 같은 이름 메서드를 그대로 전달하여 AssetStore만 가진 호출자
// (예: 에이전트 도구)도 규칙을 읽을 수 있게 한다.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept는 ID로 자산을 불러와 차단 후 허용 순서로 각각 판정하고 거부된 자산을 반환한다.
// 차단 규칙 = 전역 ∪ 작업 수준 block, 허용 규칙 = 현재 작업의 allow.
// ID가 없으면 즉시 반환한다. 작업 범위로 차단이 약화되지 않도록 범위 필터 없는 전역 GetByIDs를 사용한다.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// 차단 규칙과 활성 허용 규칙이 모두 없으면 판정 없이 모두 허용한다.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
