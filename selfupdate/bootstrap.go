package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv는 스모크 테스트가 시작한 하위 프로세스에서 Bootstrap을 건너뛰게 합니다.
//
// 없어도 하위 프로세스의 os.Executable()이 artex.new라서 파생 경로에 모두 .new가 붙고
// 실제 업데이트 파일에 닿지 않지만 이런 우연에 의존하는 것은 취약합니다.
// 명시적으로 우회하면 의도가 분명하며 불필요한 디스크 검사도 줄입니다.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action은 Bootstrap이 main에 전달하는 지시입니다.
type Action int

const (
	// Continue: 정상적으로 server를 시작합니다.
	Continue Action = iota
	// Restart: 즉시 ExitRestart로 종료해 감시 스크립트가 다시 시작하도록 합니다.
	Restart
)

// State는 이번 시작의 업데이트 상태이며 /api/update/check가 이전 업데이트의 성공/롤백 여부를
// 프런트엔드에 정확히 알릴 때 사용합니다.
type State struct {
	Pending     bool   // 교체 후 안정성을 아직 확인하지 않음
	RolledBack  bool   // 이번 시작에서 자동 롤백 수행
	FailedStage bool   // 임시 파일 검증/스모크 테스트 실패로 폐기
	Detail      string // 사용자에게 표시할 한 문장 설명
}

// Bootstrap은 main 시작 부분에서 포트 수신이나 DB 열기 전에 호출해야 합니다.
//
// 세 가지 경우:
//
//	① artex.new 있음 → 검증+스모크 테스트 후 교체 및 재시작 요청, 실패하면 폐기하고 기존 버전 실행
//	② 마커 파일만 있음 → 교체 직후이므로 시도 횟수 누적, 연속 실패가 한도에 이르면 롤백
//	③ 둘 다 없음 → 정상 시작
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 부트스트랩 건너뜀: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged는 임시 파일이 있는 경우 검증 후 교체하고 실패하면 폐기합니다.
//
// 업데이트 과정에서 실행 파일을 덮어쓰는 유일한 곳이자 마지막 검증 지점입니다. 스모크 테스트로
// 다운로드 손상, 아키텍처 오류, 동적 링크 누락을 막습니다. 실행 불가능한 바이너리를 허용하면
// 감시 스크립트가 계속 재시작하지만 Go 코드 자체가 실행되지 않아 자동 롤백도 할 수 없습니다.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 임시 새 버전이 검증에 실패해 폐기했습니다. 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "새 버전 검증 실패로 폐기: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 교체 실패, 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "교체 실패: " + err.Error()}
	}

	// 교체 성공. 마커를 유지하고 새 버전으로 다음 시작 시 안정성을 확인합니다.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업데이트 마커 기록 실패(자동 롤백 불가): %v", err)
	}
	log.Printf("[update] %s(으)로 교체 완료, 재시작을 위해 종료합니다(exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback은 교체 후 시작 시도를 누적하고 한도를 넘으면 이전 버전을 복원합니다.
//
// Go 코드가 실행된 뒤에만 횟수가 증가하므로 실행은 가능하지만 초기화 중 실패하는 경우
// (설정 비호환, 포트 점유, DB 마이그레이션 실패)를 처리합니다. 실행 자체가 불가능한 경우는
// 교체 전 스모크 테스트가 막으며 두 검증이 함께 전체를 보호합니다.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// 롤백도 실패하면 무한 재시작을 피하기 위해 마커를 지우고 현재 상태로 시작합니다.
			// 시작하지 못하더라도 사용자가 로그에서 원인을 확인할 수 있습니다.
			log.Printf("[update] 새 버전이 연속 %d회 시작 실패했으며 롤백도 실패했습니다: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "새 버전 시작 및 롤백 실패: " + err.Error()}
		}
		log.Printf("[update] 새 버전이 연속 %d회 시작 실패해 %s(으)로 롤백했습니다. 재시작을 위해 종료합니다(exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("새 버전 시작 실패, %s(으)로 롤백했습니다", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업데이트 마커 갱신 실패: %v", err)
	}
	log.Printf("[update] 새 버전 시작 중(%d/%d회 시도), 안정적으로 실행되면 업데이트를 확정합니다",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle은 새 버전의 안정적인 실행을 확인하고 업데이트 마커를 제거합니다.
//
// main이 HTTP 수신 시작 후 지연 호출합니다. 그 시간 동안 살아 있어야 확정하며 그렇지 않으면
// 마커를 남겨 다음 시작에서 시도를 누적하고 한도에 도달하면 롤백합니다.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 업데이트 후 시작이 아니므로 할 일 없음
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 업데이트 마커 제거 실패: %v", err)
		return
	}
	log.Printf("[update] 새 버전이 안정적으로 실행되어 업데이트를 완료했습니다(이전 버전은 %s에 보관)", p.Old)
}

// SettleDelay는 새 버전의 안정적인 실행을 판정할 대기 시간입니다.
const SettleDelay = 30 * time.Second

// verifyStaged는 SHA256을 비교한 뒤 실제로 실행해 임시 파일을 검증합니다.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("체크섬 읽기: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("체크섬 계산: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 불일치(다운로드 손상 또는 변조)")
	}
	return smokeTest(p.New)
}

// smokeTest는 새 바이너리를 -h로 실행해 현재 시스템에서 실행 가능한지 확인합니다.
// 다운로드 잘림, 아키텍처 오류(exec format error), 의존성 누락 등을 막습니다.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("실행 권한 부여: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("스모크 테스트 시간 초과(새 바이너리 응답 없음)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("스모크 테스트 실패: %v: %s", err, snippet)
	}
	return nil
}

// swap은 현재 바이너리를 임시 새 버전으로 교체합니다.
//
// Unix와 Windows 모두 실행 중인 파일의 rename을 허용합니다(Windows는 삭제/덮어쓰기만 금지).
// 따라서 플랫폼별 분기나 자신의 사전 종료가 필요하지 않습니다.
func swap(p Paths) error {
	// Windows rename은 기존 대상을 덮어쓰지 않으므로 이전 업데이트의 .old를 먼저 제거합니다.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("이전 백업 %s 정리: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("현재 버전 백업: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 교체 실패 후 현재 버전이 이미 이동되었으므로 다음 시작의 실행 파일이 사라지지 않도록 복원합니다.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("새 버전 설치 실패(%v), 현재 버전 복원도 실패: %w", err, rerr)
		}
		return fmt.Errorf("새 버전 설치: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback은 swap이 백업한 이전 버전을 복원합니다.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("롤백 가능한 백업 %s 없음: %w", p.Old, err)
	}
	// 실행되지 않는 새 버전은 삭제하지 않고 .failed로 옮겨 조사에 사용합니다.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("실패한 버전 이동: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("이전 버전 복원: %w", err)
	}
	return nil
}

// Rollback은 /api/update/rollback에서 이전 버전으로 되돌리는 기능입니다.
// 파일만 교체하며 호출자가 ExitRestart로 종료하면 감시 스크립트가 재시작합니다.
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("롤백 가능한 이전 버전이 없습니다(" + p.Old + " 없음)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("이전 버전을 실행할 수 없어 롤백을 거부합니다: %w", err)
	}
	// 현재 버전과 백업을 교환해 롤백 후 다시 되돌릴 수 있게 합니다.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("현재 버전 이동: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("이전 버전 설치: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] 롤백 후 백업 정리 실패(실행에는 영향 없음): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup은 이전 버전 백업 유무를 반환해 프런트엔드가 롤백 버튼 표시를 결정하게 합니다.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "알 수 없는 버전"
	}
	return s
}
