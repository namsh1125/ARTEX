package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout은 각각 연결과 SMTP 전체 세션 시간을 제한합니다.
// net/smtp에는 자체 시간 제한이 없어 멈춘 수신자가 전달 goroutine을
// 영구 대기시킬 수 있습니다. dispatcher는 단일 goroutine으로 순차 처리하므로
// 알림 시스템 전체가 멈추는 결과가 됩니다.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel은 SMTP 메일 전달을 구현합니다.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// 메일에는 플랫폼 제한이 없지만 과도한 전송을 막도록 여유 있는 기본값을 둡니다.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// 비밀번호만 마스킹합니다. SMTP 호스트, 계정, 수신자는 비밀이 아니며 가리면 편집만 불편해집니다.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port는 비밀번호를 받을 서버, tls는 암호화 여부를 결정합니다. 하나라도 바뀌면
// 비밀번호를 다시 명시해야 하므로 TLS를 끌 때도 자격 증명을 명시적으로 전달해야 합니다.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소가 없습니다")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트가 유효하지 않습니다(1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("발신자 주소가 없습니다")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("수신자 주소가 하나 이상 필요합니다")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: 서버가 지원하면 업그레이드합니다. 평문 세션에는 자격 증명을 보내면 안 됩니다(아래 auth 참조).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth는 localhost를 제외한 암호화되지 않은 연결로 자격 증명을 보내지 않습니다.
			// 올바른 보안 동작이므로 우회하지 않으며 사용자가 unencrypted connection만 보고
			// 혼란스러워하지 않도록 이유와 조치를 명확하게 안내합니다.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("자격 증명 전송 거부: 연결이 암호화되지 않았습니다. TLS를 켜거나 465 포트(암시적 TLS)를 사용하거나 TLS 활성화를 선택하세요 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("발신자 %s 거부", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("수신자 %s 거부", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("메일 본문 쓰기 실패: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("메일 제출 실패: %w", err)
	}
	// Quit 실패는 서버가 이미 메일을 받았다는 사실에 영향을 주지 않으므로 무시합니다.
	_ = client.Quit()
	// HTML 본문 전체를 보내고 길이로 자르지 않으므로 전체 묶음을 전달 완료로 간주합니다.
	return len(m.Items), nil
}

// emailDial은 SMTP 연결을 만듭니다.
//
// implicitTLS=true는 465처럼 연결 즉시 TLS, false는 25/587 평문 연결 후 STARTTLS입니다.
// 465에 평문 greeting을 보내면 끊어지므로 두 방식을 섞으면 안 됩니다.
//
// net/smtp.Client가 하위 연결을 비공개 필드에 숨기므로 세션 deadline은 나중이 아니라
// 연결 시점에 설정해야 합니다. 연결을 넘긴 뒤에는 미리 설정한 deadline에 의존하며
// 핸드셰이크 정체도 함께 제한합니다.
// Control에 blockInternalDial을 연결해 HTTP 채널과 같은 보호를 적용합니다. 없으면 SMTP가
// SSRF 보호의 빈틈이 되어 169.254.169.254나 127.0.0.1에 직접 연결할 수 있습니다.
// smtp.NewClient 핸드셰이크 오류가 서버 응답 한 줄을 포함하고 last_error와
// 전달 이력 API로 노출되면 부분 읽기 수단이 되며 연결 거부/시간 초과 차이로
// 포트도 탐지할 수 있습니다. 연결 단계가 최종 검증 지점이며 DNS 리바인딩도 막습니다.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버 연결 실패: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError는 SMTP 응답 코드로 재시도 가능/영구 실패를 구분합니다.
//
// SMTP 4xx와 5xx는 의미가 다릅니다.
//   - 4xx(450 그레이리스트, 451 로컬 오류, 452 저장 공간 부족)는 임시 거부이며
//     나중에 재시도해야 합니다. 특히 그레이리스트는 첫 전송에서 자주 발생합니다.
//   - 5xx(550 사용자 없음, 553 잘못된 주소)는 영구 거부이므로 재시도할 의미가 없습니다.
//
// 모두 영구 실패로 처리하면 그레이리스트 서버의 모든 알림이 첫 시도 후 failed가 됩니다.
// 이는 자동 재시도가 가장 필요한 상황입니다.
// 오류 텍스트 앞 세 자리에서 코드를 얻고 없으면 재시도 가능으로 처리합니다.
// 해석 실패만으로 일시적 오류를 영구 실패로 단정하기보다 한 번 더 시도합니다.
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode는 SMTP 오류 텍스트의 앞 세 자리 응답 코드를 반환하며 없으면 0입니다.
// net/smtp가 코드를 공개하지 않아 텍스트에서 추출합니다(형식: 450 4.7.1 ...).
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage는 완전한 RFC 5322 메일을 조립합니다.
//
// 본문 base64 인코딩의 두 이유: SMTP 한 줄 최대 1000바이트 제한에 긴 HTML,
// 특히 요약 메일이 걸리기 쉽고, base64에는 점으로 시작하는 줄이 없어
// SMTP 점 이스케이프 처리도 피할 수 있습니다.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 한국어 제목은 클라이언트에서 깨지지 않도록 RFC 2047로 인코딩합니다.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// 메일에는 강제 길이 한도가 없으므로 본문을 자르지 않습니다.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// RFC 2045에 따라 base64를 76자마다 줄바꿈합니다.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
