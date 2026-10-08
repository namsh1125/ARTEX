package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 잘못된 JSON, 빈 입력, 필드 타입 오류는 모두 영값 Filter로 처리해야 한다.
	// 즉 필터링하지 않는다. 누락보다 추가 전송을 택한다는 원칙을 보장한다.
	// 오류나 부분 해석으로 바꾸면 설정 한 글자 오류로 모든 고위험 알림이 조용히 사라질 수 있다.
	cases := []struct {
		name string
		raw  string
	}{
		{"빈 입력", ""},
		{"잘못된 JSON", `{not json`},
		{"잘린 JSON", `{"min_severity":`},
		{"타입 불일치", `{"min_severity": 123, "task_ids": "abc"}`},
		{"최상위 배열", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("잘못된 설정은 영값 Filter로 처리해야 함, 실제 %+v", f)
			}
			// 영값 Filter는 모든 이벤트와 일치해야 한다.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("영값 Filter는 모든 이벤트와 일치해야 함")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 알 수 없는 수준의 순서는 0이며 비어 있지 않은 모든 임계값이 차단해야 한다(불확실하면 전송하지 않음).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 기대 %v, 실제 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL 인젝션",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"빈 범위는 제한 없음", Filter{}, true},
		{"작업 일치", Filter{TaskIDs: []int64{7}}, true},
		{"작업 불일치", Filter{TaskIDs: []int64{8}}, false},
		{"여러 작업 중 일치", Filter{TaskIDs: []int64{8, 7}}, true},
		{"자산 교집합 있음", Filter{AssetIDs: []int64{20, 99}}, true},
		{"자산 교집합 없음", Filter{AssetIDs: []int64{99}}, false},
		{"작업과 자산 모두 일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"작업 일치, 자산 불일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("기대 %v, 실제 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"빈 include는 모두 허용", Filter{}, "임의 유형", true},
		{"include 일치", Filter{VulnClassInclude: []string{"SQL"}}, "SQL 인젝션", true},
		{"include 불일치", Filter{VulnClassInclude: []string{"명령 실행"}}, "SQL 인젝션", false},
		{"include 여러 단어 중 하나 일치", Filter{VulnClassInclude: []string{"명령 실행", "SQL"}}, "SQL 인젝션", true},
		{"대소문자 구분 없음", Filter{VulnClassInclude: []string{"sql"}}, "SQL 인젝션", true},
		{"exclude 일치 시 제외", Filter{VulnClassExclude: []string{"정보 노출"}}, "정보 노출", false},
		{"exclude 불일치 시 허용", Filter{VulnClassExclude: []string{"정보 노출"}}, "SQL 인젝션", true},
		// 제외는 포함보다 우선한다. 모두 일치하면 제외해야 한다.
		{"제외가 포함보다 우선", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"인젝션"},
		}, "SQL 인젝션", false},
		// 공백뿐인 키워드는 무시해야 한다. 그렇지 않으면 공백이 있는 모든 문자열과 일치한다.
		{"공백 키워드 무시", Filter{VulnClassInclude: []string{"", "  "}}, "SQL 인젝션", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("기대 %v, 실제 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 기본값은 꺼짐이다. 일반적인 취약점 알림은 상태 변경 기록이 아닌 새 취약점 발견을 의미한다.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("상태 변경 이벤트는 활성화하지 않으면 건너뛰어야 함")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change 활성화 후 상태 변경 이벤트와 일치해야 함")
	}
	// 생성 이벤트는 on_status_change의 영향을 받지 않는다.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("생성 이벤트는 on_status_change에 의존하면 안 됨")
	}
}
