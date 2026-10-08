"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * 상단 새 버전 알림: 전체 페이지 로드 시 한 번 확인하고 업데이트가 있으면 버전 옆에 표시합니다.
 * 클릭하면 시스템 설정의 버전 및 업데이트 카드로 이동합니다.
 *
 * 백엔드가 GitHub 조회 결과를 30분간 캐시하므로 마운트할 때마다 조회해도 안전합니다.
 * 미인증 GitHub API는 IP당 시간당 60회로 제한되므로 캐시가 없으면 탭 몇 개만으로
 * 할당량을 소진하여 실제 업데이트 시 조회할 수 없게 됩니다.
 *
 * 실패는 조용히 무시합니다. 오류 원인은 설정의 업데이트 확인에서 볼 수 있습니다.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update는 버전 비교 가능 여부도 포함하므로 개발 빌드에는 알림이 표시되지 않습니다.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 네트워크 단절이나 GitHub 요청 제한 오류를 상단에 표시하지 않습니다.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`새 버전 발견 ${latest}, 클릭하여 업데이트`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 상단 요소가 많아 텍스트만으로 놓치기 쉬우므로 점의 움직임으로 알립니다. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">새 버전 {latest}</span>
      <span className="sm:hidden">새 버전</span>
    </Link>
  );
}
