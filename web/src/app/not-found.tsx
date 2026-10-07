"use client";

import Link from "next/link";

import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <div className="flex h-dvh flex-col items-center justify-center space-y-2 text-center">
      <h1 className="font-semibold text-2xl">페이지를 찾을 수 없습니다.</h1>
      <p className="text-muted-foreground">요청한 페이지가 없거나 이동되었습니다.</p>
      <Link prefetch={false} replace href="/function/tasks">
        <Button variant="outline">홈으로 돌아가기</Button>
      </Link>
    </div>
  );
}
