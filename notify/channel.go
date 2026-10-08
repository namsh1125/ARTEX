package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel은 알림 채널 어댑터입니다. 여러 채널 설정이 같은 인스턴스를 동시에 재사용하므로
// 구현은 상태를 가지면 안 되며 자격 증명은 항상 cfg 매개변수로 받습니다.
type Channel interface {
	// Kind는 등록 키와 일치하는 채널 유형 식별자를 반환합니다.
	Kind() string
	// Validate는 설정 저장 시 필수 필드와 형식을 검증합니다. 오류가 설정자에게 직접 표시되므로
	// 막연한 설정 오류 대신 어떤 필드가 누락되었는지 설명해야 합니다.
	Validate(cfg map[string]any) error
	// Send는 메시지를 전송하고 실제 전달된 항목 수와 오류를 반환합니다.
	//
	// 플랫폼마다 길이 한도가 있어 요약 메시지에 전체 묶음이 들어가지 않으면 잘립니다.
	// 전체를 전달 완료로 표시하면 잘린 항목은 메시지에 없는데도 이력에는 성공으로 남아
	// 취약점 알림이 전송되지 않은 사실을 확인할 수 없습니다. kept를 반환하면
	// 호출자가 앞 kept개만 완료 처리하고 나머지는 다음 묶음으로 남길 수 있습니다.
	//
	// 오류는 전달 실패이며 *PermanentError는 재시도하지 않아야 함을 뜻합니다.
	// 실패 시 kept는 의미가 없으므로 무시해야 합니다.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin은 채널이 공식 권장하는 분당 전송 한도를 반환해 새 채널 인스턴스의
	// 기본 제한값으로 사용합니다. 0은 알려진 제한이 없다는 뜻입니다.
	DefaultRatePerMin() int
	// SecretKeys는 자격 증명에 해당하는 설정 키를 반환합니다. API 응답에서 값을 마스킹하며
	// 갱신 시 마스킹된 값이 들어오면 기존 DB 값을 유지합니다. 무엇이 자격 증명인지는
	// 구현만 알 수 있습니다(WeCom은 Webhook URL 전체, DingTalk은 secret만 해당).
	// 따라서 상위 계층이 추측하지 않고 채널이 직접 제공해야 합니다.
	SecretKeys() []string
	// DestinationKeys는 메시지 수신 위치를 결정하는 설정 키를 반환합니다.
	//
	// SecretKeys와 마찬가지로 보안에 관련됩니다. 대상 주소와 자격 증명은 별도 필드이므로
	// 주소만 바꾸고 자격 증명을 유지할 수 있으면 설정 수정자가 DB의 실제 자격 증명을
	// 자신의 서버로 보내도록 할 수 있어 마스킹의 의미가 사라집니다.
	// PrepareConfigUpdate를 참고하세요.
	DestinationKeys() []string
}

// registry는 채널 등록 목록입니다. init() 자동 등록 대신 명시적 리터럴을 사용해
// 전체 채널을 한곳에서 확인하고 추가 시 누락이 런타임 부수 효과 대신 컴파일 단계에서 드러나게 합니다.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get은 유형으로 채널 구현을 반환합니다.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind는 지원하는 채널 유형인지 반환합니다.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds는 지원 유형을 사전순으로 반환해 UI 드롭다운 표시를 안정적으로 유지합니다.
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError는 자격 증명 오류, 대상 거부, 잘못된 본문 등 재시도할 수 없는 실패입니다.
// 재시도는 일시적 네트워크 오류, 요청 제한, 상대 5xx에만 의미 있습니다. 영구 실패를 계속
// 백오프 재시도해도 성공하지 않으며 실제 오류가 반복 로그에 묻힙니다.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent는 err를 영구 실패로 표시하며 nil은 nil로 반환하므로
// return Permanent(someCheck())처럼 사용할 수 있습니다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent는 오류 체인에 영구 실패 표시가 있는지 반환합니다.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 설정 읽기 헬퍼 ----
//
// 채널 설정은 DB의 JSONB에서 encoding/json으로 역직렬화한 map[string]any이며
// 숫자는 float64, 배열은 []any입니다. 다음 헬퍼는 이 변환을 통일하고 UI에서 비워 두거나
// 포트를 문자열로 입력하는 등의 유형 차이를 허용합니다.

// cfgString은 문자열 설정을 읽고 폼 복사/붙여넣기에 붙기 쉬운 앞뒤 공백을 제거합니다.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt는 정수 설정을 읽으며 JSON 기본 float64와 문자열을 지원합니다.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool은 불리언 설정을 읽으며 문자열 true/1도 지원합니다.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings는 문자열 배열을 읽고 공백을 제거하며 빈 문자열을 버립니다.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 값이 하나인 폼 제출을 위해 단일 문자열도 허용합니다.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap은 사용자 정의 HTTP 헤더 등의 문자열 맵을 읽고 키/값 공백과 빈 키를 제거합니다.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
