package notify

// Snapshot은 notification_events.snapshot JSONB의 계약입니다. DB의 취약점 저장 트랜잭션이 쓰고
// server의 전달 엔진과 필터가 읽습니다. 알림 도메인의 페이로드이므로 이 패키지에 정의하며
// DB는 의미를 해석하지 않고 직렬화만 담당합니다.
//
// 렌더링 시 다시 조회하지 않고 필드를 복제하는 이유는 취약점 이름/등급/상태가 나중에 바뀌어도
// 알림은 이벤트 당시 결론을 반영해야 하기 때문입니다. 나중에 low로 바뀐 값을 읽으면
// 오해를 낳습니다. 또한 분배와 렌더링에서 findings/tasks/assets 세 테이블 JOIN이 필요 없습니다.
type Snapshot struct {
	// 이벤트 유형: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed일 때만 비어 있지 않습니다.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item은 채널이 렌더링할 취약점 알림 항목입니다.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets는 도메인/IP 등 해석된 자산 표시 이름이며 server가 채웁니다.
	// 이 패키지는 DB에 접근하지 않아 이름을 조회할 수 없습니다.
	Assets []string
	// DetailURL은 취약점 상세 링크이며 public_base_url이 없으면 비워 두고 표시하지 않습니다.
	DetailURL string
	// 상태 변경 전용: 둘 다 있으면 처리 대기 → 수정됨처럼 렌더링합니다.
	FromStatus string
	ToStatus   string
}

// IsStatusChange는 상태 변경 이벤트인지 반환합니다.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title은 사용자 지정 name, vulnclass 순서로 표시 제목을 선택하며
// 둘 다 없으면 예시 문구를 사용해 빈 제목을 내보내지 않습니다.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 취약점)"
}

// Message는 한 번의 채널 전송 전체 내용입니다.
type Message struct {
	// 단일 알림은 길이 1, 요약(digest)은 전체 묶음입니다.
	// 빈 슬라이스는 허용하지 않으며 호출자가 한 항목 이상을 보장해야 합니다.
	Items []Item
	// Batch=true이면 제목, 시간 구간, 개수를 포함한 요약 메시지로 렌더링합니다.
	Batch bool
	// WindowMinutes는 요약 주기(분)로 Batch=true일 때 최근 N분 안내에 사용합니다.
	// time.Since로 계산하지 않고 설정에서 받아 렌더링의 결정성을 유지합니다.
	WindowMinutes int
	// HomeURL은 플랫폼 대시보드 주소(전역 public_base_url)이며 비면 진입 링크를 생략합니다.
	HomeURL string
}
