"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 새 버전 실행까지 기다리는 최대 시간. 업그레이드 과정에서 세 번의 프로세스 시작이 필요함(임시 저장 → 교체 → 새 버전），
 *  일반적으로 수초가 걸리며 3분이면 느린 디스크와 Docker 재생성도 처리할 수 있습니다. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // 임시 저장 후 프로세스가 종료되어 SSE가 끊기므로 progress와 분리하여 /api/health 폴링으로 전환합니다.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet는 백엔드 캐시 사용도 결정합니다. 자동 확인은 캐시를 사용하고,
  // 수동 업데이트 확인은 원본을 조회해 방금 게시된 버전도 즉시 확인합니다.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("업데이트 확인 실패: " + r.error);
          else if (r.has_update) toast.success(`새 버전 발견 ${r.latest}`);
          else if (r.comparable) toast.success("이미 최신 버전입니다");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("업데이트 확인 실패: " + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // 버전 번호가 바뀔 때까지 /api/health를 폴링합니다.
  //
  // 교체 중 이전 버전이 artex.new를 설치하려고 잠시 재실행된 뒤 종료되므로
  // 연결 가능 여부가 아닌 버전 변경을 성공 기준으로 사용해야 합니다.
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`업데이트한 버전: ${j.version}, 페이지를 다시 불러오는 중`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 재시작 중 연결 실패는 정상이므로 계속 폴링합니다.
      }
    }
    setRestarting(false);
    toast.error("서비스 재시작 대기 시간이 초과되었습니다. 백엔드 로그와 ARTEX를 start.sh / start.bat로 시작했는지 확인하세요.");
  }, []);

  // 업데이트 진행률 SSE는 버퍼링을 피하기 위해 Next의 /api 재작성을 거치지 않습니다.
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("갱신 실패: " + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // 프로세스 종료 시 SSE 연결이 끊깁니다. 재시작 대기 상태라면 정상이며,
        // /api/health 폴링으로 계속 판단합니다.
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `다음 버전으로 업데이트하시겠습니까 ${info.latest}？\n\n` +
        "업데이트 시 프로그램이 재시작되며 실행 중인 작업이 중단됩니다.\n" +
        (info.mode === "docker"
          ? "\n참고: 컨테이너 내부 업데이트는 프로그램만 교체하며 이미지의 playwright / nmap 등 도구는 업데이트하지 않습니다; " +
            "새 버전에 새로운 도구가 필요하면 docker compose pull을 사용하세요."
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "준비 중…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("업데이트 시작 실패: " + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "이전 버전으로 롤백?\n\n프로그램이 재시작되며 실행 중인 작업이 중단됩니다.\n참고: 데이터베이스 구조는 되돌리지 않으므로 이전 버전에서 새 버전의 데이터를 인식하지 못할 수 있습니다.",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("이전 버전으로 전환하여 재시작 중입니다…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("롤백 실패: " + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 다운로드 단계만 Content-Length로 실제 백분율을 계산할 수 있습니다. 검증/압축 해제/재시작 대기는
  // 소요 시간을 알 수 없으므로 채워진 막대와 점멸 애니메이션으로 진행 중임을 표시합니다.
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 설정은 여러 열의 masonry 배치이므로 카드가 행 간격과 열 사이 분할 방지를 처리합니다(page.tsx 참조).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          버전 및 업데이트
        </CardTitle>
        <CardDescription>GitHub에서 새 버전을 확인하고 설치합니다. 프로그램이 재시작되며 실행 중인 작업이 중단됩니다.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">현재 버전</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "독립 실행 프로그램"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">최신 버전</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              변경 이력 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            연결할 수 없음 GitHub: {info.error}
            {"　"}위에서 전역 프록시를 설정한 후 다시 시도할 수 있습니다.
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} 제공되지 않음 {info.os}/{info.arch} 릴리스 패키지(누락된 항목: {info.asset}), 자동 업데이트 불가.
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            다운로드할 항목: <span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}후 SHA256 검증과 스모크 테스트를 통과하면 교체합니다. 실패하면 현재 버전을 유지합니다.
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            이미 최신 버전입니다.
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker 환경에서의 업데이트는 프로그램만 교체하며 이미지의 playwright / nmap 등 도구는 갱신하지 않습니다. 또한
            <span className="font-mono"> docker compose up -d </span>
            컨테이너를 다시 만들면 이미지에 포함된 버전으로 돌아갑니다. 이미지까지 업그레이드하려면 다음을 실행하세요
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "새 버전을 적용하고 재시작 중입니다. 잠시 기다려 주세요(페이지 자동 새로 고침)…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            업데이트 확인
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `업데이트할 버전: ${info.latest}` : "지금 업데이트"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              이전 버전으로 롤백
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          원클릭 업데이트는 감시 스크립트가 프로그램을 재시작해야 합니다. 다음으로 실행하세요 <span className="font-mono">start.sh</span>(Windows 의 경우
          <span className="font-mono"> start.bat</span>)로 ARTEX를 시작하세요. artex를 직접 실행하면 종료 후 자동으로 다시 시작하지 않습니다.
        </p>
      </CardContent>
    </Card>
  );
}
