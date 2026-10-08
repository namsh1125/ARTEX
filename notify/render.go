package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes는 문자를 자르지 않고 유효한 UTF-8을 유지하며 s를 최대 max바이트로 제한합니다.
//
// WeCom markdown은 문자 수가 아닌 4096바이트 한도이며 한글/한자는 보통 3바이트입니다.
// 바이트로 단순 슬라이스하면 문자가 중간에 잘려 잘못된 UTF-8이 되고
// 플랫폼이 전체를 거부하거나 깨진 문자로 표시합니다. 예산 위치에서 가장 가까운
// rune 시작 바이트까지 뒤로 이동합니다(utf8.RuneStart로 연속 바이트 0b10xxxxxx 판별).
//
// max<=0은 무제한입니다. 자른 뒤 말줄임표를 붙이되 max가 너무 작으면 생략합니다.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max가 말줄임표보다 짧으면 결과가 한도를 넘지 않도록 말줄임표 없이 자릅니다.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine은 모든 공백을 접어 여러 줄을 한 줄로 만든 뒤 문자 수로 자릅니다.
// IM 제목에 사용하며 요약의 줄바꿈 때문에 표/제목 배치가 깨지는 것을 방지합니다.
// max<=0은 길이 무제한입니다.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes는 바이트가 아닌 문자 수를 최대 max로 제한하고 초과 시 말줄임표를 붙입니다.
// max<=0은 무제한입니다.
//
// 플랫폼 기준에 따라 TruncateBytes와 구분합니다. WeCom은 바이트, Telegram은 문자 수입니다.
// 기준을 잘못 써도 오류가 나지 않고 메시지만 지나치게 짧아집니다. 한글/한자는 보통 3바이트라
// 4096바이트면 약 1365자밖에 안 되므로 두 함수를 유지하고 채널에 맞춰 선택합니다.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML은 문자 수로 HTML 조각을 자르되 불완전한 태그를 만들지 않습니다.
//
// 단순히 자르면 <a href="htt 같은 태그가 남아 전체 메시지가 거부되거나
// 뒤의 본문이 속성 값으로 해석될 수 있습니다. 먼저 문자 수로 자르고
// 마지막에 닫히지 않은 <가 있으면 그 앞까지 되돌립니다.
//
// 닫는 태그를 직접 보충하지 않습니다. Telegram 파서는 자동으로 닫으며 직접 구현하려면
// 속성 따옴표, 주석, 자체 닫힘 태그까지 처리해야 해 비용이 이점보다 큽니다.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 마지막 < 뒤에 >가 없으면 불완전한 태그이므로 < 앞까지 되돌립니다.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// &amp;가 &amp로 잘리는 등 불완전한 HTML 엔터티도 되돌립니다.
	// 엔터티만 인식하는 파서는 이런 조각 때문에 전체 메시지를 거부할 수 있습니다.
	// 흔한 길이 초과 요약 때문에 알림 전체를 잃지 않도록 합니다.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount는 예산 안에 온전히 담을 수 있는 항목 수를 계산합니다.
//
// 전체 렌더링 후 자르면 뒤 항목이 사라져도 전송 이력에는 완료로 표시되어
// 메시지나 이력 어디에서도 누락을 확인할 수 없습니다.
// 항목 단위로 채우면 못 들어간 것은 DB에 남아 다음 묶음이 되고
// kept는 이 메시지에 실제 전달된 항목 수가 됩니다.
//
// 매개변수: maxSize<=0은 무제한, reserve는 머리말/꼬리말 예약량,
// size는 플랫폼별 측정 방식(WeCom/DingTalk은 바이트, Telegram은 문자 수)입니다.
// 기준을 틀리면 오류 없이 다국어 메시지가 한도보다 훨씬 짧아집니다.
// render는 idx 항목을 실제 텍스트로 렌더링합니다. 내용별 길이가 달라 추정할 수 없습니다.
//
// 항목이 있으면 최소 1을 반환합니다. 한 항목이 매우 길어도 호출자의 최종 잘림으로
// 전송해야 긴 취약점 하나가 전체 묶음을 영구 정체시키지 않습니다.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize는 측정 기준을 명확히 드러내는 이름입니다. 익명 func(s string) int보다
// 호출 지점에서 어떤 기준인지 알아보기 쉽습니다.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine은 자산 목록을 한 줄로 표시하며 limit 초과 시 나머지는 생략하고 총수를 표시합니다.
// 취약점 하나에 수십 자산이 연결될 수 있어 전체 표시는 메시지를 가득 채울 수 있습니다.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, "、")
	}
	return strings.Join(assets[:limit], ", ") + " 등 " + itoa(len(assets)) + "개"
}

// itoa는 표시 텍스트 조합용 strconv.Itoa의 짧은 별칭으로 반복 import를 줄입니다.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
