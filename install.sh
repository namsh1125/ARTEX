#!/usr/bin/env bash
# ARTEX 설치 스크립트: ① 전체 Docker 설치 ② 로컬 컴파일 실행
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── Docker 환경 확인 / 자동 설치 ───────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker와 docker compose를 확인했습니다"; return
  fi
  warn "docker / docker compose를 찾을 수 없습니다"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Docker를 자동 설치할까요? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker 설치 완료(다시 로그인하면 변경된 그룹 권한으로 sudo 없이 사용 가능)"
      else
        die "docker를 직접 설치한 뒤 다시 시도하세요"
      fi ;;
    Darwin) die "macOS에서는 Docker Desktop을 설치하세요: https://www.docker.com/products/docker-desktop/" ;;
    *)      die "docker를 직접 설치한 뒤 다시 시도하세요" ;;
  esac
}

# ── ① 전체 Docker 설치 ───────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres 암호(Enter로 무작위 생성)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY(비워 두고 나중에 UI에서 설정 가능)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env 생성 완료(POSTGRES_PASSWORD 설정됨)"
  else
    info "기존 .env를 사용합니다"
  fi
  info "이미지를 내려받아 시작하는 중…"
  docker compose pull || true
  docker compose up -d
  ok "시작 완료 → http://localhost:8787"
  info "로그 확인: docker compose logs -f artex"
}

# ── ② 로컬 컴파일 실행 ──────────────────────────────
install_local(){
  echo "데이터베이스 설치 방식:"
  echo "  1) 기존 PostgreSQL 연결"
  echo "  2) Docker로 PostgreSQL 실행(docker 필요)"
  case "$(ask '선택' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres 암호(Enter로 무작위 생성)' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask '데이터베이스 주소' 127.0.0.1)"
      DB_PORT="$(ask '포트' 5432)"
      DB_USER="$(ask '계정' artex)"
      DB_PASS="$(ask '암호' '')"
      DB_NAME="$(ask '데이터베이스 이름' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # config.json 생성
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json 생성 완료"

  # Go 환경 확인
  command -v go >/dev/null 2>&1 || die "Go를 찾을 수 없습니다. 먼저 Go 1.26 이상을 설치하세요: https://go.dev/dl/"
  ok "Go: $(go version)"

  # 프런트엔드 임베드를 위한 정적 산출물 생성에 node가 필요합니다.
  if command -v npm >/dev/null 2>&1; then
    info "프런트엔드 정적 산출물 빌드 중…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "프런트엔드를 포함한 단일 바이너리 컴파일 중…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm을 찾을 수 없어 프런트엔드를 포함하지 않는 백엔드를 컴파일합니다(프런트엔드는 npm run dev로 별도 실행)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "컴파일 완료 → ./artex"

  info "시작 중…(Ctrl-C로 종료)"
  ./artex
}

echo "=============================="
echo "  ARTEX 설치"
echo "  1) 전체 Docker 설치"
echo "  2) 로컬 실행(Go 컴파일)"
echo "=============================="
case "$(ask '선택' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "잘못된 선택입니다" ;;
esac
