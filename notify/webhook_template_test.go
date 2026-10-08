package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 일반 Webhook 템플릿의 기능 경계를 검증합니다.
//
// 사용자 문자열을 코드처럼 평가하는 유일한 곳이므로 가능한 기능과
// 불가능한 기능을 명확히 하고 테스트로 고정해야 합니다. 컨텍스트 메서드나
// FuncMap의 readFile 하나를 추가하면 작은 변경처럼 보여도
// 접근 능력이 조용히 늘어날 수 있습니다.

// TestTemplateContextHasNoMethods가 가장 중요한 검사입니다.
//
// text/template은 공개 메서드를 호출할 수 있으므로({{.Foo}}는 필드/메서드 모두 가능)
// 공개 메서드가 있는 유형에 접근하면 그 기능을 템플릿 작성자에게 제공하게 됩니다.
// 컨텍스트는 의도적으로 공개 필드만 있고 메서드 없는 순수 데이터입니다.
//
// 실패하면 webhookTemplateData / webhookItem에 메서드가 추가된 것입니다.
// 허용 전 해당 메서드가 비공개 정보를 읽는 데 쓰일 수 있는지 확인하세요.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s에 %d개 메서드(%s)가 노출되어 text/template이 호출할 수 있으며 "+
				"템플릿 작성자에게 해당 기능을 제공하게 됩니다", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal은 템플릿에 제공하는 함수 집합을 고정합니다.
//
// FuncMap 함수마다 기능이 늘어납니다. 현재 json/jsons는 JSON 직렬화만 하며
// 파일 읽기, 요청 전송, 명령 실행을 할 수 없습니다.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("템플릿 함수 집합 변경: 실제 %v, 예상 %v. 추가 전에 접근 능력이 늘어나지 않는지 확인하세요"+
			"(파일 읽기/쓰기, 네트워크 요청, 명령 실행 금지)", got, want)
	}
}

// TestTemplateCannotReachUnknownData는 범위 밖 접근을 검증합니다.
// 없는 항목 접근은 실패해야 하며 오류에 내부 데이터를 노출하면 안 됩니다.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("없는 필드 접근은 오류여야 합니다")
	}
	// 오류에 실제 취약점 제목/요약을 포함하면 안 됩니다.
	for _, leak := range []string{"SQL 인젝션", "매개변수 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("템플릿 오류에 메시지 내용 %q 유출: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently는 잘못된 템플릿이 재시도로 해결되지 않는 설정 오류임을 검증합니다.
// 재시도 가능으로 분류하면 전송마다 불필요한 백오프 세 번을 수행합니다.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("템플릿 구문 오류는 저장 시 차단해야 합니다")
	}
	// 검증을 우회해 전송해도 반복 재시도하지 않고 영구 실패여야 합니다.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("잘못된 템플릿은 영구 실패여야 함, 실제 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON은 템플릿 결과가 유효한 JSON이어야 함을 검증합니다.
// 일반 텍스트를 만들어 다른 프로토콜을 유발하는 사용도 막습니다.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 유효한 템플릿은 통과합니다.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("유효한 템플릿은 검증을 통과해야 합니다: %v", err)
	}
	// JSON 아닌 결과는 그대로 보내지 않고 거부합니다.
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON 아닌 결과는 영구 실패여야 함, 실제 %v", err)
	}
	if !strings.Contains(err.Error(), "유효한 JSON") {
		t.Errorf("오류가 JSON 문제임을 알려야 함, 실제 %v", err)
	}
}
