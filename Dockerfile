# syntax=docker/dockerfile:1
#
# 실행 이미지(이미지 안에서 컴파일하지 않음): 일반 도구를 설치하고 미리 빌드한 Linux 단일 바이너리를 배치합니다.
# CI의 binaries 작업에서 교차 컴파일한 바이너리(순수 Go, QEMU 미사용)를 대상 아키텍처에 맞게
# 빌드 컨텍스트의 dist/<TARGETARCH>/artex에 둡니다. 다중 아키텍처 빌드 시 arm64는 apt 계층만 에뮬레이션하며,
# Next/Go 컴파일은 에뮬레이션하지 않아 훨씬 빠릅니다.
#
# 로컬에서 이미지를 직접 빌드할 때는 먼저 바이너리를 준비하세요.
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# 일반 도구: ripgrep / curl / vim 및 자주 쓰는 정찰 도구(필요에 따라 추가·삭제).
# NodeSource에서 Node 20.x 설치. bookworm의 apt nodejs는 18이지만 Playwright는 20 이상이 필요합니다.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Playwright MCP와 CLI를 전역 설치하여 실행 시 npx로 다시 다운로드하지 않습니다.
# @playwright/mcp: 전역 설치되므로 browser MCP는 `npx @playwright/mcp`로 실행(-y/@latest 불필요).
# @playwright/cli: playwright-cli를 제공하며 설치 후 --help로 실행 가능 여부를 확인합니다.
# 브라우저 관리용 playwright도 설치하고 --with-deps로 chromium과 시스템 의존성을 준비합니다.
# 컨테이너에서 MCP/CLI를 처음 실행할 때 브라우저를 다시 다운로드할 필요가 없습니다.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# 해당 아키텍처용으로 미리 빌드한 바이너리(dist/amd64/artex 또는 dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# 감시 실행 스크립트: 종료 코드에 따라 재시작 여부를 결정하며 원클릭 업데이트의 교체를 완료합니다.
# SIGTERM을 artex에 전달합니다. docker stop은 PID 1에만 신호를 보내므로
# 전달하지 않으면 artex가 정상 종료하지 못하고 10초 후 SIGKILL로 강제 종료됩니다.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/(SQLite + jwt.key) 영구 저장 위치
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
