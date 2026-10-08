#!/usr/bin/env bash
# =============================================================================
# ARTEX 관리자 암호 초기화 스크립트
#
# 로그인 사용자 이름은 ARTEX로 고정되며 암호는 데이터베이스 settings 테이블의
# auth.password_hash 키에 bcrypt 해시로 저장됩니다. 데이터베이스에 연결하여 pgcrypto로 bcrypt 해시를 생성하고
# 해당 키에 기록합니다. 백엔드 로그인 검증(golang.org/x/crypto/bcrypt)과 완전히 호환됩니다.
#
# 두 가지 배포 방식:
#   local (기본값): 호스트에서 psql로 직접 연결. 연결 정보 우선순위:
#                  명령줄 인자 > --dsn/$ARTEX_PG_DSN > config.json의 database.*
#   docker: `docker compose exec` 또는 `docker exec`로 postgres 컨테이너 안에서
#           psql 실행(compose는 기본적으로 호스트에 5432를 노출하지 않음).
#
# 사용 예:
#   ./reset-password.sh                          # 로컬 설정 자동 조회, 새 암호 대화형 입력
#   ./reset-password.sh -p 'NewPass!'              # 로컬, 새 암호 직접 지정
#   ./reset-password.sh --dsn postgres://u:p@h:5432/artex
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U autopentest -W pass -d artex
#   ./reset-password.sh -m docker                 # Docker 배포(.env의 POSTGRES_* 사용)
#   ./reset-password.sh -m docker -c pg-container --exec docker
#
# 보안: 새 암호는 환경변수와 psql \getenv로 전달하여 프로세스 argv에 노출하지 않으며,
# :'var'로 자동 이스케이프하여 SQL 주입을 방지합니다. DB 암호도 PGPASSWORD로 전달합니다.
# =============================================================================
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker(비어 있으면 자동 판정)
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # Docker 모드의 postgres 서비스/컨테이너 이름(기본값 postgres)
EXEC_KIND=""       # compose | docker(Docker 모드의 exec 방식, 비어 있으면 자동)
NEWPASS=""
ASSUME_YES=0

die() { echo "오류: $*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- 인자 분석 -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "알 수 없는 인자: $1(-h로 사용법 확인)" ;;
  esac
done

# ---- config.json에서 database.* 읽기(local 모드에서 연결을 명시하지 않은 경우) -----
# 견고한 처리를 위해 python3를 우선 사용하며, 없으면 정형화된 개별 필드를 grep합니다.
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# 전체 DSN 또는 개별 필드 지원
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # 최소 대체 처리: 키별 grep(문자열 또는 숫자 값)
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- 실행 모드 자동 판정 ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${ARTEX_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "배포 모드: $MODE"

# ---- 새 암호 입력 -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "새 암호 입력(사용자 이름은 ARTEX로 고정): " NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "암호는 비어 있을 수 없습니다"
  read -r -s -p "확인을 위해 다시 입력하세요: " NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "입력한 암호가 일치하지 않습니다"
fi
[[ -n "$NEWPASS" ]] || die "암호는 비어 있을 수 없습니다"

# 환경변수로 암호를 psql에 전달(\getenv로 읽으며 argv/ps에는 노출하지 않음)
export ARTEX_RESET_NEWPASS="$NEWPASS"

# DB에서 bcrypt 생성 후 upsert. 암호는 :'newpw'로 자동 이스케이프하며 CREATE EXTENSION은 멱등입니다.
# DB 역할에 확장 생성 권한이 없으면 실패합니다(아래 run 실패 분기의 안내 참조).
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- 실행 -----------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # 연결 정보 우선순위: 명령줄 > --dsn/$ARTEX_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "$cfg에서 데이터베이스 설정을 읽습니다"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "psql을 찾을 수 없습니다(postgresql-client 설치 또는 -m docker 사용)"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "DB 사용자(-U) 또는 유효한 config.json/DSN이 필요합니다"
    [[ -n "$DBNAME" ]] || die "DB 이름(-d) 또는 유효한 config.json/DSN이 필요합니다"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "대상 데이터베이스: $target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "이 데이터베이스의 ARTEX 암호를 초기화할까요? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "취소되었습니다"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "저장 실패. pgcrypto 권한 또는 누락 오류라면 확장 생성 권한이 있는 역할을 사용하거나 CREATE EXTENSION pgcrypto를 먼저 실행하세요."
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "docker를 찾을 수 없습니다"
  CONTAINER="${CONTAINER:-postgres}"

  # exec 방식: docker compose exec(서비스 이름)를 우선하고 없으면 docker exec(컨테이너 이름) 사용
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # 컨테이너 psql 자격 증명: 명령줄 > .env의 POSTGRES_* > compose 기본값(artex)
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-artex}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-artex}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "대상: $CONTAINER 컨테이너의 psql -U $DUSER -d $DNAME(exec=$EXEC_KIND)"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "이 컨테이너 데이터베이스의 ARTEX 암호를 초기화할까요? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "취소되었습니다"
  fi

  # -e에 이름만 전달하면 현재 환경에서 상속하므로 Docker 명령 argv에 암호가 노출되지 않습니다.
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "저장 실패. 컨테이너 이름(-c), DB 계정(.env의 POSTGRES_*), 역할의 pgcrypto 권한을 확인하세요."
  fi
fi

unset ARTEX_RESET_NEWPASS
echo "✓ ARTEX 관리자 암호를 초기화했습니다. ARTEX와 새 암호로 로그인하세요(서비스 재시작 불필요)."
