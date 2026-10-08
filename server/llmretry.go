package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 재시도 정책 서버 해석(docs/LLM重试设计.md). 다섯 계층 중:
//   - 연결/빈 응답/동일 provider 안전 구간은 엔드포인트별이며 LLM 설정이 전역 기본값을 재정의할 수 있다.
//     profile 항목 미설정은 전역 상속, 전역도 미설정이면 내장 기본값 사용.
//   - 차단/의도 재실행은 프로세스 수준으로 전역 설정 하나만 사용.
//
// 전역 정책은 settings 한 행을 읽으며 provider 생성, work 마무리, 설정 저장 등
// 저빈도 경로에서 호출되어 캐시가 필요 없다. 단, 실패마다 읽는 차단 매개변수는
// applyRetryPolicy가 Registry로 전달하여 보관한다.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 횟수는 0=기본/음수=비활성 의미를 유지한다. SDK MaxRetries와
		// EmptyResponseRetries가 동일한 의미를 사용하므로 직접 해석하도록 둔다.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 차단 대기 기본값은 llmpool과 동일하며 사용자가 값을 지정한 경우에만 재정의한다.
// 의도 재실행 기본값은 engine.go의 modelErrorRetries / modelErrorRetryBackoff 참고.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). 계층 ②의 빈 응답 재시도 횟수를 재사용한다.
// 같은 문제에 대한 두 방식이다. SDK는 내용 블록이 전혀 없을 때 같은 요청을 재전송하고,
// 여기서는 추론만 있고 본문/도구가 없을 때 지시를 추가하여
// 기존 추론을 이어간다(컨텍스트 형태에 따른 빈 턴에 동일 요청 재전송은 의미 없음).
// SDK는 이벤트 발생 여부로 판정하고 추론 델타도 이벤트이므로 빈 응답 기준은 다르다.
// 하지만 사용자가 원하는 것은 실질 내용이 없으면 다시 시도하는 것이므로
// 두 계층이 같은 횟수를 공유해야 한다.
//
// 실행 중 장애 조치로 profile이 바뀔 수 있지만 의도 전체 상한은 엔드포인트에 따라
// 달라지면 안 되므로 profile 재정의 대신 전역 정책을 읽는다. SDK emptyRetries()와 같은 의미:
// 0=defaultEmptyTurnNudges, -1(음수)=빈 턴 재개 비활성, >0=지정값.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
