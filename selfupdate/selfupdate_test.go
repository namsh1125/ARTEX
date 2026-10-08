package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths는 격리된 업데이트 디렉터리를 만듭니다. ResolvePaths()를 직접 쓰면 테스트 실행 파일을
// 가리켜 go test 자체의 실행 파일 이름을 바꾸게 됩니다.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin은 artex를 흉내 내는 실행 가능한 셸 스크립트를 씁니다. smokeTest는 -h 실행의 종료 코드만
// 확인하므로 충분하며 실제 바이너리 컴파일보다 훨씬 빠릅니다.
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("모의 바이너리 %s 쓰기: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage는 artex.new와 체크섬을 써서 교체 대기 상태를 구성합니다.
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("체크섬 계산: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("체크섬 쓰기: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s 읽기: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("모의 바이너리가 sh 스크립트이므로 Windows에서는 실행할 수 없습니다")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh는 v를 제거하고 태그에는 v가 있으므로 둘 다 인식
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 사전순이 아닌 숫자 비교
		{"1.0.0", "0.99.99", 1, true},
		// 개발 빌드는 비교 불가로 처리해 정식 버전이 미커밋 변경을 덮어쓰지 않게 합니다.
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, 예상 %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, 예상 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 핵심 불변 조건: 모든 업데이트 파일은 실행 파일과 같은 디렉터리에 있어야 합니다.
	// CWD를 쓰면 작업 디렉터리가 /일 수 있는 서비스 실행에서 교체가 작동하지 않습니다.
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s가 실행 파일 디렉터리에 없습니다: %s (예상 %s)", name, path, p.Dir)
		}
	}
	// Windows의 .new/.old는 .exe를 유지해야 스모크 테스트 및 교체 후 실행이 가능합니다.
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows의 .new/.old는 .exe로 끝나야 합니다: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// 체크섬 기록 후 파일을 변경해 다운로드 손상/바꿔치기를 재현합니다.
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 불일치로 거부되어야 하지만 통과했습니다")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 실행 가능하지만 종료 코드는 0이 아님

	if err := verifyStaged(p); err == nil {
		t.Fatal("스모크 테스트 실패로 거부되어야 하지만 통과했습니다")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("마커 쓰기: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart 예상, 실제 %v", action)
	}
	if !st.Pending {
		t.Error("교체 후 상태는 Pending이어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex가 새 버전으로 교체되어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("이전 버전이 artex.old에 백업되어야 합니다")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("교체 후 artex.new가 없어야 합니다")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("교체 후 체크섬 파일을 정리해야 합니다")
	}
	// 마커를 남겨 새 버전의 다음 시작에서 횟수를 세고 필요하면 롤백합니다.
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("교체 후 업데이트 마커를 유지해야 합니다")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // 체크섬 손상

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("검증 실패 시 Continue 예상, 실제 %v", action)
	}
	if !st.FailedStage {
		t.Error("FailedStage 상태여야 합니다")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("검증 실패 시 현재 버전을 변경하면 안 됩니다")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("다음 시작에 다시 시도하지 않도록 검증 실패한 임시 파일을 제거해야 합니다")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 이전 업데이트의 백업
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3로 교체되어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("백업은 방금 교체한 v2여야 합니다")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// 처음 maxAttempts회는 횟수만 누적해 새 버전이 안정적으로 시작할 기회를 줍니다.
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d번째 시도: Continue 예상, 실제 %v", i, action)
		}
		if !st.Pending {
			t.Errorf("%d번째 시도 상태는 Pending이어야 합니다", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d번째 시도 후 attempts=%d(ok=%v), 예상 %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// 한 번 더 실패하면 한도를 넘으므로 이전 버전을 자동 복원합니다.
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("시도 한도 초과 시 Restart 예상, 실제 %v", action)
	}
	if !st.RolledBack {
		t.Error("RolledBack 상태여야 합니다")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("이전 버전으로 롤백되어야 합니다")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("무한 롤백을 막기 위해 롤백 후 마커를 제거해야 합니다")
	}
	// 시작 실패 버전은 삭제하지 않고 조사용으로 남깁니다.
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("실패 버전은 조사용 .failed로 보관해야 합니다")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback()은 ResolvePaths()를 사용하므로 여기서는 하위 교환 동작을 직접 테스트합니다.
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("롤백 후 현재 버전은 v1이어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("다시 되돌릴 수 있도록 롤백 후 백업은 v2여야 합니다")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum은 공백 두 개로 구분하며 shasum -a 256은 바이너리 모드에서 파일명 앞에 *를 붙입니다.
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // 필드가 두 개지만 첫 필드가 해시가 아님
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // 해시 길이 오류

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux 항목 해석 오류: %v", out)
	}
	// 해시는 소문자로 통일해 대소문자 때문에 불일치로 오판하지 않습니다.
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows 항목 오류(* 접두사 제거, 해시 소문자 변환 필요): %q", got)
	}
	if len(out) != 2 {
		t.Errorf("빈 줄, 해시 아닌 줄, 길이 오류 줄은 무시해야 합니다. 실제 %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows의 패키지 기본 이름은 artex.exe지만 이 테스트는 Unix 이름을 사용합니다")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 실제 릴리스 패키지 구조: artex-<버전>-<os>-<arch>/artex와 몇몇 무관한 파일.
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("추출한 파일이 artex 실행 파일이 아닙니다: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("추출한 바이너리에 실행 권한이 있어야 합니다")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("패키지에 실행 파일이 없으면 오류여야 합니다")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS가 아님
		"https://evil.com/artex.zip",    // 허용 목록 밖 도메인
		"https://github.com.evil.com/x", // 접미사 위장
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q)는 거부해야 합니다", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // 도메인은 대소문자 무시
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q)는 허용해야 하지만 오류 발생: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh의 package_binary는 artex-<버전>-<os>-<arch>.zip을 사용하고 v 접두사를 제거합니다.
	// 한 글자만 달라도 모든 플랫폼의 원클릭 업데이트에서 자산을 찾지 못합니다.
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q 해석: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("안정성 확인 후 업데이트 마커를 제거해야 합니다")
	}
	// 마커가 없으면 이후 정상 재시작에서 횟수 누적이나 잘못된 롤백이 발생하지 않습니다.
	if _, ok := readMarker(p.Marker); ok {
		t.Error("마커 읽기가 실패해야 합니다")
	}
	// 사용자가 수동 롤백할 수 있도록 백업은 유지합니다.
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("안정성 확인 후에도 이전 버전 백업을 유지해야 합니다")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 일반 시작 경로: panic이나 파일 변경이 없어야 함
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("마커가 없으면 settle은 파일에 영향을 주면 안 됩니다")
	}
}
