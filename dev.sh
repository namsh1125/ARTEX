#!/usr/bin/env bash
# 개발 모드: 백엔드(:8787), 트래픽 프록시(:8788), 프런트엔드 next dev(:5173)를 함께 실행합니다.
# 프런트엔드 /api는 백엔드로 역방향 프록시하며 Ctrl-C로 함께 종료합니다.
#
# 프런트엔드를 포함한 단일 바이너리는 README의 해당 절을 참고하세요. 이 스크립트는 사용하지 않습니다.
set -euo pipefail
cd "$(dirname "$0")"

# 종료 시 현재 프로세스 그룹의 모든 자식 프로세스(백엔드와 프런트엔드)를 종료합니다.
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# 백엔드(프런트엔드를 포함하지 않는 일반 go run). 동시 worker 수는 시스템 설정에서 지정합니다.
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# 프런트엔드 핫 리로드(Vite/Next 개발 서버, /api를 :8787로 역방향 프록시).
( cd web && npm run dev ) &

echo "[dev] 백엔드 :8787 / 프록시 :8788 / 프런트엔드 http://localhost:5173  (Ctrl-C로 종료)"
wait
