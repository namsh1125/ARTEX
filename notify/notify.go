// Package notify는 취약점 발견의 IM/메일 알림 채널 어댑터 계층입니다.
//
// 표준 라이브러리만 의존하는 말단 패키지로 DB/server를 알지 못합니다. 채널 설정은
// notification_channels.config JSONB에 해당하는 map[string]any,
// 알림 내용은 Message로 받습니다. 따라서 서명, UTF-8 잘림, 필터 같은 오류 가능 부분을
// PostgreSQL 없이 단위 테스트하고 server는 조정만 담당할 수 있습니다.
//
// 동시성 계약: Channel 구현은 상태를 가지면 안 됩니다. 여러 채널 설정이나
// 동일 채널의 여러 봇이 같은 인스턴스를 동시에 재사용하므로 자격 증명은 cfg로 받고
// webhook URL 등을 구현 필드에 캐시하면 안 됩니다.
package notify

// 채널 유형 식별자는 notification_channels.kind의 허용 값이며 server에서 검증합니다.
// findings.status처럼 DB CHECK를 쓰지 않아 향후 채널 추가가 쉽습니다.
const (
	KindDingTalk = "dingtalk" // DingTalk 사용자 정의 봇
	KindFeishu   = "feishu"   // Feishu/Lark 사용자 정의 봇
	KindWeCom    = "wecom"    // WeCom 그룹 봇
	KindWebhook  = "webhook"  // 일반 Webhook: 메서드/헤더/JSON 템플릿 사용자 지정
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP 메일
)

// notification_events.kind에 대응하는 이벤트 유형입니다.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind는 config의 kind가 비어 있을 때 기본값입니다.
const InitKind = KindDingTalk

// severityRank는 비교 가능한 등급 순번을 반환합니다. 알 수 없으면 0이므로 비어 있지 않은
// min_severity가 차단해 불확실한 오탐 알림의 범람을 막습니다.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank는 등급 순번을 반환하며 알 수 없으면 0입니다.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel은 제목/카드 색상에 사용할 이모지 포함 한국어 등급 이름입니다.
// 알 수 없는 등급은 지어내지 않고 그대로 반환합니다.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 치명적"
	case "high":
		return "🟠 높음"
	case "medium":
		return "🟡 보통"
	case "low":
		return "🔵 낮음"
	default:
		return severity
	}
}

// StatusLabel은 상태 변경 메시지에 사용할 한국어 처리 상태를 반환합니다.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "처리 대기"
	case "in_progress":
		return "처리 중"
	case "confirmed":
		return "확인됨"
	case "resolved":
		return "처리됨"
	case "fixed":
		return "수정됨"
	case "false_positive":
		return "오탐"
	case "ignored":
		return "무시"
	case "duplicate":
		return "중복"
	case "risk_accepted":
		return "위험 수용"
	default:
		return status
	}
}

// AtLeast는 severity가 min 이상인지 판정하며 min이 비면 모두 통과합니다.
// 알 수 없는 severity는 0이므로 비어 있지 않은 min이면 거부합니다(severityRank 참고).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
