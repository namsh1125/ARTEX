package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 이 파일은 전송 작업 획득과 상태 전환을 담당한다.
//
// 긴 트랜잭션 대신 임대를 사용한다. 행을 sending으로 바꾸고 next_attempt_at을 미래의
// 임대 만료 시점으로 설정한 뒤 커밋하고 네트워크 전송을 수행한다. 전송 중 DB 잠금을 보유하지 않는다.
// 네트워크는 수초가 걸릴 수 있으며(클라이언트 시간 제한 15초) 행 잠금은 다른 쓰기 작업을 방해한다.
//
// 전송 중 프로세스가 종료되면 sending에 남지만 자동 복구할 수 있다. 임대가 만료되어
// next_attempt_at이 과거가 되면 다음 획득에서 같은 행을 다시 가져온다
// (state IN ('pending','sending') 조건 참고). 획득 시 재시도 수를 증가시키므로
// 장애가 반복되어도 무한 재시도하지 않고 MaxNotifyAttempts 소진 후 failed로 수동 처리를 기다린다.

// MaxNotifyAttempts는 최초 시도를 포함한 전송당 최대 시도 횟수다.
// 엔진은 실행자일 뿐이며 이 정책은 상태 머신에 속하므로 여기 정의한다.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize는 요약 배치 하나에 병합할 최대 전송 수다.
//
// 한 주기에 전체 스캔 등으로 수만 취약점이 발견될 수 있으므로 자원을 제한한다.
// 상한이 없으면 모든 행을 메모리에 읽고 매우 긴 메시지로 렌더링한 뒤
// 채널 길이 제한으로 대부분 잘려 메모리를 낭비하고 취약점 알림이 조용히 누락된다.
// 초과분은 DB에 남겨 다음 주기의 배치로 전송하므로 누락되지 않는다.
//
// 500은 WeCom의 4096바이트 제한 내에서 렌더링 후 읽을 내용이 남는 규모로 선택했다.
// 더 늘리면 잘리는 위치만 뒤로 이동할 뿐이다.
const MaxDigestBatchSize = 500

// NotificationDelivery는 채널 설정과 이벤트 스냅샷을 포함하는 전송 작업이다.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 조인으로 읽은 렌더링 컨텍스트. JSON에 넣지 않고 서버에서 DTO를 구성한다.
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind는 이벤트에서 가져와 기록 목록에서 취약점 상세로 이동할 때 사용한다.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind는 프런트엔드 추가 조회를 줄이는 목록 표시용 중복 필드다.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery는 전송 + 이벤트 스냅샷 + 채널 설정을 함께 읽는 공통 쿼리다.
// 메시지 렌더링에 모두 필요하며 따로 조회하면 세 번 왕복해야 한다.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery는 sel로 후보를 골라 잠근 뒤 sending으로 바꾸고 임대를 연장하는 획득 작업이다.
// sel의 lease 위치는 호출자가 $n으로 지정하고 인자를 전달한다.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries는 채널의 기한이 된 실시간 전송을 최대 limit개 획득한다.
//
// 전역 획득 후 분배하지 않고 채널별로 획득한다. 엔진의 채널별 속도 제한에서
// 이번에 보낼 수 있는 개수를 먼저 확인하고 그만큼만 획득해야 재시도 예산을 낭비하지 않는다.
// 먼저 획득하고 나중에 버리면 제한에 막힌 행에도 attempts가 증가하여
// 단순 대기만으로 3회 예산이 소진되고 failed가 된다.
//
// 만료된 sending도 포함하여 장애에서 복구한다. lease는 단일 전송 최악 시간
// (HTTP 시간 제한 15초)보다 충분히 길어야 두 dispatcher의 중복 전송을 막는다.
// 비활성 채널도 차단한다. 비활성화가 기존 전송을 skipped로 바꾸지만
// 여기서도 검사하여 비활성화와 획득의 동시 실행 사이 누락을 막는다.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue는 대기 전송 중 가장 오래된 항목이 요약 주기에 도달하여
// 채널 배치를 보낼 때가 되었는지 반환한다.
//
// 시계의 정각 대신 가장 오래된 전송의 경과 시간을 기준으로 삼아 새 채널이
// 정각에 맞춰 단일 항목 요약을 즉시 보내거나 오래 쌓인 배치가 한 주기 더 기다리지 않게 한다.
//
// 전송 시점 판정과 획득의 의미가 달라 ClaimDigestBatch와 분리했다.
// 획득은 아직 주기에 도달하지 않은 항목까지 채널의 모든 대기 행을 포함한다.
// 그렇지 않으면 한 주기가 여러 메시지로 나뉘어 요약의 의미가 사라진다.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch는 채널에서 현재 기한이 된 대기 전송을 요약 배치로 획득한다.
// 배치당 최대 MaxDigestBatchSize개다.
//
// 같은 배치는 최소 ID를 batch_id로 공유한다. 안정적이고 읽기 쉬우며 별도 시퀀스가 필요 없다.
// 재시도 시 COALESCE로 기존 배치 번호를 보존하여 여러 차례 재시도해도
// 함께 전송한 N개라는 관계를 유지한다.
//
// 무작위 대신 ID 오름차순의 앞 N개를 가져와 오래된 전송부터 처리한다.
// 새 취약점만 먼저 전송되어 오래된 항목이 계속 밀리는 기아를 방지한다.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit은 메모리 상한이다. 호출자는 MaxDigestBatchSize를 전달하며
	// 여기서도 더 큰 값이 들어오지 않도록 제한한다.
	//
	// 속도 제한 예산을 배치 크기로 사용하지 않는다. 제한 단위는 메시지 수이며
	// 배치는 메시지 하나와 토큰 하나를 소비하고 서버 takeTokens가 차감한다.
	// 배치에 담는 취약점 수와는 다른 단위다. 이전에 digest 속도 제한을 위해
	// 요청 예산을 배치 크기로 전달하자 분당 20개 채널도 배치당 취약점 하나만 담아
	// 요약 문구가 붙은 실시간 알림으로 변했다. 속도 제한은 takeTokens의 want를 수정하고
	// 여기는 바꾸지 않는다.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries는 선택 + sending 전환 및 임대 연장 + 전체 행 조회를 한 트랜잭션에서 처리한다.
// postClaim은 선택적 추가 단계로 요약 배치의 batch_id 저장에 사용한다.
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋 성공 후에는 no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending으로 바꾸고 next_attempt_at을 미래의 임대 만료 시점으로 옮긴다.
	// 임대 미만료와 재시도 시점 미도달을 같은 조건으로 표현하여 새 열이 필요 없다.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent는 여러 전송을 완료로 표시한다.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries는 여러 전송을 pending으로 돌리고 재시도 시간을 늦춘다.
//
// 새 중간 상태 대신 pending을 재사용하여 남은 기회를 MaxNotifyAttempts 한곳에서만
// 표현하고 재시도 정책 때문에 상태 분기가 늘어나지 않게 한다.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries는 전송을 즉시 다시 획득 가능한 pending으로 돌리고 획득 시 증가한 시도 수를 취소한다.
//
// 채널 길이 제한으로 요약 메시지를 나눌 때 이번 메시지에 담지 못한 항목을 다음 배치에 남기는 용도다.
// 실패가 아니므로 재시도 예산을 소모하면 안 된다. 획득 시 미리 증가한 attempts를
// 여기서 되돌리지 않으면 500개를 20개씩 25개 메시지로 나눌 때
// 뒤쪽 항목이 아무 오류 없이도 세 번째에 MaxNotifyAttempts로 failed가 된다.
//
// 수동 재전송으로 attempts가 0이 된 뒤 이 경로에 들어올 수 있으므로
// GREATEST(...,0)로 음수가 되지 않게 한다.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries는 최종 실패로 표시하여 전송 기록에서 수동 재전송을 기다리게 한다.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 자리표시자는 $3부터 시작한다. $1은 state, $2는 last_error다.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery는 수동 재전송을 위해 pending으로 바꾸고 시도 수를 0으로 초기화하여
// 즉시 기한이 되도록 한다. 사용자가 재전송을 누르면 이전 실패 원인을 해결했다는 뜻이므로
// 기존 시도 수로 제한하지 않는다.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("전송 %d가 없거나 현재 상태에서 재전송할 수 없습니다", id)
	}
	return nil
}

// NotificationDeliveryFilter는 전송 기록 조회 조건이다.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries는 전송 기록을 최신순으로 페이지별 반환한다.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError는 오류를 열이 수용할 길이로 자른다. 채널 응답이 길 수 있으며
// 특히 자체 서비스 Webhook에서 길이 제한이 없으면 기록 목록 페이로드가 커진다.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// UTF-8 문자 일부가 잘려 화면이 깨지지 않도록 문자 경계까지 되돌린다.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders는 IN (...)용으로 start부터 시작하는 $n 문자열과 대응 인자를 생성한다.
// 예: start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
