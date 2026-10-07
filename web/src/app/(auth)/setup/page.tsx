"use client";

import { useEffect, useState } from "react";

import { useRouter } from "next/navigation";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function SetupPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  // 查不到初始化状态时不能默认当成"未初始化"——那样会把初始化表单摆给一个
  // 其实已经设过密码的实例，用户照着填就会覆盖掉原密码。此时关闭入口。
  const [unavailable, setUnavailable] = useState(false);

  useEffect(() => {
    api
      .authStatus()
      .then(({ initialized }) => {
        if (initialized) router.replace("/login");
      })
      .catch((err) => {
        setError(err instanceof Error ? err.message : "백엔드 서비스에 연결할 수 없습니다");
        setUnavailable(true);
      })
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (password !== confirm) {
      setError("입력한 비밀번호가 서로 일치하지 않습니다");
      return;
    }
    if (password.length < 8) {
      setError("비밀번호는 8자 이상이어야 합니다");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.initPassword(password);
      auth.setToken(token);
      router.replace("/function/tasks");
    } catch (err) {
      setError(err instanceof Error ? err.message : "초기화 실패");
    } finally {
      setLoading(false);
    }
  }

  if (checking) return null;

  return (
    <div className="flex h-dvh">
      {/* Left panel */}
      <div className="hidden flex-col items-center justify-center bg-primary p-12 text-center lg:flex lg:w-1/3">
        <div className="relative flex items-center justify-center">
          <div className="absolute size-80 rounded-full border border-primary-foreground/10" />
          <div className="absolute size-60 rounded-full border border-primary-foreground/15" />
          <div className="absolute size-40 rounded-full border border-primary-foreground/20" />
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="ARTEX" width={160} height={160} className="relative brightness-0 invert" />
        </div>
      </div>

      {/* Right panel */}
      <div className="flex w-full items-center justify-center bg-background p-8 lg:w-2/3">
        <div className="w-full max-w-md space-y-10 py-24 lg:py-32">
          <div className="space-y-4 text-center">
            <h2 className="text-2xl font-medium tracking-tight">{unavailable ? "초기화 상태를 확인할 수 없습니다" : "비밀번호 초기 설정"}</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">
              {unavailable
                ? "백엔드 또는 데이터베이스를 일시적으로 사용할 수 없습니다. 기존 비밀번호를 덮어쓰지 않도록 초기 설정을 잠시 차단했습니다. 서비스를 복구한 후 다시 시도해 주세요."
                : "ARTEX를 처음 사용하려면 로그인 비밀번호를 설정해 주세요(최소 8자)"}
            </p>
          </div>
          {unavailable ? (
            <div className="flex flex-col gap-4">
              {error && <p className="text-center text-sm text-destructive">{error}</p>}
              <Button type="button" className="w-full" onClick={() => window.location.reload()}>
                다시 시도
              </Button>
            </div>
          ) : (
            <form onSubmit={handleSubmit} className="flex flex-col gap-4">
              <div className="space-y-1.5">
                <Label htmlFor="password">새 비밀번호</Label>
                <Input
                  id="password"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder="최소 8자"
                  autoFocus
                  autoComplete="new-password"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="confirm">비밀번호 확인</Label>
                <Input
                  id="confirm"
                  type="password"
                  value={confirm}
                  onChange={(e) => setConfirm(e.target.value)}
                  placeholder="비밀번호를 다시 입력하세요"
                  autoComplete="new-password"
                />
              </div>
              {error && <p className="text-sm text-destructive">{error}</p>}
              <Button type="submit" className="w-full" disabled={loading || !password || !confirm}>
                {loading ? "저장 중..." : "비밀번호 설정 후 로그인"}
              </Button>
            </form>
          )}
        </div>
      </div>
    </div>
  );
}
