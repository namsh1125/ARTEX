package server

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	authPassKey    = "auth.password_hash"
	jwtTTL         = 7 * 24 * time.Hour
	keyChars       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 최소 길이는 setup UI 검증과 같다. 프런트엔드에서만 검증하면 API 직접 호출로 우회할 수 있다.
	// 최대 길이는 bcrypt의 제한이다. 72바이트 초과 시 GenerateFromPassword가
	// ErrPasswordTooLong을 반환하므로 모호한 암호화 실패 대신 미리 차단한다.
	minPasswordRunes = 8
	maxPasswordBytes = 72
)

// errDataSourceUnavailable은 비밀번호 관련 읽기 실패의 공통 응답이다. 읽기 실패를
// 미설정으로 취급하면 안 된다. 이전 authInit은 DB 오류 시 인증되지 않은 요청을 허용하여
// 기존 관리자 비밀번호를 덮어쓸 수 있었다.
const errDataSourceUnavailable = "데이터 소스를 일시적으로 사용할 수 없습니다. 잠시 후 다시 시도하세요"

// validatePassword는 성공 시 빈 문자열, 실패 시 사용자에게 표시할 한국어 사유를 반환한다.
func validatePassword(pw string) string {
	if utf8.RuneCountInString(pw) < minPasswordRunes {
		return fmt.Sprintf("비밀번호는 최소 %d자여야 합니다", minPasswordRunes)
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Sprintf("비밀번호는 %d바이트를 초과할 수 없습니다", maxPasswordBytes)
	}
	return ""
}

// loadOrCreateJWTKey reads the 32-byte signing key from keyDir/jwt.key. keyDir is
// the project base dir (next to the executable), NOT the browsable workspace root
// (dataDir) — the signing key must never be listable/downloadable via the file
// manager. Legacy installs kept it at dataDir/jwt.key; if present there and not yet
// at the new location, it is migrated (key preserved, so sessions stay valid) and
// the old file removed so it disappears from the workspace. On first run a random
// key is generated and persisted.
func loadOrCreateJWTKey(keyDir, dataDir string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	// one-time migration out of the old in-workspace location.
	if legacy := filepath.Join(dataDir, jwtKeyFilename); legacy != path {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if data, rerr := os.ReadFile(legacy); rerr == nil {
				if werr := os.WriteFile(path, data, 0o600); werr == nil {
					_ = os.Remove(legacy)
					log.Printf("[auth] JWT 키를 %s에서 %s로 이전했습니다(탐색 가능한 작업 공간 밖으로 이동)", legacy, path)
				}
			}
		}
	}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate jwt key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key: %w", err)
	}
	log.Printf("[auth] 새 JWT 키를 %s에 저장했습니다", path)
	return buf, nil
}

// signJWT issues a 7-day HS256 token for user ARTEX.
func signJWT(key []byte) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "ARTEX",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}).SignedString(key)
}

// verifyJWT returns true when tokenStr is a valid, non-expired HS256 token.
func verifyJWT(tokenStr string, key []byte) bool {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return key, nil
	})
	return err == nil && t.Valid
}

// extractToken reads the JWT from Authorization: Bearer header,
// artex_token cookie, or ?token= query param (for SSE connections).
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("artex_token"); err == nil && c.Value != "" {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

// requireAuth wraps h with JWT validation.
// /api/auth/* and /api/health are exempt.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || p == "/api/health" {
			h.ServeHTTP(w, r)
			return
		}
		tok := extractToken(r)
		if tok == "" {
			writeErr(w, 401, "인증되지 않았습니다")
			return
		}
		if !verifyJWT(tok, s.jwtKey) {
			writeErr(w, 401, "토큰이 유효하지 않거나 만료되었습니다")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// GET /api/auth/status — reports whether the admin password has been initialised.
// 읽기 실패는 initialized:false 대신 503을 반환해야 한다. false이면 프런트엔드가
// 비밀번호 설정용 /setup으로 이동한다(login/page.tsx). DB 장애를 200으로 응답하면
// 사용자가 기존 비밀번호를 덮어쓰는 경로로 들어갈 수 있다.
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	hash, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	writeJSON(w, 200, map[string]any{"initialized": hash != ""})
}

// POST /api/auth/init — sets the password for the first time; rejected if already set.
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	existing, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if existing != "" {
		writeErr(w, 403, "비밀번호가 이미 설정되어 있습니다")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil || req.Password == "" {
		writeErr(w, 400, "비밀번호는 비워 둘 수 없습니다")
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "비밀번호 암호화 실패")
		return
	}
	// upsert 대신 INSERT ... ON CONFLICT DO NOTHING을 쓴다. 위 GetSetting은 빠른 실패용이고
	// 최초 설정만 허용하는 보장은 기본 키 제약에 있다. bcrypt 실행 수십 밀리초 사이에
	// 다른 요청이 먼저 설정할 수 있으며 읽기 검사 자체도 장애로 실패할 수 있다.
	inserted, err := pg.InsertSettingIfAbsent(authPassKey, string(hash))
	if err != nil {
		writeErr(w, 500, "저장 실패: "+err.Error())
		return
	}
	if !inserted {
		writeErr(w, 403, "비밀번호가 이미 설정되어 있습니다")
		return
	}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "토큰 생성 실패")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}

// POST /api/auth/change-password — changes the admin password. Requires a valid
// token (this route is under /api/auth/* which requireAuth exempts, so the token
// is validated here) AND the current password.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if !verifyJWT(extractToken(r), s.jwtKey) {
		writeErr(w, 401, "인증되지 않았습니다")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "잘못된 요청 형식")
		return
	}
	if req.NewPassword == "" {
		writeErr(w, 400, "새 비밀번호는 비워 둘 수 없습니다")
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "비밀번호가 초기화되지 않았습니다. 먼저 설정하세요")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		writeErr(w, 401, "현재 비밀번호가 올바르지 않습니다")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "비밀번호 암호화 실패")
		return
	}
	if err := pg.SetSetting(authPassKey, string(newHash)); err != nil {
		writeErr(w, 500, "저장 실패: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/login — validates username/password and returns a JWT.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "잘못된 요청 형식")
		return
	}
	if req.Username != "ARTEX" {
		writeErr(w, 401, "사용자 이름 또는 비밀번호가 올바르지 않습니다")
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "비밀번호가 초기화되지 않았습니다. 먼저 설정하세요")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeErr(w, 401, "사용자 이름 또는 비밀번호가 올바르지 않습니다")
		return
	}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "토큰 생성 실패")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}
