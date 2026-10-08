// Package selfupdate는 ARTEX 화면의 원클릭 업데이트를 구현합니다. GitHub Release에서 새
// 바이너리를 받아 검증/임시 저장하고 다음 시작 시 원자적으로 교체합니다.
//
// 역할 분담(start.sh / start.bat 참고):
//
//	시작 스크립트 = 프로세스 종료 코드로 재시작 여부만 결정하는 단순 감시 루프
//	이 패키지 = 다운로드 / SHA256 검증 / 스모크 테스트 / 교체 / 실패 롤백의 모든 복잡한 로직
//
// 교체를 스크립트 대신 Go에서 처리하는 이유는 sh와 bat의 SHA256 검증/스모크 테스트를
// 두 벌(sha256sum / shasum / certutil)로 작성해야 하기 때문입니다. 실행 불가 바이너리로 바뀌면
// 감시 프로세스가 계속 재시작해 사용자가 수동 복구해야 하므로 이 단계는 특히 정확해야 합니다.
//
// 전체 업데이트는 세 번의 프로세스 시작으로 이루어집니다.
//
//	① 이전 server가 /api/update/apply 수신 → 다운로드 검증 → artex.new 저장 → exit 75
//	② 스크립트가 이전 버전 재시작 → Bootstrap이 artex.new 발견 → 검증+스모크 → 교체 → exit 75
//	③ 새 버전으로 재시작 → Bootstrap이 시도 기록 → 시작 성공 후 마커 제거
//
// 어느 단계든 실패하면 이전 버전으로 돌아갑니다. ② 검증 실패 시 임시 파일을 지우고 이전 버전 실행,
// ③ 마커 제거 전까지 연속 3회 실패하면 artex.old를 자동 복원합니다.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart는 감시 프로세스에 재시작을 요청하는 종료 코드(EX_TEMPFAIL)입니다. 시작 스크립트는
// 충돌 백오프 없이 즉시 재시작합니다. 0은 정상 중지(루프 종료), 나머지는 충돌로 간주합니다.
const ExitRestart = 75

// maxAttempts는 교체 후 허용할 시작 시도 수입니다. 새 버전 시작마다 1 증가하고 settleDelay를
// 넘기면 마커를 제거합니다. 연속 maxAttempts회 실패하면 실행 불가로 판단해 자동 롤백합니다.
const maxAttempts = 3

// Paths는 업데이트 관련 모든 파일을 실행 파일 디렉터리 아래에 모읍니다.
// 서비스 실행 시 작업 디렉터리가 / 또는 임의 경로일 수 있으므로 CWD를 사용하지 않습니다.
// CWD를 쓰면 임시 파일이 다른 곳에 생겨 교체 로직이 작동하지 않습니다.
type Paths struct {
	Dir     string // 실행 파일 디렉터리
	Current string // 현재 바이너리        artex      / artex.exe
	New     string // 임시 새 버전          artex.new  / artex.new.exe
	Sum     string // 새 버전 SHA256(hex)   artex.new.sha256 / artex.new.exe.sha256
	Old     string // 교체 전 백업          artex.old  / artex.old.exe
	Marker  string // 업데이트 상태 마커    artex.upgrade.json
}

// ResolvePaths는 현재 실행 파일을 기준으로 업데이트 경로를 계산합니다.
//
// Windows의 .new/.old도 .exe 확장자가 있어야 스모크 테스트와 교체 후 실행이 가능하므로
// 먼저 확장자를 뗀 뒤 조합해 두 플랫폼의 이름 체계를 맞춥니다.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 확인: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows는 .exe, Unix는 보통 빈 값
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker는 교체 진행을 기록해 새 버전 시작 실패 시 자동 롤백을 유발합니다.
type marker struct {
	From     string `json:"from"`     // 업데이트 전 버전
	To       string `json:"to"`       // 대상 버전
	Attempts int    `json:"attempts"` // 교체 후 시작 시도 횟수
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged는 교체 성공, 검증 실패, 사용자 취소 시 임시 파일을 제거해 남은 artex.new가
// 다음 시작에 다시 시도되지 않게 합니다.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions는 두 버전을 비교해 -1/0/1(a<b / a==b / a>b)을 반환합니다.
// ok=false는 dev 또는 git describe의 0.3.7-2-gabc1234-dirty처럼 적어도 한쪽이
// 비교 가능한 버전이 아니라는 뜻입니다. 호출자는 원클릭 업데이트를 비활성화해
// 개발 빌드를 정식 버전으로 바꾸면서 미커밋 변경을 덮어쓰지 않도록 해야 합니다.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion은 v0.3.7 / 0.3.7 형식을 [3]int로 해석합니다.
//
// 순수한 세 부분 버전만 허용합니다. 태그 없는 빌드에서 build.sh의 git describe가 만드는
// 0.3.7-2-gabc1234 같은 접미 버전은 0.3.7로 간주하지 않고 비교 불가로 처리해야
// 개발 빌드를 최신으로 오판하거나 정식 버전으로 덮어쓰지 않습니다.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker는 컨테이너 실행 여부를 반환합니다. Docker에서 교체는 컨테이너 쓰기 계층에 적용되므로
// docker compose up -d로 컨테이너를 재생성하면 이미지의 버전으로 돌아갑니다. 사용자가 새 이미지를
// 가져오는 상황이므로 예상된 동작이지만 프런트엔드에서 명확히 안내해야 합니다.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
