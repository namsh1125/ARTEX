package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent는 auth.password_hash의 최종 보호다. GetSetting 검사는 DB 오류나
// bcrypt 실행 수십 밀리초 사이의 동시 요청으로 무효화될 수 있으므로 최초 설정 보장은
// 상위 if문이 아닌 기본 키 제약에 두어야 한다.
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// 테스트 전용 키를 사용하고 개발 DB의 실제 auth.password_hash는 건드리지 않는다.
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("최초 저장은 inserted=true를 반환해야 함")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("기존 키는 inserted=false를 반환해야 함")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("값이 %q로 덮어써짐, %q를 유지해야 함", got, "first")
	}
}
