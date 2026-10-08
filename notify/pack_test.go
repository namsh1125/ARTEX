package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 채널 길이 한도를 넘는 요약을 항목 단위로 묶는 처리를 검증합니다.
// 실제 포함하지 못한 수를 알려 호출자가 전달된 항목만 완료 처리해야 합니다.
//
// 전체 렌더링 후 잘라서 전부 완료 처리하면 뒤 항목이 사라져도 이력은 성공으로 남아
// 취약점 알림 누락을 어디에서도 확인할 수 없습니다.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 한국어 항목 200개는 WeCom 4096바이트를 충분히 초과합니다.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("본문 %d바이트가 한도 %d 초과", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("본문이 유효한 UTF-8이 아닙니다")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 포함해야 함(0 < kept < %d), 실제 %d", len(m.Items), kept)
	}
	// 머리말에서 이번 포함 수와 남은 수를 정확히 알려
	// 독자가 표시된 숫자를 전체로 오해하지 않게 합니다.
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지에 계속") {
		t.Fatalf("머리말에 미포함 항목 수를 안내해야 합니다:\n%s", body[:minInt(400, len(body))])
	}
	// 앞 kept개만 포함해야 합니다.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "취약점"+itoa(i+1)) {
			t.Fatalf("%d번째 항목이 이번 메시지에 있어야 합니다:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "취약점"+itoa(kept+1)) {
		t.Fatalf("%d번째 항목은 다음 묶음이므로 나오면 안 됩니다", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 무제한
	if kept != len(m.Items) {
		t.Fatalf("길이 무제한이면 모두 유지해야 함, kept=%d", kept)
	}
	if strings.Contains(body, "나머지") {
		t.Fatalf("잘리지 않았으면 잘림 안내가 없어야 합니다:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 예산이 한 항목도 담지 못해도 최종 잘림으로 하나는 보내야 합니다.
	// 그렇지 않으면 긴 취약점 하나 때문에 매번 아무것도 보내지 못하고 묶음이 영구 정체됩니다.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("최소 1개 유지 예상, 실제 %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("단일 메시지는 1개 전달로 보고해야 함, 실제 %d", kept)
	}
	// 빈 메시지에는 전달할 항목이 없습니다.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("빈 메시지는 0개로 보고해야 함, 실제 %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram은 문자 수 한도이며 바이트를 쓰면 한글/한자 메시지가 약 1/3로 줄어듭니다.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("본문 %d자가 한도 %d 초과", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 포함해야 함, 실제 %d", kept)
	}
	if !strings.Contains(text, "다음 메시지에 계속") {
		t.Fatalf("미포함 항목이 있음을 안내해야 합니다:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("카드에 일부만 포함해야 함, 실제 %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// 두 채널은 본문을 자르지 않아 전체 묶음을 전달 완료로 처리합니다.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("선행 조건 불충족")
	}
	// markdownBody(0)의 반환값으로 무제한일 때 전체 유지됨을 간접 확인합니다.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("길이 무제한이면 전부 사용해야 함, 실제 %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent는 신뢰할 수 없는 내용이 메시지 구조를 바꾸지 못하는지 검증합니다.
// 제목/요약은 대상 응답을 읽은 모델 출력이고 자산 이름은 대상 URL에서 옵니다.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 반드시 포함할 이스케이프된 형태
		wrong []string // 포함하면 안 되는 원본 형태
	}{
		{
			name: "제목의 줄바꿈과 외부 링크",
			item: Item{
				Severity: "high",
				Name:     "로그인 SQL 인젝션\n[긴급: 계정 확인](http://attacker.tld)",
			},
			// 줄바꿈을 접어 목록/인용 위조를 막고
			// 대괄호/괄호를 이스케이프해 클릭 가능한 외부 링크를 막습니다.
			must:  []string{`\[긴급: 계정 확인\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[긴급", "\n\n[긴급"},
		},
		{
			name: "제목의 이미지 비콘",
			item: Item{
				Severity: "high",
				Name:     "취약점 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "자산 이름의 강조와 인용",
			item: Item{
				Severity: "high",
				Name:     "일반 제목",
				Assets:   []string{"a.com/*주입*>인용"},
			},
			must:  []string{`\*주입\*`, `\>`},
			wrong: []string{"*주입*"},
		},
		{
			name: "요약의 백틱과 세로줄",
			item: Item{
				Severity: "high",
				Name:     "제목",
				Summary:  "`code` | 표",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 단일 모드 writeItem은 세 Markdown 채널이 공유하는 렌더링 경로입니다.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("이스케이프된 형태 %q 누락:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("구조/외부 링크 주입이 가능한 원본 형태 %q 포함:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst는 역슬래시를 먼저 처리하는 순서를 검증합니다.
// 뒤에 추가한 역슬래시까지 다시 처리하면 이중 역슬래시가 출력됩니다.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("이스케이프 순서 오류, 실제 %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes는 공유 제목 함수의 Markdown 이스케이프가
// Telegram HTML로 새어 나오지 않는지 검증합니다. 이전에는 괄호 앞의
// 역슬래시가 그대로 표시되는 회귀가 있었습니다.
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *중점*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 본문에 Markdown 역슬래시가 있습니다:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
