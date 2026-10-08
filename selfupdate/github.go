package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo는 릴리스 출처입니다. 설정 가능하게 하면 설정 수정 권한자에게 원격 코드 실행 경로를
// 제공하게 되므로 모의 침투 테스트 플랫폼에서는 코드에 고정합니다.
const Repo = "Autumn-27/artex"

// latestURL은 GitHub 최신 정식 릴리스 API이며 prerelease와 draft는 자동 제외합니다.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts는 업데이트 과정에서 접근할 도메인을 제한합니다. checkRedirect와 함께
// 목록 밖 호스트로 리디렉션되면 즉시 실패시켜 DNS 변조/중간자에 의한 바이너리 교체를
// 일차적으로 막습니다. 두 번째 검증은 SHA256SUMS 비교입니다.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // 릴리스 자산을 실제 저장하는 객체 스토리지
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release는 GitHub Release에서 사용하는 필드입니다.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset은 Release에 첨부된 파일 하나입니다.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient는 GitHub 도메인만 허용하는 HTTP 클라이언트를 만듭니다. proxy가 비면 직접 연결합니다.
//
// 기본 Transport는 재사용하지 않습니다. 업데이트는 TLS와 인증서 검증을 강제해야 하므로
// 다른 곳에서 설정한 InsecureSkipVerify 등의 영향을 받지 않게 합니다.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 전체 패키지 다운로드이므로 짧은 요청 시간 제한을 적용하지 않습니다.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("리디렉션 횟수가 너무 많습니다")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL은 HTTPS와 도메인 허용 목록을 강제합니다.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS가 아닌 주소 거부: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub가 아닌 도메인 거부: %s", u.Hostname())
	}
	return nil
}

// FetchLatest는 최신 정식 릴리스를 조회합니다.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 접근 실패(시스템 설정에서 전역 프록시를 설정할 수 있습니다): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 비인증 GitHub API는 IP당 시간당 60회 제한이며 송신 IP를 공유하면 쉽게 도달합니다.
		return nil, fmt.Errorf("GitHub API 요청 제한(시간당 60회)에 도달했습니다. 나중에 다시 시도하세요")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("저장소 %s에 아직 정식 릴리스가 없습니다", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub가 %d를 반환했습니다", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release 해석 실패: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release에 tag가 없습니다")
	}
	return &rel, nil
}

// AssetName은 build.sh의 package_binary와 같은 현재 플랫폼용 패키지 이름을 반환합니다.
// artex-<버전>-<os>-<arch>.zip(버전에 v 접두사 없음).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset은 Release에서 이름으로 자산을 찾습니다.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
