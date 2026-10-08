#!/bin/sh
# ARTEX 감시 실행 스크립트(Linux / macOS / Docker ENTRYPOINT)
#
# 사용법:
#   ./start.sh                         포그라운드 실행(Ctrl-C로 중지)
#   nohup ./start.sh >artex.log 2>&1 &   백그라운드 실행
#   ./start.sh -addr :9000             추가 인자를 artex에 그대로 전달
#
# artex를 실행하고 종료 코드에 따라 재시작 여부만 결정합니다.
#
#   0      사용자의 정상 종료 → 반복 종료
#   75     프로그램 재시작 요청 → 즉시 재실행(페이지의 원클릭 업데이트 또는 롤백)
#   기타   비정상 종료 → 백오프 후 재실행(1→2→4…최대 60초)
#
# 다운로드, SHA256 검증, 교체는 여기서 처리하지 않습니다. sh와 bat에 같은 로직을 두 번 작성해야 하며,
# 잘못된 바이너리로 교체하면 이 스크립트가 실행 불가능한 파일을 계속 재시작하여
# 사용자가 직접 복구해야 합니다. 검증과 교체는 모두 Go의 selfupdate 패키지에서 처리하고,
# artex 시작 시 완료하도록 하여 스크립트를 단순하게 유지합니다.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 실행 파일을 찾을 수 없습니다: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 종료 신호를 artex 프로세스에 전달합니다.
#
# Docker에서는 필수입니다. docker stop은 PID 1(이 스크립트)에만 SIGTERM을 보내고
# 자식 프로세스에는 보내지 않습니다. 전달하지 않으면 artex가 정상 종료하지 못하며 10초 후 SIGKILL로
# 강제 종료되어 실행 중인 작업이 중간에 끊깁니다.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# 신호는 wait를 중단시켜 128보다 큰 값을 반환합니다. 자식 프로세스는 아직 정상 종료 중이므로
	# 실제 종료 코드를 얻으려면 다시 wait해야 합니다.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 중지됨"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 정상 종료"
			exit 0
			;;
		"$RESTART_CODE")
			# 업데이트/롤백 준비 완료: 재실행 시 artex가 교체를 완료합니다(selfupdate.Bootstrap 참조).
			echo "[artex] 재시작 요청(새 버전 적용)…"
			delay=1
			;;
		*)
			echo "[artex] 비정상 종료(code=$code), ${delay}초 후 재시작" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
