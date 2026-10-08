package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 전역 설정 키(settings 키·값 테이블 사용, 새 테이블 불필요).
const (
	// settingNotifyEnabled는 기본 활성인 알림 전체 설정으로 유지보수 중 즉시 중지하는 용도다.
	// 기능의 실제 활성 조건은 채널 설정 여부다.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL은 취약점 상세 링크를 만들 외부 접근 주소다
	// (예: https://artex.example.com). 빈 값이면 링크 버튼을 넣지 않는다.
	// 재사용할 외부 주소 설정이 없어 별도 항목을 추가했다.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes는 요약 주기(분)다.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick은 전송 엔진 조회 간격이다. 3초는 실시간 응답의 상한이자
	// 취약점 저장부터 메신저 도착까지 발생하는 주된 지연이다.
	notifyTick = 3 * time.Second
	// notifyLease는 전송 획득의 임대 시간이며 단일 전송 최악 시간
	// (notify HTTP 클라이언트 15초)보다 충분히 길어야 두 dispatcher가
	// 같은 행을 동시에 보내지 않는다.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick은 최초 채널 활성화 때 과거 누적 항목을 한꺼번에
	// 전송 작업으로 확장하지 않도록 회차별 이벤트 분배 수를 제한한다.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes는 기본 요약 주기다.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick은 속도 제한 없는 채널의 회차별 전송 상한이다.
	// 무제한 채널에 한 번에 수천 취약점이 생겨도
	// 한 회차가 오랫동안 막히지 않게 한다.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick은 채널당 회차별 최대 전송 수다.
	//
	// 획득 시 3분 임대(notifyLease)를 설정하므로 상한은 임대 시간에서 역산한다.
	// 직렬 전송의 최악 시간이 임대를 넘으면 뒤쪽 항목 전송 전에 임대가 끝난다.
	// 단일 프로세스는 Run goroutine이 직렬이며 tick 재진입이 없어 괜찮지만
	// 같은 DB를 쓰는 두 프로세스는 만료 행을 재획득하여 중복 전송하고
	// attempts도 이중 증가시켜 원래 전송 중에 실패로 판정할 수 있다.
	//
	// 3분 임대 / 30초 전송 = 6은 여유가 전혀 없어 사용할 수 없다.
	// 5로 설정하면 최악 150초에 30초 여유가 남는다. 이 관계는
	// TestNotifyTickBudgetFitsWithinLease가 보장하며 notifyLease,
	// notifySendTimeout 또는 이 값을 바꾸면 해당 검증이 실패한다.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout은 단일 전송 시간 제한이며 위 상수도 결정한다.
	// 두 값의 곱이 notifyLease를 넘으면 안 된다(TestNotifyTickBudgetFitsWithinLease).
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff는 시도 횟수를 인덱스로 쓰는 실패 재시도 백오프다.
// 최초 포함 3회는 db.MaxNotifyAttempts와 대응하므로 함께 변경해야 한다.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier는 취약점 알림 전송 엔진이다.
//
// server.New에서 Scheduler와 별개 goroutine으로 실행한다. 알림의 3초 실시간 요구는
// 트리거 실행 주기와 달라 Scheduler tick을 재사용하지 않는다.
// 알림 정체가 에이전트 트리거에 영향을 주지 않도록 실패도 분리한다.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu는 buckets를 보호한다. 채널 수와 경합이 적어 단일 mutex로 충분하며
	// 더 세밀한 구조가 필요 없다.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket은 채널별 토큰 버킷이다.
//
// 매분 카운터를 초기화하는 윈도 방식은 경계에서 문제가 생긴다.
// 마지막 순간 20개와 다음 순간 20개가 합쳐져 초당 40개로 제한에 걸릴 수 있다.
// 토큰 버킷은 일정 속도로 보충하여 이런 순간 폭증을 방지한다.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run은 ctx 종료까지 반복하며 server.New가 한 번 시작한다.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step은 새 이벤트 분배 후 기한이 된 작업을 전송한다.
//
// 실패는 로그만 남기고 루프를 중단하지 않아 알림 장애가 프로세스 장애로 번지지 않게 한다.
// 각 tick은 독립적이며 다음 회차에서 자연스럽게 재시도한다.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] 이벤트 분배 실패: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] 채널 읽기 실패: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// 토큰 단위는 취약점 수가 아닌 메시지 수(HTTP 요청 수)다.
		// 실시간은 취약점 하나당 메시지 하나지만 요약은 배치 전체를
		// 메시지 하나로 보내므로 토큰 하나만 소비한다.
		//
		// 두 모드 모두 토큰을 먼저 확인하고 예산만큼 획득한다. 순서를 바꾸면
		// 속도 제한에 막힌 전송도 재시도 횟수를 소비한다.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan은 이번 요약 채널의 토큰 소비와 배치 크기 상한을 반환한다.
//
// 두 값의 단위가 다르므로 독립 함수로 둔다.
//
//   - tokens는 메시지 수. 배치 전체가 메시지/HTTP 요청 하나여서 항상 1이다.
//     rate_per_min은 분당 요약 메시지 상한으로 계속 적용된다.
//   - claimLimit는 배치 취약점 수. 요청 예산과 무관하며 메모리 상한만 따른다.
//
// 이전에는 digest에 rate_per_min을 적용하려고 임대에서 역산한 회차별 요청 예산
// notifyMaxSendsPerChannelPerTick을 배치 크기로 전달했다.
// rate_per_min=20이면 3초 tick에 토큰 하나만 보충되어 요약에도 취약점 하나만 담았다.
// 결과적으로 요약 문구가 붙은 실시간 전송이 되어
// 최근 30분 새 취약점 1개 메시지가 연속 도착하고 db.MaxDigestBatchSize에 도달하지 못했다.
//
// 기존 E2E는 stepDigest에 충분히 큰 limit를 직접 전달하여 step 예산 계산을 우회하므로
// 발견하기 어려웠다. 결정을 여기로 모아
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget으로 직접 검증한다.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime은 채널 실시간 작업을 획득하여 취약점당 메시지 하나를 전송한다.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 실시간 전송 획득 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q가 등록되지 않았습니다", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 렌더링 실패는 로컬 데이터 문제라 재시도로 해결되지 않는다.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest는 기한이 된 채널의 대기 전송을 메시지 하나로 요약한다.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 요약 배치 판정 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 요약 배치 획득 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q가 등록되지 않았습니다", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// 잘못된 스냅샷으로 메시지에 포함하지 못한 전송은 명시적으로 실패 처리한다.
	// 그렇지 않으면 included 밖에 남아 메시지와 실패 목록 모두에 없고 성공 후 일괄 표시에서도
	// 빠져 sending에 머물다가 임대 만료 때마다 재획득된다.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "이벤트 스냅샷을 해석할 수 없어 이 취약점을 메시지로 렌더링할 수 없습니다"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 잘못된 스냅샷 전송의 실패 표시 오류 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 스냅샷 해석 불가 전송 %d개 건너뜀 channel=%d", len(skipped), ch.ID)
	}
	// 메시지에 포함된 항목만 send에 전달하며 included[i]와 msg.Items[i]는 정확히 대응한다.
	// 이 관계로 채널이 처음 K개를 담았다고 보고하면 올바른 전송 행을 갱신한다.
	n.send(ctx, channel, cfg, msg, included)
}

// send는 전송 결과에 따라 상태를 전환한다.
//
// 같은 배치(요약에서는 수십 개일 수 있음)는 같은 전송 결과로 함께 성공하거나 재시도한다.
// 요약은 메시지 하나이므로 일부만 재시도하면 배치 의미가 깨진다.
//
// 예외는 채널 길이 제한에 따른 분할이다. 앞 K개만 담았다면
// K+1부터는 성공 표시하지 않고 다음 배치에 남겨야 한다. 그렇지 않으면 잘린
// 취약점은 메시지와 실패 목록에서 모두 사라진다.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 채널 하나의 정체가 나머지를 막지 않도록 단일 전송 시간을 제한한다.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 보고된 수가 전송 수보다 크면 렌더링 계층 계산 오류다.
			// 기록을 망가뜨리는 대신 모두 전송됨으로 처리하고 문제를 기록한다.
			log.Printf("[notify] 채널 보고 완료 수 %d가 전송 수 %d를 초과 channel=%s, 모두 완료로 처리",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 전송 완료 표시 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 채널 길이 상한에 도달했으므로 나머지를 즉시 대기열로 돌려 다음 tick에서 이어 보낸다.
			// 실패가 아니므로 RescheduleDeliveries 대신 DeferDeliveries를 사용하여
			// 획득 시 미리 증가한 시도 수를 되돌리고 재시도 예산을 보존한다.
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("메시지 길이 상한으로 앞 %d개만 전송했고 나머지는 다음 배치에서 전송합니다", delivered)); err != nil {
				log.Printf("[notify] 분할 후속 전송 대기열 등록 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 오류도 완료 수도 보고하지 않으면 백오프 실패로 처리하여
		// 표시되지 않은 전송이 반복 획득되는 것을 방지한다.
		err = fmt.Errorf("채널이 완료 개수를 보고하지 않았습니다(delivered=%d)", delivered)
	}

	// 실패 처리는 배치 최대 시도 수가 아닌 항목별로 결정한다.
	//
	// 이전 maxAttempts(deliveries) >= MaxNotifyAttempts는 배치 전체를 실패시켰지만
	// 각 항목의 시도 수는 다르다. 이미 두 번 시도한 오래된 전송(attempts=2)이
	// 새 전송(attempts=1)까지 failed로 만들어 새 취약점이 재시도 없이 영구 누락되었다.
	// 이는 오래된 행이 새 행에 피해를 주지 않게 하려는 의도에 반한다.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("%d회 시도 후에도 실패: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 백오프는 세 단계뿐이므로 지연별로 묶어 재배치한다. 항목별 UPDATE로
	// 500개 배치에 500번 왕복할 필요가 없다.
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 전송 재배치 실패 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 전송 실패 channel=%d kind=%s 영구 실패=%d 재시도 소진=%d 재시도 대기=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries는 포인터 식별자로 비교하여 all 중 keep에 없는 항목을 반환한다.
// 메시지에 담지 못한 전송을 찾아 명시적으로 처리하고 불명확한 상태로 남기지 않는다.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt는 채널 구현을 가져와 설정을 해석한다.
// ok=false는 미등록 유형으로 무한 재시도 대신 즉시 실패 처리해야 한다.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 설정 해석 실패 시 빈 map을 넘기면 채널 Validate가 누락 필드를 안내한다.
		// 이는 JSON 해석 오류보다 사용자에게 도움이 된다.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle은 단일 취약점 메시지를 렌더링한다.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch는 스냅샷을 개별 해석하여 잘못된 항목 하나 때문에
// 전체 요약이 사라지지 않도록 해당 항목만 생략한다.
//
// included와 msg.Items는 엄격히 일대일 대응한다(i번째 전송 ↔ i번째 항목).
// 호출자가 채널의 앞 K개 수용 보고에 따라 처음 K개를 완료로 표시하므로
// 잘못된 스냅샷을 건너뛰면 included에서도 제거해야 한다. 그렇지 않으면
// 인덱스가 어긋나 잘못된 항목을 완료로, 정상 항목을 미완료로 표시한다.
// 잘못된 항목은 호출자가 명시적으로 실패 표시한다(stepDigest).
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 잘못된 스냅샷은 메시지와 included 모두에서 제외하고 호출자가
			// 완료 항목에 섞지 않고 명시적으로 실패 처리한다.
			log.Printf("[notify] 요약 배치의 해석 불가 스냅샷 생략 delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("요약 배치 전송 %d개를 모두 해석할 수 없습니다", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor는 스냅샷을 알림 항목으로 변환하며 자산 이름과 상세 링크를 해석한다.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 자산 이름 해석 실패는 알림을 막지 않는다. 알림 누락보다
		// 메시지에 자산 한 줄이 빠지는 편이 낫다.
		log.Printf("[notify] 자산 이름 해석 실패 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 상세 경로는 web/src/app/(main)/function/findings/detail/page.tsx이며
		// 쿼리 인자 id에서 취약점 ID를 읽는다.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens는 채널 버킷에서 최대 want개 토큰을 가져와 실제 개수를 반환한다.
//
// 토큰 하나는 메시지/HTTP 요청 하나다. 실시간은 원하는 전송 수를,
// 요약은 전체 배치가 메시지 하나이므로 1을 전달한다.
//
// 버킷 용량은 채널 분당 상한이며 일정 속도로 보충한다. ratePerMin<=0은 무제한이지만
// 무한 누적이 한 회차를 막지 않도록 충분히 크고 유한한 값을 반환한다.
//
// want 상한이 없으면 버킷 전체를 비워 호출자의 회차 상한보다 많이 가져온 토큰이
// 사용되지 않고 사라진다. 누적한 순간 전송 용량에 도달하지 못하며
// 대기 전송이 없는 회차에서도 토큰이 차감된다.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 실제 경과 시간에 따라 초당 ratePerMin/60으로 보충한다.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 부동소수점 누적으로 0.5+0.5가 0.9999999999가 되어 int()가 0으로 자를 수 있으므로
	// 정수 변환 전 작은 epsilon을 더한다. 수학적으로 가득 찬 버킷에서 토큰을 못 가져오는 것을 막는다.
	// 1e-9는 토큰 하나보다 훨씬 작아 실제 부족량을 허용하지 않는다.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled는 전체 활성화 설정을 읽는다.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL은 상세 링크용 외부 주소의 끝 슬래시를 제거하여 반환한다.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval은 요약 주기를 반환하고 미설정/잘못된 값은 기본값으로 대체한다.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot은 전송 이벤트 스냅샷을 해석한다.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("전송 %d의 이벤트 스냅샷이 비어 있습니다", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("전송 %d의 이벤트 스냅샷 해석 실패: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// 스냅샷은 구버전이 작성했을 수 있으므로 유형은 이벤트 행을 기준으로 한다.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
