package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 IM 알림의 채널 설정과 이벤트 계층을 담당한다. 전송 작업 획득과 상태 전환은
// db/notification_delivery.go。
//
// 수정 시 반드시 지켜야 할 두 가지 불변 조건:
//
//  1. 취약점 저장 트랜잭션(RecordFindingTx)은 InsertNotificationEventTx의 단순 삽입만 호출한다.
//     알림 테이블을 읽거나 필터를 대조하지 않는다. 읽기를 추가하면 잘못된 사용자 필터 설정이
//     취약점 저장 트랜잭션에 영향을 주거나 중단시킬 수 있다.
//  2. 필터 대조는 오류를 반환하지 않는다. 잘못된 설정은 일치로 처리한다(notify.Match 참고).
//     알림 누락보다 추가 전송을 우선한다.

// ErrNotificationChannelNotFound는 채널이 없음을 나타낸다.
var ErrNotificationChannelNotFound = errors.New("알림 채널이 없습니다")

// 전송 상태.
const (
	NotifyStatePending = "pending" // 전송 대기
	NotifyStateSending = "sending" // dispatcher가 획득했고 임대 기간이 남아 있음
	NotifyStateSent    = "sent"    // 전송 완료
	NotifyStateFailed  = "failed"  // 재시도 소진 또는 영구 실패. 수동 재전송 가능
	NotifyStateSkipped = "skipped" // 채널 비활성화로 전송하지 않음
)

// 알림 모드.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode는 허용 목록으로 알림 모드를 검증한다. findings.status와 마찬가지로
// 향후 확장을 위해 DB CHECK는 사용하지 않는다.
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel은 채널 인스턴스 설정이다. Config와 Filter는 원본 JSON을 유지하며
// 필드 의미를 모르는 db 계층 대신 notify 패키지가 해석한다.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled는 필드 생략과 명시적 false를 구분하기 위해 포인터를 쓴다.
	// 프런트엔드 토글은 변경된 필드만 제출한다.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled는 활성화 여부를 반환한다. Enabled가 nil(미로드)이면 활성으로 처리한다.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent는 이벤트 사실 하나다.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels는 활성 채널 우선, 같은 그룹은 ID순으로 모든 인스턴스를 반환한다.
// UI와 dispatcher가 동일한 안정적 순서를 보도록 SQL에서 정렬한다.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID는 채널 하나를 조회한다.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel은 채널을 생성하거나 갱신한다.
//
// 갱신 시 호출자가 명시한 필드(nil이나 빈 값이 아닌 필드)만 덮어쓴다. 프런트엔드가
// 표시하지 않은 config 필드를 재전송하지 않고 서랍 폼의 부분 변경만 제출할 수 있다.
// 재전송하면 마스킹 값으로 실제 키가 덮어써질 수 있다.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// 0은 속도 제한 없음을 뜻하는 유효한 설정이므로 변경하지 않는다.
	//
	// 이전 if c.RatePerMin <= 0 { c.RatePerMin = 기본값 }은 미설정 시 안전한 기본값을
	// 제공하려던 의도와 달리 명시적 0까지 무시했다. 문서, UI, takeTokens는 모두 0을
	// 무제한으로 해석하지만 여기서만 DingTalk/WeCom/Telegram은 20,
	// Feishu는 100으로 바꿔 사용자가 제한을 풀었다고 생각해도 안내 없이 분당 20개로 제한되었다.
	//
	// 요청 필드 생략과 명시적 0의 차이는 호출자만 알 수 있으므로
	// 기본값은 서버의 notifyCreateChannel에서 필드가 없을 때 채운다.
	if c.RatePerMin < 0 {
		return 0, errors.New("속도 제한 값은 음수일 수 없습니다")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled는 활성화 여부를 전환한다.
//
// 비활성화 시 미전송 항목을 skipped로 표시한다. 그렇지 않으면 재활성화 후
// 비활성 기간에 쌓인 오래된 취약점이 갑자기 전송되어 새 취약점으로 오인할 수 있다.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "채널이 비활성화되었습니다", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel은 채널을 삭제한다. 설정이 없으면 기록을 해석할 수 없으므로
// 전송 기록도 외래 키에 따라 연쇄 삭제한다.
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx는 호출자의 트랜잭션에서 가능한 한 알림 이벤트를 저장한다.
//
// 취약점 저장 경로의 유일한 알림 작업은 INSERT 하나다. 테이블 읽기, 채널 조회,
// 필터 대조를 하지 않는다. 커밋 시 취약점 저장과 알림 작업 존재를 원자적으로 보장하여
// 저장 성공 후 대기열 누락으로 메시지가 영구 소실되는 구간을 없앤다.
//
// 다음 두 설계는 의도적이다.
//
//  1. SAVEPOINT 이유: PostgreSQL은 트랜잭션의 문장 하나가 실패하면 전체가 aborted가 되어
//     이후 COMMIT을 포함한 모든 문장이 실패한다. 저장점으로 해당 문장의 오류를 격리해야
//     INSERT 오류를 무시하고 호출자가 계속 커밋할 수 있다.
//     저장점이 없으면 전체 롤백만 가능하다.
//
//  2. 전체 롤백을 피하는 이유: 알림은 편의 기능이며 취약점 기록이 핵심이다. 알림 테이블의
//     문제(미적용 마이그레이션, 일시적 디스크 장애)로 고위험 취약점을 저장하지 못하면 안 된다.
//     오류를 격리하고 로그를 남긴 뒤 false를 반환하여 취약점 저장은 커밋한다. 해당 알림은 누락된다.
//     저장 성패에 영향을 주는 오류로 취급하지 않도록 error 대신 bool을 반환한다.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 알림 이벤트 직렬화 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 저장점 생성 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 알림 이벤트 저장 실패 finding=%d(취약점 기록은 영향 없음): %v", findingID, err)
		// 저장점으로 롤백하여 트랜잭션을 aborted 상태에서 복구한다.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 저장점 롤백 실패 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// 긴 트랜잭션에 불필요한 저장점이 쌓이지 않도록 해제한다.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent는 기존 트랜잭션 밖의 호출자를 위한 InsertNotificationEventTx 독립 트랜잭션 버전이다.
// 예를 들어 실제 finding이 없는 채널 테스트 메시지 전송에서 사용한다.
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("알림 이벤트 스냅샷 직렬화 실패: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents는 미분배 취약점 이벤트를 현재 활성 채널별 전송 작업으로 확장하고
// 이번에 처리한 이벤트 수와 생성한 전송 수를 반환한다.
//
// 전체를 하나의 트랜잭션으로 처리한다. FOR UPDATE SKIP LOCKED로 이벤트를 획득하여
// 여러 프로세스가 동시에 실행해도 서로 다른 행을 얻는다. 보관 대기열도 같은 방식을 쓴다
// (db/task_archives.go의 completeNextArchiveJob 참고).
//
// 선택 필드 JSONB인 채널 필터의 여섯 조합을 SQL로 표현하면 유지보수가 어려워
// Go에서 대조한다. 채널은 사람이 설정하는 소수 항목이므로
// 전체를 메모리에 읽고 하나씩 비교하는 편이 빠르고 테스트하기도 쉽다.
//
// 어느 채널과도 일치하지 않는 이벤트도 fanned_out으로 표시하여
// 대기 집합에 영구 잔류하며 tick마다 재검색되지 않게 한다.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋 성공 후에는 no-op

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// 자체 저장 스냅샷은 원칙적으로 해석 가능하지만 실패해도 전송 흐름을 막지 않는다.
		// 이 이벤트는 필드가 모두 비어 필터 있는 채널에서 건너뛰게 된다. 하나의 잘못된 행이
		// 전체 대기열을 멈추게 하는 대신 알림 하나의 누락을 허용한다.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind는 행 값을 기준으로 한다. 스냅샷 값은 렌더링용 사본이며 구버전이 작성했을 수 있다.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// 어느 채널과도 일치하지 않는 항목을 포함해 이번 이벤트를 분배 완료로 표시한다.
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx는 트랜잭션에서 활성 채널을 조회한다. 개수가 적어
// 페이지 구분과 캐시를 쓰지 않는다. 캐시는 설정 변경 적용 시점 문제를 추가한다.
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames는 알림용으로 자산 ID를 짧은 표시 이름으로 변환한다.
//
// 입력 순서를 유지하고 없는 ID는 생략하므로 결과가 더 짧을 수 있다. 같은 취약점의
// 재전송에서도 자산 순서가 안정적으로 유지되어야 순서 변경을
// 자산 변경으로 오해하지 않는다.
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName은 자산 유형별로 가장 식별하기 쉬운 값을 선택한다.
// 없으면 빈 문자열을 반환하고 이름 없는 자산 표시는 호출자가 정한다. 임의 자리표시자를 만들면
// 자산#42 같은 값이 알림에 섞여 실제 도메인으로 오해될 수 있다.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify는 취약점 처리 상태를 갱신하고 같은 트랜잭션에
// 상태 변경 알림 이벤트를 등록한다.
//
// from은 이전 상태, found는 취약점 존재 여부, notified는 이벤트 등록 성공 여부다.
//
// 세 가지 의도된 동작:
//   - 실제 상태가 바뀌지 않으면 이벤트를 등록하지 않는다. 서랍 폼에서 같은 값을
//     다시 제출하거나 자동화가 멱등 재실행되어도 불필요한 알림을 만들지 않는다.
//   - 취약점이 없으면 쓰기 없이 found=false를 반환하고 호출자가 404로 변환한다.
//   - 이벤트 등록 실패는 상태 갱신에 영향을 주지 않는다(RecordNotificationEventTx 저장점 참고).
//     notified=false여도 상태는 갱신되었으므로 호출자가 오류를 보고하면 안 된다.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx는 호출자의 트랜잭션에서 취약점 상태를 바꾸고 알림 이벤트를 등록한다.
//
// 모든 상태 변경 경로가 같은 의미를 갖도록 트랜잭션 함수로 분리했다. 이전에는
// patchFinding만 알림 포함 버전을 사용하고 재검증이 수정됨으로 결론 나면 finding_retests의
// UPDATE findings SET status=...로 직접 저장했다. 따라서
// on_status_change 채널은 이 전환 알림을 받지 못하고 화면 상태만 조용히 바뀌어
// 운영자가 플랫폼을 열어야 알 수 있었다.
//
// from은 이전 상태, found는 취약점 존재 여부, changed는 실제 변경 여부,
// notified는 이벤트 등록 성공 여부다(실패해도 상태 갱신은 유지, RecordNotificationEventTx 참고).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 같은 값 반복 제출이나 멱등 재실행으로 불필요한 알림이 생기지 않도록
		// 실제 상태가 바뀐 경우에만 이벤트를 등록한다.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats는 알림 페이지 상단의 개요 집계다.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 가장 오래된 대기 전송의 경과 밀리초
}

// NotificationStatsSnapshot은 알림 시스템 상태를 요약한다.
// BacklogAgeMS는 알림 정체를 가장 직접적으로 보여 주며 pending 개수보다 유용하다.
// 같은 세 개의 대기 항목도 3초와 3시간의 차이가 있을 수 있기 때문이다.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
