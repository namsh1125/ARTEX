package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("마스킹 값에 전체 인증 정보 노출: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("마스킹 값에 주소 본문이 노출되면 안 됨: %q", got)
	}
	// 사용자가 봇을 식별할 수 있도록 마지막 6자를 유지한다.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("식별 힌트로 마지막 6자를 유지해야 함: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("IsMasked가 마스킹 값을 식별해야 함: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 짧은 인증 정보의 마지막 6자를 표시하면 전체가 노출될 수 있다.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("길이 %d 인증 정보는 끝부분 힌트를 주면 안 됨, 실제 %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("마스킹 값에 원래 값 포함: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s는 마스킹해야 함, 실제 %q", k, s)
		}
	}
	// UI가 표시할 수 있도록 인증 정보가 아닌 필드는 그대로 유지해야 한다.
	if masked["port"] != float64(587) {
		t.Errorf("인증 정보가 아닌 port 필드를 바꾸면 안 됨: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// 알 수 없는 채널 유형은 인증 정보가 있을 수 있는 원본 대신 빈 설정을 UI에 표시한다.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("알 수 없는 채널 유형은 빈 설정을 반환해야 함, 실제 %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// 마스킹은 표시 계층의 동작이며 DB의 실제 값을 수정하면 안 된다.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig가 입력을 수정하여 실제 인증 정보가 마스킹 값으로 덮어써질 수 있음")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// 사용자는 method만 수정했고 브라우저는 마스킹 값과 새 method를 제출한다.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("마스킹 필드는 DB의 원래 값을 유지해야 함, 실제 %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("수정한 필드가 적용되어야 함, 실제 %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("빈 문자열은 필드를 지워야 함, 실제 %v", got)
	}
	// 언급하지 않은 필드는 유지한다(부분 업데이트 의미).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("언급하지 않은 필드는 유지해야 함, 실제 %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("언급하지 않은 필드는 유지해야 함, 실제 %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("언급한 필드는 갱신해야 함, 실제 %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap은 핵심 보안 불변 조건을 검증한다.
// 대상 주소를 바꿀 때 기존 인증 정보를 새 주소로 가져가면 안 된다.
//
// 주소만 바꾸고 인증 정보를 생략하는 실제 공격 형태의 입력을 사용한다.
// 정상적인 방어 로직 입력만 테스트하면 방어가 작동하지 않아도 모두 통과할 수 있다.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing은 누락되었다고 명시되어야 하는 인증 정보 키다.
		wantMissing string
	}{
		{
			name: "일반 Webhook 주소 변경 시 Authorization 헤더 재사용 시도",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram base_url 변경으로 자체 엔드포인트에 Bot Token 전송 시도",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "메일 SMTP 호스트 변경으로 비밀번호 전송 시도",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "메일 TLS 해제 시에도 비밀번호 재지정 필요",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// 마스킹 값은 기존 인증 정보 재사용을 의미하므로 주소 변경 시에도 거부해야 한다.
			name:        "마스킹 인증 정보와 새 주소 제출",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk Webhook 변경 시 서명 키 재사용 시도",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("주소 변경 시 인증 정보를 재지정하지 않으면 거부해야 함; 실제 설정 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("API가 해결 방법을 안내하도록 전용 오류 타입을 반환해야 함, 실제 %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("누락된 인증 정보 키 %q를 명시해야 함, 실제 %v", tc.wantMissing, target.Missing)
			}
			// 오류 메시지가 해결 방법을 안내해야 한다.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("오류 메시지에 %q가 있어야 함: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits는 정상 편집이 잘못 차단되지 않는지 검증한다.
// 너무 불편한 방어 기능은 우회되거나 삭제될 수 있다.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "이름만 수정(설정 그대로 제출)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "요청 메서드만 수정하고 주소와 인증 정보 유지",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "주소 변경과 동시에 새 인증 정보 지정",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "주소 변경과 동시에 인증 정보가 필요 없음을 명시",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram chat_id 변경(대상 주소 아님)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "메일 수신자 변경(대상 주소 아님)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("정상 편집이 잘못 차단됨: %v", err)
			}
			if merged == nil {
				t.Fatal("병합 결과를 반환해야 함")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance는 오판하기 쉬운 세부 사항을 검증한다.
// 프런트엔드 포트는 JSON number(float64)이고 DB에서도 float64로 읽지만
// 두 값의 타입은 다를 수 있다(int와 float64 등). == 비교는 같은 값을 변경으로 오인하여
// 이름만 바꾼 사용자에게 비밀번호 재입력을 요구하고 방어 기능에 대한 신뢰를 떨어뜨릴 수 있다.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 같은 포트를 int로 제출한다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("포트 값이 같고 타입만 다르면 주소 변경으로 판정하면 안 됨: %v", err)
	}
	// 실제로 포트를 변경하면 차단해야 한다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("포트 변경을 차단해야 함")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination은 비워 둘 수 있는
// 대상 필드 경로를 검증한다. Telegram base_url이 비어 있으면 공식 API 주소를 사용한다.
//
// 이전에는 두 번째 저장부터 계속 실패했다.
//
//	생성 시 base_url:"" 저장(생성 경로는 MergeConfig 없이 프런트엔드 config를 직접 저장)
//	→ 첫 저장에서 MergeConfig가 빈 문자열을 명시적 삭제로 처리하여 키 삭제
//	→ 두 번째 저장에서 incoming은 ""지만 stored에 키가 없어 주소 변경으로 판정
//	→ bot_token이 마스킹 값이므로 400 오류로 대상 변경 시 인증 정보 재입력 요구
//
// 사용자가 아무것도 바꾸지 않아도 Bot Token을 다시 붙여 넣기 전에는 저장할 수 없었다.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// 프런트엔드 buildConfig()는 모든 필드 값을 제출한다. 인증 정보는 마스킹 값을,
	// 빈 입력란은 빈 문자열을 제출한다. 변경한 키만 보내지 않고 이 출력을 그대로 재현한다.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 첫 저장: 채널 이름만 바꾸고 config를 그대로 제출한다.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("첫 저장이 잘못 차단됨: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("전제가 바뀜: MergeConfig가 빈 문자열을 삭제해야 함. 키가 사라진 다음 단계를 검증하는 사례임")
	}

	// 두 번째 저장: 이전과 동일한 내용을 제출하며 사용자는 아무것도 바꾸지 않았다.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("두 번째 저장이 잘못 차단됨(변경 없음): %v", err)
	}
	// 세 번째에도 안정적으로 저장할 수 있는지 확인한다.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("세 번째 저장이 잘못 차단됨: %v", err)
	}
	// 빈 문자열 처리로 인증 정보까지 삭제하지 않고 계속 유지해야 한다.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token은 원래 값을 유지해야 함, 실제 %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges는 이전 사례와 짝을 이루는 검증이다.
// 빈 문자열과 키 없음을 동등하게 취급하더라도 실제 주소 변경은 허용하면 안 된다.
// 두 방향 모두 실제 인증 정보 외부 전송 경로다. Telegram Bot Token은 URL 경로에 있으므로
// base_url 변경은 새 주소로 Token을 보내는 것과 같다.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 첫 방향: 빈 값(공식 주소)에서 자체 주소로 변경.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("공식 주소에서 자체 주소로 변경하면 Token 재입력을 요구해야 함")
	}

	// 반대 방향: 자체 주소를 지우고 공식 API로 돌아가는 것도 주소 변경이다.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("자체 주소 삭제(공식 API 복귀)도 주소 변경이므로 Token 재입력을 요구해야 함")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys와 마찬가지로 대상 키를 선언하지 않으면 PrepareConfigUpdate가 보호할 수 없다.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("채널 %s에 대상 키 선언이 없어 주소 변경 시 인증 정보 유출을 방어할 수 없음", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s에 인증 정보 키 선언이 없음", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// 컴파일러가 모든 채널의 SecretKeys 구현을 강제하지만 마스킹 선언이 빈 채널이 없는지도
	// 확인한다. 빈 슬라이스를 반환하면 인증 정보가 브라우저에 평문으로 표시된다.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("채널 %s의 마스킹 기대값이 테스트에 등록되지 않음", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s에 인증 정보 필드 선언이 없어 설정이 평문으로 표시됨", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer는 감사에서 발견한 허점을 검증한다.
// webhook.headers 객체처럼 문자열이 아닌 구조 안에 마스킹 센티널을 넣으면
// MergeConfig가 접두사 있는 문자열만 마스킹으로 인식하여 "__masked__" 리터럴을
// 실제 헤더 값으로 저장하고 이후 인증이 오류 없이 조용히 실패했다.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// 객체 내부에 마스킹 센티널을 포함한다.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("구조 내부의 마스킹 센티널을 거부해야 함(그렇지 않으면 리터럴이 DB에 저장됨)")
	}
	// 실제 새 값을 담은 객체 전체 제출은 정상적으로 허용한다.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("정상적인 새 요청 헤더 제출을 차단하면 안 됨: %v", err)
	}
}
