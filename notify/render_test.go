package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// 핵심 불변 조건: WeCom은 바이트 기준이며 한글/한자는 보통 3바이트이므로
	// 바이트로 단순 자르면 문자가 반으로 잘려 잘못된 UTF-8로 거부될 수 있습니다.
	// 길이가 서로소인 여러 다국어 혼합 입력으로 가능한 절단 지점을 모두 검사합니다.
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4바이트 이모지는 잘못 자르면 더 명확하게 드러남
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("입력 %q max=%d: 유효하지 않은 UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("입력 %q max=%d: 결과 %d바이트가 한도 초과", in, max, len(got))
			}
			// 잘리지 않았으면 내용을 바꾸면 안 됩니다.
			if len(in) <= max && got != in {
				t.Fatalf("입력 %q max=%d: 한도 이내인데 내용 변경 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0은 무제한이어야 합니다")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0은 무제한이어야 합니다")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max가 말줄임표보다 작을 때 추가 때문에 한도를 넘으면 안 됩니다.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 결과 %q 길이 %d가 한도 초과", got, len(got))
	}
	// 일반적인 잘림에는 말줄임표가 있어야 합니다.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("말줄임표 예상, 실제 %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// Telegram은 문자 수 기준이므로 TruncateBytes와의 차이를 유지해야 합니다.
	// 바이트 기준이면 한글/한자 메시지가 약 1/3로 줄어듭니다.
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("5자 예상, 실제 %d자(%q)", n, got)
	}
	// 같은 문자열을 바이트로 제한하면 확실히 짧아야 합니다.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("바이트/문자 기준 결과의 문자 수가 같으면 안 됩니다")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("첫째 줄\n\n둘째 줄\t탭 포함   여러 공백", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("모든 공백을 접어야 함, 실제 %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("연속 공백이 없어야 함, 실제 %q", got)
	}
	// 잘린 뒤에도 읽을 수 있고 유효해야 합니다.
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("4자 예상, 실제 %d(%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML을 직접 자르면 불완전한 태그가 남아 플랫폼이 전체를 거부할 수 있습니다.
	s := `<b>제목</b>본문본문본문<a href="https://example.com/very/long/path">상세 보기</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 결과 %d자가 한도 초과", max, n)
		}
		// 끝에 > 없이 <만 있는 불완전한 태그가 없어야 합니다.
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 끝 태그가 잘림 -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("자산이 없으면 빈 문자열 예상, 실제 %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a、b" {
		t.Fatalf("한도 이내면 전체 표시 예상, 실제 %q", got)
	}
	// 초과 시 전체 자산 수를 알려 생략량을 알 수 있게 합니다.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "등 5개") {
		t.Fatalf("총수 5를 표시해야 함, 실제 %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("빈 등급은 순번 0이므로 모든 한도에 차단되어야 합니다")
	}
	if !AtLeast("critical", "") {
		t.Fatal("빈 한도는 허용해야 합니다")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("상태 매핑 오류, 실제 %q", got)
	}
	// 알 수 없는 상태는 이름을 지어내지 않고 그대로 반환합니다.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("알 수 없는 상태는 그대로 반환해야 함, 실제 %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity는 불완전한 태그뿐 아니라 잘린 HTML 엔터티도
// 피해야 한다는 검토 결과를 검증합니다.
//
// &amp;가 &amp로 잘리면 엔터티 파서가 전체 메시지를 거부할 수 있으며
// 긴 요약에서 흔히 생길 수 있어 방지해야 합니다.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 끝에 &만 있고 대응 ;가 없는 엔터티 조각이 없어야 합니다.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 끝에 엔터티 조각 %q가 남음", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 잘못된 엔터티 발생", max)
		}
	}
}
