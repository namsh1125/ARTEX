#!/usr/bin/env bash
# ARTEX 업데이트 스크립트: ① Docker(새 이미지로 재생성) ② 로컬 컴파일(바이너리 재빌드)
# install.sh는 최초 설치, update.sh는 새 버전 업그레이드를 담당합니다.
# artex가 시작할 때마다 ADD COLUMN/CREATE INDEX IF NOT EXISTS를 포함한 schema.sql을 멱등 실행하므로
# DB 마이그레이션은 재시작 시 자동 수행됩니다. pgdata 볼륨, ./data, ./skills는 유지됩니다.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── 선택 사항: 저장소를 최신 코드로 동기화(compose/스크립트/로컬 빌드 소스 갱신) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Git 작업 사본이 아니므로 git pull을 생략합니다"; return; }
  [ "$(ask '최신 코드를 가져올까요(git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull을 fast-forward할 수 없습니다(로컬 변경 또는 브랜치 분기). 직접 해결한 뒤 재시도하세요. 이번에는 현재 코드를 사용합니다"
  fi
}

# ── ① Docker 업데이트 ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose가 없습니다. 먼저 ./install.sh로 설치하세요"
  [ -f .env ] || die ".env가 없습니다. 먼저 ./install.sh로 최초 배포를 완료하세요"

  # 선택 사항: 대상 버전 태그 지정(비워 두면 .env의 ARTEX_TAG 사용, 기본값 latest)
  local tag; tag="$(ask '대상 이미지 태그(Enter로 .env / latest 유지)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG를 ${tag}로 설정했습니다"
  fi

  # artex만 갱신합니다. postgres는 16-alpine으로 고정되어 함께 갱신할 필요가 없으며,
  # 불필요한 다운로드와 메이저 버전 호환성 문제를 피합니다. artex는 postgres에 depends_on을 선언하므로
  # 서비스 이름으로 up하면 postgres가 없을 때 시작하고 실행 중이면 재생성하지 않습니다.
  info "새 이미지 내려받는 중(artex만)…"
  docker compose pull artex
  info "재생성 후 시작 중(artex 재시작 시 schema 자동 마이그레이션)…"
  docker compose up -d artex
  ok "업데이트 완료 → http://localhost:8787"
  info "로그 확인: docker compose logs -f artex"
  info "이전 이미지 정리(선택 사항): docker image prune -f"
}

# ── ② 로컬 컴파일 업데이트 ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go 1.26 이상이 필요합니다: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json이 없습니다. 최초 배포라면 ./install.sh를 사용하세요"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "프런트엔드 정적 산출물 재빌드 중…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "프런트엔드를 포함한 단일 바이너리 재컴파일 중…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm이 없어 프런트엔드를 포함하지 않는 백엔드를 컴파일합니다(프런트엔드는 npm run dev로 별도 실행)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "컴파일 완료 → ./artex"
  warn "실행 중인 artex를 재시작해야 적용됩니다(재시작 시 schema 자동 마이그레이션)"
}

echo "=============================="
echo "  ARTEX 업데이트"
echo "  1) Docker 업데이트(새 이미지로 재생성)"
echo "  2) 로컬 업데이트(Go 재컴파일)"
echo "=============================="
case "$(ask '선택' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "잘못된 선택입니다" ;;
esac
