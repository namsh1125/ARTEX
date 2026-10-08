package db

import (
	"encoding/json"
	"time"
)

// LLM 재시도 정책: 다섯 계층의 횟수와 간격 전역 설정. docs/LLM重试设计.md 참고.
// 시스템 전체 실행 매개변수이므로 별도 테이블 대신 settings의 JSON 값 하나에 저장한다.
// 읽을 때 내장 기본값으로 보완하므로 새 DB나 미설정 상태에서도 기존 상수 방식과 동일하게 동작한다.

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule is one layer's knob pair. The zero value means "unset":
//
//	Attempts 0=내장 기본 횟수, -1=해당 계층 재시도 비활성화, >0=지정 횟수
//	IntervalMS 0=기존 간격 정책(보통 지수 백오프), >0=고정 밀리초 간격
//
// 0은 미설정 의미이므로 명시적 비활성화에는 -1을 사용한다.
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy holds the five-layer retry configuration. Connect/Empty/Stream are the
// per-request layers (a profile may override them, see LLMProfile.Retry);
// Breaker and Intent are process-wide by nature and live only here.
type LLMRetryPolicy struct {
	// Connect: 스트림 시작 전 SDK 연결 재시도(연결 리셋/시간 초과/429/5xx). 기본 3회, 지수 백오프.
	Connect RetryRule `json:"connect"`
	// Empty: 완료되었지만 content block이 없는 SDK 응답 재시도(openai 형식만). 기본 2회, 지수 백오프.
	Empty RetryRule `json:"empty"`
	// Stream: 출력 전달 전 끊긴 스트림을 같은 provider의 안전 구간에서 재실행. 기본 2회, 0.5초부터 지수 증가(최대 4초).
	Stream RetryRule `json:"stream"`
	// Breaker: 순환 선택 차단기. Attempts는 연속 일시 실패 임계값(기본 3, -1은 일시 실패 차단 안 함).
	// 잔액 부족/키 무효 같은 영구 실패는 즉시 차단한다. IntervalMS는 고정 대기 시간(0은 기본 1/5/30분 단계).
	Breaker RetryRule `json:"breaker"`
	// Intent: worker가 model_error로 끝난 의도 전체를 재실행. 기본 2회, 고정 3초.
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
