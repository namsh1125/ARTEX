package notify

import (
	"fmt"
	"strings"
)

// DingTalk과 WeCom 등 Markdown 채널이 공유하는 메시지 렌더링입니다.
// Feishu는 카드 JSON, Telegram과 메일은 HTML을 각 어댑터에서 렌더링합니다.

// maxAssetsShown은 메시지에 나열할 최대 자산 수입니다. 취약점 하나에 수십 자산이 있으면
// 전체 표시가 메시지를 채우고 정보 가치도 낮아 제한합니다.
const maxAssetsShown = 3

// maxSummaryRunes는 요약의 최대 문자 수입니다. IM은 상세 확인을 유도하는 알림이며
// 전체 보고서는 플랫폼에서 확인합니다.
const maxSummaryRunes = 120

// markdownReservedBytes는 머리말(요약 행+등급 분포+잘림 안내)과
// 꼬리말(플랫폼 링크)에 예약할 바이트 수입니다. 항목을 채울 때 예산에서 빼서
// 묶음의 정체와 생략된 수를 알려 주는 머리말/꼬리말이 잘리지 않게 합니다.
const markdownReservedBytes = 320

// markdownEscape는 Markdown 특수 문자를 이스케이프합니다.
//
// 취약점 제목, 요약, 유형, 자산 이름은 모두 신뢰할 수 없는 출처입니다.
// 모델 출력은 테스트 대상 응답을 바탕으로 하고 자산 URL의 쿼리는 대상이 제어할 수 있습니다.
// 이스케이프하지 않으면 다음 제목이
//
//	로그인 SQL 인젝션\n[긴급: 계정 확인](http://attacker.tld)
//
// DingTalk/Feishu에서 클릭 가능한 외부 링크가 됩니다.
// ![](http://attacker.tld/beacon)은 렌더링 시 클라이언트가 요청해
// 취약점 열람 여부와 독자의 IP를 노출합니다. 악의가 없어도 굵은 글씨나
// 인용 블록 삽입이 중요한 취약점을 접힘 영역 밖으로 밀어낼 수 있습니다.
//
// 제목/링크/강조/목록/인용/취소선 등 구조나 클릭 요소를 만드는 문자를 처리합니다.
// 역슬래시는 나중에 추가한 역슬래시를 다시 처리하지 않도록 가장 먼저 이스케이프합니다.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText는 신뢰할 수 없는 텍스트를 한 줄로 만들고 이스케이프합니다.
// 줄바꿈 자체로 목록/인용을 위조할 수 있으므로 문자 이스케이프 외에
// 한 줄로 만드는 처리도 필요합니다.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle은 IM 제목/카드 제목의 이스케이프하지 않은 원문을 반환합니다.
//
// Markdown 본문, Telegram HTML, Feishu plain_text,
// Webhook JSON 및 메일 제목이 공유하므로 여기서는 이스케이프하지 않습니다.
// 맥락마다 규칙이 달라 Markdown 이스케이프를 HTML/JSON에 넣으면
// 보이는 역슬래시나 오염된 데이터가 됩니다. 각 출력단 writeItem / feishuItemLines /
// telegramEscape에서 처리해야 합니다. 과거 공통 함수에서 처리했다가 Telegram에
// 역슬래시가 그대로 표시된 적이 있습니다.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("취약점 요약 · 총 %d개", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "취약점 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody는 본문과 실제 포함한 항목 수를 반환합니다.
//
// kept는 실제 전달된 수이며 호출자는 앞 kept개만 완료 처리해야 합니다.
// 길이 한도로 제외된 항목은 성공 처리하지 않고 다음 묶음으로 남깁니다.
// 메시지는 잘렸는데 이력은 전부 성공이면 어디에서도
// 뒤 항목의 미전송을 알 수 없는 조용한 누락이 발생합니다.
//
// maxBytes<=0은 무제한입니다.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 단일 항목이 길어도 최종 잘림으로 전송합니다. 일부 정보라도
		// 보내는 편이 아무것도 보내지 않는 것보다 낫습니다.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 전체 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro는 시간 구간, 개수, 등급 분포를 렌더링합니다.
// 수신자가 플랫폼에 들어가지 않아도 즉시 처리할 필요가 있는지 판단할 수 있습니다.
//
// items는 실제 포함한 항목, total은 전체 묶음 수입니다. 다르면 다음 메시지에 남은 수를
// 명시해야 독자가 머리말 숫자를 전체로 오해하거나
// 아직 전송되지 않은 항목을 놓치지 않습니다.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d분 새 취약점 %d개**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**새 취약점 %d개**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(이 메시지에는 앞 %d개 표시, 나머지 %d개는 다음 메시지에 계속)", len(items), extra)
	}
	// 이 메시지에 실제 포함된 항목만 등급별로 집계해
	// 치명적 3개라는 표시와 아래에서 셀 수 있는 수가 일치하게 합니다.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem은 취약점 항목 하나를 렌더링합니다.
//
// prefix는 요약 목록 번호, single=true는 요약/상세 링크를 포함한 전체 형식입니다.
// 요약 목록은 50개가 긴 문서가 되지 않도록 한 줄씩만 표시합니다.
//
// 외부 내용(제목/유형/자산/요약)은 모두 markdownText로
// 한 줄 처리 및 이스케이프합니다. 상세 링크는 관리자 public_base_url로 만들고
// 클릭 가능해야 하므로 그대로 출력합니다.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 요약 모드: 한 줄로 표시하며 압축한 자산과 요약을 뒤에 붙입니다.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**요약**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[상세 보기](%s)\n", it.DetailURL)
	}
}
