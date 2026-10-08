package db

import (
	"regexp"
	"testing"
)

// 내장 삭제 API 경로 규칙은 tool_input JSON 전체를 대조하므로 테스트도
// Interceptor가 실제 받는 subject와 동일한 JSON 형태를 사용한다.
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET으로 삭제 API 호출
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST로 삭제 API 호출
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 경로 규칙이 허용하지 않던 접미사 보완
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("일치해야 하지만 허용됨: %s", s)
		}
	}

	// /delivery나 /details 같은 읽기 전용 경로를 잘못 차단하지 않도록 동사 뒤에 구분자가 필요하다.
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 삭제 동사가 경로 대신 도메인에 있음
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("잘못 차단됨: %s", s)
		}
	}
}
