package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache는 인증 없는 GitHub API의 IP당 시간당 60회 한도를 보호한다.
// 상단 새 버전 알림은 페이지 전체 로드 때마다 조회하므로 캐시가 무효화되면 여러 탭만으로도
// 한도를 소진해 실제 업데이트 확인이 막힐 수 있다.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5회 조회에서 원격 접근은 1회여야 하지만 실제 %d회", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 사용자가 업데이트 확인을 누르면 캐시 만료를 기다리지 않고 새 릴리스를 볼 수 있어야 한다.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force는 캐시를 우회해야 함: 원격 접근 기대 2회, 실제 %d회", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 캐시 저장 시각을 과거로 옮겨 TTL이 막 만료된 상황을 재현한다.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 만료 후 다시 원격 조회해야 함: 기대 2회, 실제 %d회", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("GitHub에 연결할 수 없음")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류를 반환해야 함")
	}
	// GitHub에 연결할 수 없을 때 매 페이지 로드마다 시간 초과를 기다리지 않도록 실패도 잠시 캐시한다.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류를 반환해야 함")
	}
	if calls != 1 {
		t.Errorf("오류도 잠시 캐시해야 함: 원격 접근 기대 1회, 실제 %d회", calls)
	}

	// 네트워크 복구 후 빠르게 회복하도록 오류 TTL은 성공 TTL보다 훨씬 짧아야 한다.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("오류 TTL(%v)은 성공 TTL(%v)보다 짧아야 함", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류를 반환해야 함")
	}
	if calls != 2 {
		t.Errorf("오류 TTL 만료 후 재시도해야 함: 기대 2회, 실제 %d회", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 방문자가 탭을 닫아 요청이 취소되어도 GitHub 장애는 아니다. 취소 오류를 캐시하면
	// 이후 30분간 모든 방문자가 이유 없는 오류를 받게 된다.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 캐시를 만료시켜 원격 조회를 유도한다

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("호출자가 취소하면 오류를 그대로 전달해야 함")
	}

	// 핵심 불변 조건: 취소된 호출은 캐시에 취소 오류를 남기지 않고
	// 이전의 정상 결과를 유지해야 한다.
	if c.err != nil {
		t.Fatalf("취소 오류는 캐시에 기록하면 안 됨: %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("캐시는 이전 정상 결과를 유지해야 함: %+v", c.rel)
	}

	// 취소된 호출은 새 데이터를 받지 못했으므로 다음 방문자는 다시 원격 조회하여
	// 이전 취소의 영향 없이 정상 결과를 받아야 한다.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("취소 이후 정상 요청에서 오류가 발생하면 안 됨: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("정상 결과를 받아야 함: %+v", rel)
	}
}
