package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// 웹의 원클릭 업데이트 HTTP 계층. 실제 다운로드/검증/교체는 selfupdate 패키지가 담당하고
// 여기서는 인증 경계, 동시 실행 방지, 진행률 전파와 main에 종료 시점을 알리는 일을 맡는다.
//
// 이 프로세스가 직접 재시작하지는 않는다. 새 버전을 준비한 뒤 selfupdate.ExitRestart로 종료하면
// 감시 스크립트(start.sh / start.bat, Docker에서는 ENTRYPOINT)가 다시 실행한다.

// restartCh는 업데이트 준비 또는 롤백 완료 후 닫히며 main은 이를 받고 ExitRestart로 종료한다.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested는 종료 후 감시 프로세스가 다시 실행해야 할 때 닫히는 채널을 반환한다.
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState는 이번 시작 시 selfupdate.Bootstrap 결과(업데이트 성공/롤백 직후/임시 파일 폐기)다.
// main이 주입하며 /api/update/check가 이전 업데이트 결과를 프런트엔드에 정확히 전달할 때 사용한다.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState는 시작 시 main이 한 번 호출한다.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache는 GitHub의 최신 버전 조회 결과를 캐시한다.
//
// 상단의 새 버전 알림은 페이지를 전체 로드할 때마다 조회한다. 인증 없는 GitHub API는
// IP당 시간당 60회이므로 캐시가 없으면 탭을 여러 개 열거나 새로고침만 해도 한도를 소진한다.
// 이후 실제 업데이트 확인이 막히므로 캐시를 사용하되 명시적인 업데이트 확인은 force로 우회한다.
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch는 테스트용 주입 지점이며 nil이면 실제 GitHub 조회를 사용한다.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// GitHub에 연결할 수 없을 때 매 페이지 로드마다 시간 초과를 기다리지 않도록 실패도 잠시 캐시한다.
	// 네트워크 복구 후 빠르게 회복할 수 있도록 실패 TTL은 짧게 둔다.
	releaseErrTTL = 2 * time.Minute
	// 버전 조회 시간 제한. NewClient의 30분 제한은 전체 다운로드용이므로 조회에는 너무 길다.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get은 최신 릴리스를 반환하며 캐시가 유효하면 네트워크에 접근하지 않는다.
//
// 조회 중에는 잠금을 유지해 동시 요청이 각각 GitHub에 접근하지 않고 같은 결과를 기다리게 한다.
// 여러 탭을 동시에 로드하며 조회할 때 속도 제한에 걸리기 쉽기 때문이다.
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// 요청 취소(사용자가 탭을 닫음)는 GitHub 장애가 아니므로 캐시하지 않는다.
	// 그렇지 않으면 다음 방문자가 이유 없이 취소 오류를 받게 된다.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress는 프런트엔드에 전달할 진행 상태다.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // 다운로드 단계에서만 유효하며 나머지는 -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub는 업데이트 진행 상태를 보유하고 SSE 구독자에게 전파한다.
//
// running은 상호 배제도 담당한다. 업데이트 중 POST /api/update/apply가 다시 오면 409를 반환해
// 두 goroutine이 같은 artex.new에 동시에 쓰지 못하게 한다.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin은 업데이트 실행권을 얻으며 이미 진행 중이면 false를 반환한다.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "준비 중…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish는 업데이트를 마친다. err가 nil이면 준비에 성공했으며 재시작을 기다린다.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "업데이트 실패", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "새 버전이 준비되어 재시작 중입니다…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout은 h.mu 잠금을 가진 상태에서 호출해야 한다. 구독자 채널은 버퍼가 가득 차면 메시지를 버린다.
// 진행 상태는 버려도 되는 일시 정보이므로 멈춘 SSE 연결 하나가 업데이트 자체를 막아서는 안 된다.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck는 GitHub의 최신 정식 버전을 조회해 현재 버전과 비교한다.
//
// 프런트엔드도 api.github.com에 직접 접근하지만(GitHub CORS는 *) 이 API의 결과를 기준으로 삼는다.
// 다운로드는 백엔드가 담당하므로 백엔드에서 GitHub에 접근할 수 있어야 한다. 브라우저만 연결되고
// 서버는 연결되지 않는 경우(내부망 서버/브라우저에만 프록시 설정)가 흔하며 이때 업데이트는 실패한다.
// 따라서 확인 단계에서 정확한 오류를 안내한다.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// 상단 알림은 기본적으로 캐시를 사용한다. 사용자가 업데이트 확인을 누르면 force=1로 직접 조회한다.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 개발 빌드(dev 또는 접미사가 붙은 git describe)는 비교 가능한 버전 번호가 없다. 허용하면
		// 디버깅 중인 로컬 바이너리를 정식 버전으로 덮어쓰므로 업데이트를 제공하지 않는다.
		out["reason"] = fmt.Sprintf("현재 버전 %q은(는) 정식 릴리스가 아니므로 원클릭 업데이트를 사용할 수 없습니다", current)
	}
	writeJSON(w, 200, out)
}

// updateApply는 새 버전을 다운로드해 준비하고 프로세스를 종료하여 감시 스크립트가 재시작하게 한다.
//
// 전체 다운로드에 수 분이 걸릴 수 있어 즉시 202를 반환하고 백그라운드 goroutine에서 실행한다.
// 요청에 연결하면 리버스 프록시의 시간 제한에 걸린다. 진행 상태는 /api/update/stream으로 전달한다.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// 사용자가 화면에서 확인한 버전과 설치 버전이 같도록 캐시를 사용한다.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("현재 버전 %q은(는) 정식 릴리스가 아니므로 원클릭 업데이트를 사용할 수 없습니다", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("이미 최신 버전 %s입니다", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "다른 업데이트가 이미 진행 중입니다")
		return
	}

	go func() {
		// HTTP 응답 직후 종료되는 요청 ctx 대신 의도적으로 s.ctx를 사용한다.
		// 요청 ctx에 다운로드를 연결하면 응답 직후 취소되기 때문이다.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 업데이트 실패: %v", err)
			return
		}
		log.Printf("[update] %s → %s 준비 완료, 교체를 마치기 위해 곧 종료합니다", current, rel.TagName)
		// 마지막 진행 상태를 프런트엔드에 전달할 시간을 조금 준 뒤 종료한다.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback은 교체 전에 백업한 artex.old를 이용해 이전 버전으로 되돌린다.
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "업데이트 중에는 롤백할 수 없습니다")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 이전 버전으로 수동 롤백했습니다. 교체를 마치기 위해 곧 종료합니다")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream은 SSE로 업데이트 진행 상태를 전달한다.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 새로고침 후에도 진행 중인 업데이트를 바로 볼 수 있도록 현재 상태를 먼저 보낸다.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
