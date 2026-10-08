package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset은 release.yml이 생성하며 Release의 모든 zip을 포함하는 체크섬 목록입니다.
const sumsAsset = "SHA256SUMS"

// maxBinarySize는 비정상 zip이 디스크를 채우지 않도록 추출 바이너리 크기를 제한합니다.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase는 업데이트 단계이며 SSE 이벤트의 phase 필드에 직접 사용합니다.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress는 호출자가 제공하는 프런트엔드 진행 콜백입니다. pct는 다운로드에서만 0-100을 사용하고
// 나머지 단계에서는 -1을 전달합니다.
type Progress func(ph Phase, pct int, msg string)

// Stage는 지정 Release의 현재 플랫폼 패키지를 받아 검증한 뒤 바이너리를 artex.new로 임시 저장합니다.
//
// 바이너리 대신 전체 zip을 쓰는 이유는 두 가지입니다. 기존 SHA256SUMS가 zip만 포함하므로
// CI 변경 없이 과거 릴리스와 호환되며 zip의 skills/를 향후 내장 스킬 동기화에 사용할 수 있습니다.
// 추가 다운로드 비용은 스킬의 수백 KB뿐입니다.
//
// 함수가 반환하면 임시 저장이 완료되며 호출자는 정상 종료 후 ExitRestart로 끝냅니다.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("이 버전에는 %s/%s용 패키지가 없습니다(%s 누락)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "체크섬 목록 가져오는 중…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s에 %s가 없어 검증되지 않은 바이너리 설치를 거부합니다", sumsAsset, name)
	}

	// 모든 임시 파일을 대상 디렉터리에 두어 마지막 rename이 동일 파일 시스템 내 원자적 연산이 되게 합니다.
	// 다른 장치 간 rename은 실패하고 /tmp는 별도 마운트인 경우가 많습니다.
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s(%s) 다운로드 중…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 검증 중…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 불일치: 예상 %s, 실제 %s(다운로드 손상 또는 변조)", short(want), short(got))
	}

	prog(PhaseExtract, -1, "압축 해제 및 스모크 테스트 중…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("새 버전이 현재 시스템에서 실행되지 않습니다: %w", err)
	}

	// 임시 파일 자체의 SHA256을 별도로 저장해 다음 시작의 교체 직전에 다시 확인합니다.
	// 임시 저장부터 재시작 사이의 파일 변경/손상을 방지합니다.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("새 바이너리 체크섬 계산: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("체크섬 쓰기: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("새 버전 임시 저장: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// 마커는 자동 롤백에만 영향을 주며 임시 파일은 준비되어 있으므로 업데이트를 중단하지 않습니다.
		prog(PhaseStaged, -1, "경고: 업데이트 마커 기록 실패로 이번 업데이트는 자동 롤백 보호가 없습니다")
	}

	prog(PhaseStaged, 100, "새 버전이 준비되어 재시작 중입니다…")
	return nil
}

// fetchSums는 SHA256SUMS를 다운로드/해석해 파일명 → 16진수 해시를 반환합니다.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("이 Release에 %s가 없어 무결성을 검증할 수 없으므로 업데이트를 거부합니다", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("%s 다운로드: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s 읽기: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s가 비어 있거나 형식을 인식할 수 없습니다", sumsAsset)
	}
	return out, nil
}

// parseSums는 sha256sum 형식 목록을 해석해 파일명 → 16진수 해시를 반환합니다.
//
// 첫 필드가 64자리 16진수일 때만 포함합니다. 필드가 두 개인지만 검사하면
// 두 단어짜리 설명도 정상 항목으로 인식해 잘못된 해시를 저장하고
// 실제 자산이 잘못된 해시에 일치할 수 있습니다.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 형식은 <sha256>  <filename>입니다(sha256sum은 공백 두 개,
		// shasum 바이너리 모드는 파일명 앞에 *를 붙임).
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download는 자산을 dst에 쓰며 SHA256을 계산하고 Content-Length 기준 진행률을 보고합니다.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s 다운로드: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("임시 파일 생성: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("다운로드 중단: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("디스크 저장 실패: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("불완전한 다운로드: 예상 %d바이트, 실제 %d바이트", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get은 허용 목록을 적용한 GET 요청을 보내 응답 본문을 반환합니다.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary는 릴리스 패키지에서 artex 실행 파일을 추출합니다.
//
// 구조는 artex-<버전>-<os>-<arch>/artex이지만 전체 경로 대신 기본 이름으로 찾습니다.
// 버전 문자열을 다시 조합하다 한 글자만 틀려도 업데이트가 실패하므로 기본 이름 검색이 변경에 강합니다.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("릴리스 패키지 열기: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s 읽기: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("새 바이너리 쓰기: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s 압축 해제: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("패키지 실행 파일이 %s를 초과해 압축 해제를 거부합니다", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("패키지의 %s가 빈 파일입니다", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("패키지에서 %s를 찾지 못했습니다", want)
}

// checkWritable은 디렉터리 쓰기 가능 여부를 미리 확인합니다. 그렇지 않으면 비root 실행이나
// 시스템 디렉터리의 바이너리가 수십 MB 다운로드 후 교체 시점에야 실패할 수 있습니다.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("프로그램 디렉터리 %s에 쓸 수 없어 자동 업데이트가 불가능합니다(권한을 확인하거나 수동 업데이트하세요): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter는 쓴 바이트를 세고 보고 빈도를 제한해 32KiB 블록마다 SSE를 보내지 않게 합니다.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("다운로드 중 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
