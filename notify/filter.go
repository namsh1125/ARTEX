package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter는 notification_channels.filter JSONB의 계약이며 채널 인스턴스의 필터 조건입니다.
// 모든 필드는 선택 사항이고 기본은 필터 없음입니다. 잘못된 설정에도 이를 적용합니다(ParseFilter 참조).
type Filter struct {
	// MinSeverity는 최소 등급(low/medium/high/critical)이며 비면 제한이 없습니다.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs가 비면 제한 없고 값이 있으면 이벤트와 교집합이 있어야 합니다.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude가 비면 모두 수신하며 값이 있으면 vulnclass가 키워드 하나 이상에 일치해야 합니다.
	// VulnClassExclude의 키워드에 하나라도 일치하면 제외하며 포함보다 우선합니다.
	// 대소문자 무시 부분 문자열 검색으로 잘못된 정규식 때문에 채널이 조용히 실패하는 문제를 피합니다.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange는 취약점 상태 변경 수신 여부이며 realtime 모드에서만 의미 있습니다.
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter는 채널 필터 설정을 해석합니다.
//
// 오류를 반환하지 않습니다. 잘못된 설정은 제로 값 Filter로 대체해
// 필터 없이 모두 일치하게 합니다. 취약점 알림은 고위험 항목을 조용히 빠뜨리는 것보다
// 하나 더 보내는 편이 낫기 때문입니다. 해석 실패로 전송을 막으면 사용자는 설정이 된 것처럼
// 보이지만 아무것도 받지 못하게 됩니다.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 해석 실패 시 f는 제로 값으로 유지되어 필터링하지 않습니다.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity는 유효한 최소 등급인지 확인하며 빈 문자열은 제한 없음입니다.
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate는 저장 시 허용 값이 제한된 필드를 검증합니다.
//
// Match는 알 수 없는 등급을 rank >= 0으로 항상 참 처리하므로 저장 단계에서 막아야 합니다.
// min_severity를 hgih처럼 오타 내면 필터가 조용히 무효화되어
// 전체 알림을 보냅니다. 누락보다 추가 전송을 선호하는 설계와는 맞지만
// 사용자가 등급별 전송이라 생각하는 동안 모든 취약점이 채널로 전달되고
// 설정 오류 표시도 없으므로 이런 조용한 동작 변경은 입력 시 차단해야 합니다.
//
// Validate는 쓰기 경로에만 적용합니다. 읽기는 ParseFilter의 관대한 해석을 유지해
// 과거 잘못된 값 때문에 채널 전체를 읽지 못하는 일이 없게 합니다.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("최소 등급 %q가 유효하지 않습니다. low / medium / high / critical 또는 제한 없음은 빈 값", f.MinSeverity)
	}
	return nil
}

// Match는 이벤트를 이 필터의 채널에 전달할지 판정합니다.
//
// ParseFilter와 같은 이유로 오류를 반환하지 않고 내부 이상은 일치로 처리합니다.
// 순서: 이벤트 유형 → 최소 등급 → 작업/자산 범위 → 취약점 유형 키워드.
func Match(f Filter, s Snapshot) bool {
	// 상태 변경은 명시적으로 켠 채널만 받습니다. 대부분 새 취약점 발견 알림을 기대하므로
	// 모든 상태 전환을 추적하는 알림은 기본적으로 끕니다.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 제외 우선: 포함 목록에도 일치하더라도 제외 키워드에 맞으면 제외합니다.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 양쪽 모두 사람이 선택한 수십 개 수준의 작은 집합이므로
	// map 생성보다 선형 순회가 적합합니다.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold는 s에 keywords 중 하나가 포함되는지 대소문자 무시로 확인합니다.
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
