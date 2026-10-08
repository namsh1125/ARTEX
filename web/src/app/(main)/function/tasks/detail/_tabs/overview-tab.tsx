"use client";

import * as React from "react";

import {
  ActivityIcon,
  AlertTriangleIcon,
  BugIcon,
  CheckIcon,
  ClockIcon,
  CoinsIcon,
  ListChecksIcon,
  PencilIcon,
  PlusIcon,
  RefreshCwIcon,
  ShieldAlertIcon,
  ShieldCheckIcon,
  TargetIcon,
  Trash2Icon,
  XIcon,
} from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { Progress } from "@/components/ui/progress";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type {
  AssetInterceptKind,
  AssetInterceptRule,
  Finding,
  ModelTokenStat,
  Stats,
  Task,
  TaskConstraint,
  TaskGoal,
  TaskNode,
  TaskScopeRow,
} from "@/lib/types";

// 토큰 수 축약 표시(12345 → 12.3k, 2000000 → 2M).
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(n >= 10000 ? 0 : 1) + "k";
  return String(n);
}

// 캐시 적중률 = 캐시 읽기 / 입력(InputTokens에 cache_read가 포함되므로 0–100%).
function cacheHitRate(cacheRead: number, input: number): string {
  if (input <= 0) return "—";
  return Math.round((cacheRead / input) * 100) + "%";
}

// 테스트 범위 항목 표시 값: 도메인 / 네트워크 대역 / 기업.
function scopeValue(row: TaskScopeRow): string {
  if (row.value) return row.value;
  if (row.domain) return row.domain;
  if (row.net) return row.net;
  if (row.company_id) return row.company_name?.trim() ? row.company_name : `기업 #${row.company_id}`;
  return "—";
}

const SCOPE_KIND_LABELS: Record<TaskScopeRow["kind"], string> = {
  company: "회사",
  root_domain: "루트 도메인",
  subdomain: "하위 도메인",
  ip: "IP",
  cidr: "네트워크 대역",
  icp: "ICP",
  keyword: "키워드",
};

const SCOPE_SOURCE_LABELS: Record<TaskScopeRow["source"], string> = {
  auto: "자동",
  agent: "Agent",
  manual: "수동",
};

function StatCard({
  label,
  value,
  sub,
  icon: Icon,
}: {
  label: string;
  value: React.ReactNode;
  sub?: string;
  icon: React.ElementType;
}) {
  return (
    <Card className="gap-1.5">
      <CardHeader className="pb-0">
        <CardDescription className="flex items-center gap-1.5">
          <Icon className="size-3.5" /> {label}
        </CardDescription>
        <CardTitle className="text-2xl tabular-nums">{value}</CardTitle>
      </CardHeader>
      {sub && <CardContent className="text-xs text-muted-foreground">{sub}</CardContent>}
    </Card>
  );
}

export function OverviewTab({ taskId }: { taskId: string }) {
  const [task, setTask] = React.useState<Task | null>(null);
  const [stats, setStats] = React.useState<Stats | null>(null);
  const [intents, setIntents] = React.useState<TaskNode[]>([]);
  const [findings, setFindings] = React.useState<Finding[]>([]);
  const [coverage, setCoverage] = React.useState<{
    enabled: boolean;
    scope_rows: number;
    denominator: number;
    tested: number;
    pct: number | null;
    by_type: { type: string; total: number; tested: number }[];
  } | null>(null);
  // 재실행 중인 의도 ID(__all__은 일괄 실행). 버튼 비활성화와 로딩 표시에 사용합니다.
  const [rerunning, setRerunning] = React.useState<Set<string>>(new Set());
  // 테스트 범위 목록과 추가 폼 상태.
  const [scope, setScope] = React.useState<TaskScopeRow[]>([]);
  const [scopeKind, setScopeKind] = React.useState<TaskScopeRow["kind"]>("root_domain");
  const [scopeValueInput, setScopeValueInput] = React.useState("");
  const [scopeBusy, setScopeBusy] = React.useState(false);
  const [scopeErr, setScopeErr] = React.useState("");
  // 항상 기록되는 llm_usage 원장의 호출별 정확한 모델별 토큰 사용량.
  const [modelTokens, setModelTokens] = React.useState<ModelTokenStat[]>([]);
  // 목표 관리: 목록 + 추가 폼 + 인라인 편집 상태.
  const [goals, setGoals] = React.useState<TaskGoal[]>([]);
  const [goalText, setGoalText] = React.useState("");
  const [goalVuln, setGoalVuln] = React.useState("");
  const [goalBusy, setGoalBusy] = React.useState(false);
  const [goalErr, setGoalErr] = React.useState("");
  const [editingGoalId, setEditingGoalId] = React.useState<string | null>(null);
  const [editText, setEditText] = React.useState("");
  const [editVuln, setEditVuln] = React.useState("");
  // 제약 관리: 목록 + 추가 폼 + 인라인 편집 상태.
  const [constraints, setConstraints] = React.useState<TaskConstraint[]>([]);
  const [conText, setConText] = React.useState("");
  const [conKind, setConKind] = React.useState<TaskConstraint["kind"]>("deny");
  const [conBusy, setConBusy] = React.useState(false);
  const [conErr, setConErr] = React.useState("");
  const [editingConId, setEditingConId] = React.useState<string | null>(null);
  const [editConText, setEditConText] = React.useState("");
  const [editConKind, setEditConKind] = React.useState<TaskConstraint["kind"]>("deny");

  const loadTokens = React.useCallback(async () => {
    try {
      const resp = await api.tokensByModel(taskId);
      setModelTokens(resp.models);
    } catch {
      // PG가 없으면 API 오류로 카드가 비므로 무시합니다.
    }
  }, [taskId]);

  const loadScope = React.useCallback(async () => {
    try {
      const resp = await api.taskScope(taskId);
      setScope(resp.scope);
    } catch {
      // asset store가 없으면 API가 503을 반환하고 범위 카드가 비므로 무시합니다.
    }
  }, [taskId]);

  const loadGoals = React.useCallback(async () => {
    try {
      const resp = await api.taskGoals(taskId);
      setGoals(resp.goals);
    } catch {
      // 일시적 오류는 다음 폴링에서 재시도합니다.
    }
  }, [taskId]);

  const addGoal = async () => {
    const text = goalText.trim();
    if (!text) return;
    setGoalBusy(true);
    setGoalErr("");
    try {
      await api.addGoal(taskId, text, goalVuln.trim() || undefined);
      setGoalText("");
      setGoalVuln("");
      await loadGoals();
    } catch (e) {
      setGoalErr(e instanceof Error ? e.message : "추가 실패");
    } finally {
      setGoalBusy(false);
    }
  };

  const startEditGoal = (g: TaskGoal) => {
    setEditingGoalId(g.id);
    setEditText(g.text);
    setEditVuln(g.vulnclass ?? "");
  };

  const cancelEditGoal = () => {
    setEditingGoalId(null);
    setEditText("");
    setEditVuln("");
  };

  const saveEditGoal = async (g: TaskGoal) => {
    const text = editText.trim();
    if (!text) return;
    setGoalBusy(true);
    setGoalErr("");
    try {
      await api.updateGoal(taskId, g.id, text, editVuln.trim() || undefined);
      cancelEditGoal();
      await loadGoals();
    } catch (e) {
      setGoalErr(e instanceof Error ? e.message : "저장 실패");
    } finally {
      setGoalBusy(false);
    }
  };

  const removeGoal = async (g: TaskGoal) => {
    setGoals((prev) => prev.filter((x) => x.id !== g.id));
    try {
      await api.deleteGoal(taskId, g.id);
    } catch {
      await loadGoals(); // 삭제 실패: 다시 불러와 복원
    }
  };

  const loadConstraints = React.useCallback(async () => {
    try {
      const resp = await api.taskConstraints(taskId);
      setConstraints(resp.constraints);
    } catch {
      // 일시적 오류는 다음 폴링에서 재시도합니다.
    }
  }, [taskId]);

  const addConstraint = async () => {
    const text = conText.trim();
    if (!text) return;
    setConBusy(true);
    setConErr("");
    try {
      await api.addConstraint(taskId, text, conKind);
      setConText("");
      await loadConstraints();
    } catch (e) {
      setConErr(e instanceof Error ? e.message : "추가 실패");
    } finally {
      setConBusy(false);
    }
  };

  const startEditConstraint = (c: TaskConstraint) => {
    setEditingConId(c.id);
    setEditConText(c.text);
    setEditConKind(c.kind);
  };

  const cancelEditConstraint = () => {
    setEditingConId(null);
    setEditConText("");
    setEditConKind("deny");
  };

  const saveEditConstraint = async (c: TaskConstraint) => {
    const text = editConText.trim();
    if (!text) return;
    setConBusy(true);
    setConErr("");
    try {
      await api.updateConstraint(taskId, c.id, text, editConKind);
      cancelEditConstraint();
      await loadConstraints();
    } catch (e) {
      setConErr(e instanceof Error ? e.message : "저장 실패");
    } finally {
      setConBusy(false);
    }
  };

  const removeConstraint = async (c: TaskConstraint) => {
    setConstraints((prev) => prev.filter((x) => x.id !== c.id));
    try {
      await api.deleteConstraint(taskId, c.id);
    } catch {
      await loadConstraints(); // 삭제 실패: 다시 불러와 복원
    }
  };

  const addScope = async () => {
    const value = scopeValueInput.trim();
    if (!value) return;
    setScopeBusy(true);
    setScopeErr("");
    try {
      await api.addTaskScope(taskId, scopeKind, value);
      setScopeValueInput("");
      await loadScope();
    } catch (e) {
      setScopeErr(e instanceof Error ? e.message : "추가 실패");
    } finally {
      setScopeBusy(false);
    }
  };

  const removeScope = async (row: TaskScopeRow) => {
    setScope((prev) => prev.filter((s) => s.id !== row.id));
    try {
      await api.deleteTaskScope(taskId, row.id);
    } catch {
      await loadScope(); // 삭제 실패: 다시 불러와 복원
    }
  };

  const markRerun = (key: string, on: boolean) =>
    setRerunning((prev) => {
      const next = new Set(prev);
      if (on) next.add(key);
      else next.delete(key);
      return next;
    });

  // 단일 재실행: open으로 되돌려 worker가 다시 가져가 처음부터 실행합니다(낙관적 갱신, 3초 폴링으로 보완).
  const rerunOne = async (id: string) => {
    markRerun(id, true);
    try {
      await api.rerunIntent(taskId, id);
      setIntents((prev) => prev.map((i) => (i.id === id ? { ...i, state: "open" } : i)));
    } catch {
      // 실패는 무시합니다. 다음 폴링에서 blocked가 표시되면 다시 클릭할 수 있습니다.
    } finally {
      markRerun(id, false);
    }
  };

  // 이 작업의 모든 blocked 의도를 일괄 재실행합니다.
  const rerunAll = async () => {
    markRerun("__all__", true);
    try {
      await api.rerunBlocked(taskId);
      setIntents((prev) => prev.map((i) => (i.state === "blocked" ? { ...i, state: "open" } : i)));
    } catch {
      // ignore
    } finally {
      markRerun("__all__", false);
    }
  };

  React.useEffect(() => {
    let cancelled = false;
    let loading = false;

    const load = async () => {
      if (loading) return;
      loading = true;
      try {
        const [taskResp, statsResp, intentsResp, findingsResp] = await Promise.all([
          api.task(taskId),
          api.stats(taskId),
          api.intents(taskId),
          api.findings(taskId),
        ]);
        if (cancelled) return;
        const activeTask = statsResp.active_task;
        setTask(
          activeTask
            ? {
                ...taskResp,
                in_flight: activeTask.in_flight,
                goals_total: activeTask.goals_total,
                goals_met: activeTask.goals_met,
                engine_mode: statsResp.engine_mode ?? activeTask.engine_mode,
                paused: activeTask.paused,
              }
            : taskResp,
        );
        setStats(statsResp);
        setIntents(intentsResp);
        setFindings(findingsResp);
        // coverage is independent + may 503 when no asset store — fetch separately so
        // its failure never blocks the others.
        api
          .taskCoverage(taskId)
          .then((c) => {
            if (!cancelled) setCoverage(c);
          })
          .catch(() => {
            // A transient coverage failure is retried by the next poll.
          });
      } catch {
        // transient errors are ignored; the next poll will retry
      } finally {
        loading = false;
      }
    };

    void load();
    void loadScope();
    void loadTokens();
    void loadGoals();
    void loadConstraints();
    const timer = setInterval(() => {
      void load();
      void loadTokens();
      void loadGoals();
      void loadConstraints();
    }, 3000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [taskId, loadScope, loadTokens, loadGoals, loadConstraints]);

  const running = intents.filter((i) => i.state === "running");
  const open = intents.filter((i) => i.state === "open");
  const blocked = intents.filter((i) => i.state === "blocked");
  const taskFindings = findings.filter((f) => f.task_id === taskId);
  const goalsPct = task?.goals_total ? Math.round(((task.goals_met ?? 0) / task.goals_total) * 100) : 0;
  // 카드 헤더용 전체 모델의 토큰 합계.
  const tokenTotals = modelTokens.reduce(
    (acc, m) => {
      acc.input += m.input_tokens;
      acc.output += m.output_tokens;
      acc.cacheRead += m.cache_read_tokens;
      acc.cacheWrite += m.cache_write_tokens;
      acc.calls += m.calls;
      return acc;
    },
    { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, calls: 0 },
  );

  return (
    <div className="flex flex-col gap-4">
      {/* 생성 시 입력한 원래 작업 설명과 목표를 위에 두어 언제든 다시 확인할 수 있게 합니다. */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <TargetIcon className="size-4 text-primary" /> 작업 설명 및 목표
          </CardTitle>
        </CardHeader>
        <CardContent className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <div className="text-xs font-medium text-muted-foreground">설명</div>
            <p className="text-sm whitespace-pre-wrap break-words">{task?.description?.trim() || "—"}</p>
          </div>
          <div className="flex flex-col gap-1.5">
            <div className="text-xs font-medium text-muted-foreground">목표</div>
            <p className="text-sm whitespace-pre-wrap break-words">{task?.goal?.trim() || "—"}</p>
          </div>
        </CardContent>
      </Card>
      {/* 이 작업의 탐색 목표 조회/추가/수정/삭제. 추가·수정 시 계획 담당자에게 알리고 작업을 다시 활성화하며,
          삭제 시 계획 담당자에게만 알림(재활성화하지 않음). 목표 = 최종 산출물/검증 가능한 결과이며 공격 단계나 정찰 동작이 아님。 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ListChecksIcon className="size-4 text-primary" /> 목표 관리
            <span className="text-muted-foreground text-xs font-normal">
              (최종 검증 가능한 목표, 총 {goals.length} 개. 추가/수정 시 계획 담당자에게 알리고 작업을 다시 활성화합니다)
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {/* 추가 폼 */}
          <div className="flex flex-wrap items-center gap-2">
            <Input
              className="h-7 min-w-56 flex-1 text-sm"
              placeholder="새 목표 예:『관리자 계정에 대한 권한 없는 접근 확인』"
              value={goalText}
              onChange={(e) => setGoalText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void addGoal();
              }}
              disabled={goalBusy}
            />
            <Input
              className="h-7 w-32 text-sm"
              placeholder="취약점 분류(선택 사항)"
              value={goalVuln}
              onChange={(e) => setGoalVuln(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void addGoal();
              }}
              disabled={goalBusy}
            />
            <Button size="sm" variant="outline" disabled={goalBusy || !goalText.trim()} onClick={() => void addGoal()}>
              <PlusIcon className="size-3.5" /> 추가
            </Button>
            {goalErr && <span className="text-xs text-red-500">{goalErr}</span>}
          </div>
          {/* 목표 목록 */}
          {goals.length > 0 ? (
            <div className="flex flex-col gap-1.5">
              {goals.map((g) =>
                editingGoalId === g.id ? (
                  <div key={g.id} className="flex flex-wrap items-center gap-2 rounded-md border px-2.5 py-1.5">
                    <Input
                      className="h-7 min-w-56 flex-1 text-sm"
                      value={editText}
                      onChange={(e) => setEditText(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") void saveEditGoal(g);
                        if (e.key === "Escape") cancelEditGoal();
                      }}
                      disabled={goalBusy}
                      autoFocus
                    />
                    <Input
                      className="h-7 w-32 text-sm"
                      placeholder="취약점 분류(선택 사항)"
                      value={editVuln}
                      onChange={(e) => setEditVuln(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") void saveEditGoal(g);
                        if (e.key === "Escape") cancelEditGoal();
                      }}
                      disabled={goalBusy}
                    />
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={goalBusy || !editText.trim()}
                      onClick={() => void saveEditGoal(g)}
                    >
                      <CheckIcon className="size-3.5 text-emerald-500" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={goalBusy}
                      onClick={cancelEditGoal}
                    >
                      <XIcon className="size-3.5" />
                    </Button>
                  </div>
                ) : (
                  <div key={g.id} className="flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-sm">
                    <StatusBadge domain="goal" value={g.state} />
                    <span className="min-w-0 flex-1 break-words">{g.text}</span>
                    {g.vulnclass && (
                      <span className="bg-muted text-muted-foreground shrink-0 rounded px-1.5 py-0.5 text-xs">
                        {g.vulnclass}
                      </span>
                    )}
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={goalBusy}
                      onClick={() => startEditGoal(g)}
                    >
                      <PencilIcon className="size-3.5" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={goalBusy}
                      onClick={() => void removeGoal(g)}
                    >
                      <Trash2Icon className="size-3.5 text-red-500" />
                    </Button>
                  </div>
                ),
              )}
            </div>
          ) : (
            <p className="text-muted-foreground text-sm">목표가 없습니다. 추가하면 계획 담당자가 탐색 의도를 배정하고 달성 여부를 판단합니다.</p>
          )}
        </CardContent>
      </Card>
      {/* 작업 제약 관리: allow=허용, deny=금지. 다음 계획 주기부터 planner/worker의 시스템
          탐색 경계를 지정하는 안내(시스템 설정에서 주입 범위를 다음 기준으로 planner/worker 전환 가능). 변경 시 즉시 중단하지 않음，
          다음 계획 수립에서 반영。 */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldAlertIcon className="size-4 text-amber-500" /> 작업 제약
            <span className="text-muted-foreground text-xs font-normal">
              (경계 지정 대상: planner/worker 탐색 경계, 총 {constraints.length} 개. 변경 사항은 다음 계획 수립부터 적용됩니다)
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {/* 추가 폼 */}
          <div className="flex flex-wrap items-center gap-2">
            <NativeSelect
              size="sm"
              value={conKind}
              onChange={(e) => setConKind(e.target.value as TaskConstraint["kind"])}
            >
              <NativeSelectOption value="deny">금지</NativeSelectOption>
              <NativeSelectOption value="allow">허용</NativeSelectOption>
            </NativeSelect>
            <Input
              className="h-7 min-w-56 flex-1 text-sm"
              placeholder="작업 제약 예:『현재 포트만 테스트하고 다른 포트는 스캔하지 않음』"
              value={conText}
              onChange={(e) => setConText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void addConstraint();
              }}
              disabled={conBusy}
            />
            <Button
              size="sm"
              variant="outline"
              disabled={conBusy || !conText.trim()}
              onClick={() => void addConstraint()}
            >
              <PlusIcon className="size-3.5" /> 추가
            </Button>
            {conErr && <span className="text-xs text-red-500">{conErr}</span>}
          </div>
          {/* 제약 목록 */}
          {constraints.length > 0 ? (
            <div className="flex flex-col gap-1.5">
              {constraints.map((c) =>
                editingConId === c.id ? (
                  <div key={c.id} className="flex flex-wrap items-center gap-2 rounded-md border px-2.5 py-1.5">
                    <NativeSelect
                      size="sm"
                      value={editConKind}
                      onChange={(e) => setEditConKind(e.target.value as TaskConstraint["kind"])}
                    >
                      <NativeSelectOption value="deny">금지</NativeSelectOption>
                      <NativeSelectOption value="allow">허용</NativeSelectOption>
                    </NativeSelect>
                    <Input
                      className="h-7 min-w-56 flex-1 text-sm"
                      value={editConText}
                      onChange={(e) => setEditConText(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") void saveEditConstraint(c);
                        if (e.key === "Escape") cancelEditConstraint();
                      }}
                      disabled={conBusy}
                      autoFocus
                    />
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={conBusy || !editConText.trim()}
                      onClick={() => void saveEditConstraint(c)}
                    >
                      <CheckIcon className="size-3.5 text-emerald-500" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={conBusy}
                      onClick={cancelEditConstraint}
                    >
                      <XIcon className="size-3.5" />
                    </Button>
                  </div>
                ) : (
                  <div key={c.id} className="flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-sm">
                    <span
                      className={`shrink-0 rounded px-1.5 py-0.5 text-xs ${
                        c.kind === "allow"
                          ? "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
                          : "bg-red-500/15 text-red-600 dark:text-red-400"
                      }`}
                    >
                      {c.kind === "allow" ? "허용" : "금지"}
                    </span>
                    <span className="min-w-0 flex-1 break-words">{c.text}</span>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={conBusy}
                      onClick={() => startEditConstraint(c)}
                    >
                      <PencilIcon className="size-3.5" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      disabled={conBusy}
                      onClick={() => void removeConstraint(c)}
                    >
                      <Trash2Icon className="size-3.5 text-red-500" />
                    </Button>
                  </div>
                ),
              )}
            </div>
          ) : (
            <p className="text-muted-foreground text-sm">
              작업 제약이 없습니다. 작업 생성 시 설명과 목표에서 자동 추출하며, 여기서 허용·금지할 작업을 직접 추가하거나 수정·삭제할 수 있습니다.
            </p>
          )}
        </CardContent>
      </Card>
      <TaskInterceptRulesCard taskId={taskId} />
      {coverage && coverage.enabled && coverage.scope_rows > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <TargetIcon className="size-4 text-emerald-500" /> 자산 테스트 커버리지
              <span className="text-muted-foreground text-xs font-normal">(대략적인 추정치이며 참고용입니다)</span>
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-baseline gap-3">
              <span className="text-2xl font-semibold tabular-nums">
                {coverage.pct != null ? Math.round(coverage.pct * 100) + "%" : "—"}
              </span>
              <span className="text-muted-foreground text-sm">
                테스트 완료 {coverage.tested} / 범위 내 {coverage.denominator}
              </span>
            </div>
            {coverage.pct != null && <Progress value={Math.round(coverage.pct * 100)} />}
            {coverage.by_type.length > 0 && (
              <div className="flex flex-wrap gap-1.5 text-xs">
                {coverage.by_type.map((b) => (
                  <span key={b.type} className="bg-muted rounded px-1.5 py-0.5">
                    <span className="text-muted-foreground">{b.type}</span>{" "}
                    <span className="tabular-nums font-medium">
                      {b.tested}/{b.total}
                    </span>
                  </span>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      )}
      {/* 모델별 LLM 토큰 사용량. llm_records에서 조회하며 LLM 기록 활성화가 필요합니다. */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <CoinsIcon className="size-4 text-amber-500" /> LLM Token 사용량
            <span className="text-muted-foreground text-xs font-normal">
              (모델별 통계{tokenTotals.calls > 0 ? `, 총 ${tokenTotals.calls} 회 호출` : ""}）
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {modelTokens.length > 0 ? (
            <>
              {/* 전체 합계 */}
              <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
                <span className="tabular-nums">
                  <span className="text-muted-foreground">입력 </span>
                  <span className="font-semibold">{fmtTokens(tokenTotals.input)}</span>
                </span>
                <span className="tabular-nums">
                  <span className="text-muted-foreground">출력 </span>
                  <span className="font-semibold">{fmtTokens(tokenTotals.output)}</span>
                </span>
                <span className="tabular-nums">
                  <span className="text-muted-foreground">캐시 읽기 </span>
                  <span className="font-semibold">{fmtTokens(tokenTotals.cacheRead)}</span>
                </span>
                <span className="tabular-nums">
                  <span className="text-muted-foreground">캐시 적중률 </span>
                  <span className="font-semibold text-emerald-500">
                    {cacheHitRate(tokenTotals.cacheRead, tokenTotals.input)}
                  </span>
                </span>
              </div>
              {/* 모델별 상세 표 */}
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="text-muted-foreground border-b text-left text-xs">
                      <th className="py-1.5 pr-3 font-medium">모델</th>
                      <th className="py-1.5 pr-3 text-right font-medium">호출</th>
                      <th className="py-1.5 pr-3 text-right font-medium">입력</th>
                      <th className="py-1.5 pr-3 text-right font-medium">출력</th>
                      <th className="py-1.5 pr-3 text-right font-medium">캐시 읽기</th>
                      <th className="py-1.5 text-right font-medium">적중률</th>
                    </tr>
                  </thead>
                  <tbody>
                    {modelTokens.map((m) => (
                      <tr key={m.model} className="border-b last:border-0">
                        <td className="max-w-[16rem] truncate py-1.5 pr-3 font-mono text-xs" title={m.model}>
                          {m.model}
                        </td>
                        <td className="py-1.5 pr-3 text-right tabular-nums">{m.calls}</td>
                        <td className="py-1.5 pr-3 text-right tabular-nums">{fmtTokens(m.input_tokens)}</td>
                        <td className="py-1.5 pr-3 text-right tabular-nums">{fmtTokens(m.output_tokens)}</td>
                        <td className="py-1.5 pr-3 text-right tabular-nums">{fmtTokens(m.cache_read_tokens)}</td>
                        <td className="py-1.5 text-right tabular-nums text-emerald-500">
                          {cacheHitRate(m.cache_read_tokens, m.input_tokens)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          ) : (
            <p className="text-muted-foreground text-sm">없음 LLM 사용량(아직 호출이 없거나 기록을 저장하는 중).</p>
          )}
        </CardContent>
      </Card>
      {/* 테스트 범위: 커버리지 분모와 승인 경계. 직접 추가·삭제할 수 있습니다. */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldCheckIcon className="size-4 text-emerald-500" /> 테스트 범위
            <span className="text-muted-foreground text-xs font-normal">
              (커버리지 분모 + 허용 범위, 총 {scope.length} 건)
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {/* 추가 폼 */}
          <div className="flex flex-wrap items-center gap-2">
            <NativeSelect
              size="sm"
              value={scopeKind}
              onChange={(e) => setScopeKind(e.target.value as TaskScopeRow["kind"])}
            >
              <NativeSelectOption value="root_domain">루트 도메인</NativeSelectOption>
              <NativeSelectOption value="subdomain">하위 도메인</NativeSelectOption>
              <NativeSelectOption value="ip">IP</NativeSelectOption>
              <NativeSelectOption value="cidr">네트워크 대역</NativeSelectOption>
              <NativeSelectOption value="icp">ICP</NativeSelectOption>
              <NativeSelectOption value="keyword">키워드</NativeSelectOption>
              <NativeSelectOption value="company">회사</NativeSelectOption>
            </NativeSelect>
            <Input
              className="h-7 w-56 text-sm"
              placeholder={
                scopeKind === "company"
                  ? "회사 이름 또는 id"
                  : scopeKind === "ip" || scopeKind === "cidr"
                    ? "예: 10.0.0.1 또는 10.0.0.0/24"
                    : scopeKind === "icp"
                      ? "예: 京ICP备12345678号-1"
                      : scopeKind === "keyword"
                        ? "예: 기업 이름 키워드"
                        : "예: example.com"
              }
              value={scopeValueInput}
              onChange={(e) => setScopeValueInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void addScope();
              }}
              disabled={scopeBusy}
            />
            <Button
              size="sm"
              variant="outline"
              disabled={scopeBusy || !scopeValueInput.trim()}
              onClick={() => void addScope()}
            >
              <PlusIcon className="size-3.5" /> 추가
            </Button>
            {scopeErr && <span className="text-xs text-red-500">{scopeErr}</span>}
          </div>
          {/* 범위 목록 */}
          {scope.length > 0 ? (
            <div className="flex flex-col gap-1.5">
              {scope.map((row) => (
                <div key={row.id} className="flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-sm">
                  <span className="bg-muted text-muted-foreground shrink-0 rounded px-1.5 py-0.5 text-xs">
                    {SCOPE_KIND_LABELS[row.kind]}
                  </span>
                  <span className="min-w-0 flex-1 truncate font-mono text-xs">{scopeValue(row)}</span>
                  <span className="text-muted-foreground shrink-0 text-xs">{SCOPE_SOURCE_LABELS[row.source]}</span>
                  {row.task_id.toString() === taskId ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 shrink-0 px-1.5"
                      onClick={() => void removeScope(row)}
                    >
                      <Trash2Icon className="size-3.5 text-red-500" />
                    </Button>
                  ) : (
                    <span className="text-muted-foreground shrink-0 text-xs">상속</span>
                  )}
                </div>
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground text-sm">테스트 범위 없음. 추가하면 자산 커버리지의 분모로 사용할 수 있습니다.</p>
          )}
        </CardContent>
      </Card>
      {/* Heartbeat */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ActivityIcon className="size-4 text-blue-500" /> 하트비트
          </CardTitle>
        </CardHeader>
        <CardContent className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          <div>
            <div className="text-xs text-muted-foreground">엔진 상태</div>
            <StatusBadge
              domain="engine"
              value={stats?.engine_mode ?? task?.engine_mode ?? "idle"}
              dot
              className="mt-1"
            />
          </div>
          <div>
            <div className="text-xs text-muted-foreground">실행 중 Worker</div>
            <div className="mt-1 text-lg font-semibold tabular-nums">{running.length}</div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">최근 활동</div>
            <div className="mt-1 inline-flex items-center gap-1 text-sm">
              <ClockIcon className="size-3.5" />
              {task?.last_activity ? new Date(task.last_activity).toLocaleTimeString("ko-KR") : "—"}
            </div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">
              목표 {task?.goals_met ?? 0}/{task?.goals_total ?? 0}
            </div>
            <Progress value={goalsPct} className="mt-2" />
          </div>
          {task?.completed_unix && task.completed_unix > 0 ? (
            <div>
              <div className="text-xs text-muted-foreground">완료 시각</div>
              <div className="mt-1 inline-flex items-center gap-1 text-sm">
                <ClockIcon className="size-3.5" />
                {new Date(task.completed_unix * 1000).toLocaleString("ko-KR")}
              </div>
            </div>
          ) : null}
        </CardContent>
      </Card>

      {/* Work set */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm">
              <TargetIcon className="size-4" /> 진행 중인 의도
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            {running.slice(0, 6).map((i) => (
              <div key={i.id} className="flex items-center gap-2 text-sm">
                <StatusBadge domain="intent" value={i.state} />
                <span className="min-w-0 flex-1 truncate">{i.payload}</span>
              </div>
            ))}
            {running.length === 0 && <p className="text-sm text-muted-foreground">진행 중인 의도 없음</p>}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm">
              <AlertTriangleIcon className="size-4 text-amber-500" /> 확인 필요
            </CardTitle>
          </CardHeader>
          <CardContent className="grid grid-cols-2 gap-3 text-sm">
            <div>
              <div className="text-2xl font-semibold tabular-nums text-red-600">{taskFindings.length}</div>
              <div className="text-xs text-muted-foreground">확인된 취약점</div>
            </div>
            <div>
              <div className="text-2xl font-semibold tabular-nums text-blue-600">{running.length}</div>
              <div className="text-xs text-muted-foreground">실행 중</div>
            </div>
            <div>
              <div className="text-2xl font-semibold tabular-nums">{open.length}</div>
              <div className="text-xs text-muted-foreground">frontier 할당 대기</div>
            </div>
            <div>
              <div className="text-2xl font-semibold tabular-nums text-red-600">{blocked.length}</div>
              <div className="text-xs text-muted-foreground">차단된 의도</div>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm">
              <BugIcon className="size-4 text-red-500" /> 최근 발견 사항
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            {taskFindings.slice(0, 6).map((f) => (
              <div key={f.id} className="flex items-center gap-2 text-sm">
                <StatusBadge domain="severity" value={f.severity} dot />
                <span className="min-w-0 flex-1 truncate">{f.summary}</span>
              </div>
            ))}
            {taskFindings.length === 0 && <p className="text-sm text-muted-foreground">발견 사항 없음</p>}
          </CardContent>
        </Card>
      </div>

      {/* Blocked intents: 오류/차단(예: LLM 네트워크 문제)된 의도를 원클릭으로 open으로 되돌려 재실행하며,
          worker 다시 할당하여 처음부터 실행합니다(그래프에 저장한 데이터 유지). 작업이 종료 상태이거나/일시 중지 상태이면 자동으로 다시 활성화합니다。 */}
      {blocked.length > 0 && (
        <Card className="border-red-500/30">
          <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
            <CardTitle className="flex items-center gap-2 text-sm">
              <AlertTriangleIcon className="size-4 text-red-500" /> 차단됨/오류가 발생한 의도
              <span className="text-xs font-normal text-muted-foreground">(총 {blocked.length} 개, 다시 실행 가능)</span>
            </CardTitle>
            <Button size="sm" variant="outline" disabled={rerunning.has("__all__")} onClick={() => void rerunAll()}>
              <RefreshCwIcon className={`size-3.5 ${rerunning.has("__all__") ? "animate-spin" : ""}`} />
              모두 다시 실행
            </Button>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            {blocked.slice(0, 20).map((i) => (
              <div key={i.id} className="flex items-center gap-2 text-sm">
                <StatusBadge domain="intent" value={i.state} />
                <span className="min-w-0 flex-1 truncate">{i.payload}</span>
                <Button
                  size="sm"
                  variant="ghost"
                  className="h-7 shrink-0 px-2 text-xs"
                  disabled={rerunning.has(i.id)}
                  onClick={() => void rerunOne(i.id)}
                >
                  <RefreshCwIcon className={`size-3 ${rerunning.has(i.id) ? "animate-spin" : ""}`} />
                  다시 실행
                </Button>
              </div>
            ))}
            {blocked.length > 20 && (
              <p className="text-xs text-muted-foreground">
                표시된 항목은 처음 20 개입니다. 나머지는 「모두 다시 실행」으로 처리하세요 {blocked.length - 20} 건.
              </p>
            )}
          </CardContent>
        </Card>
      )}

      {/* Stat cards */}
      <div className="grid grid-cols-2 gap-4 lg:grid-cols-3">
        <StatCard label="할당 대기 의도" value={open.length} icon={ShieldCheckIcon} sub="frontier 열림" />
        <StatCard label="확인된 발견 사항" value={taskFindings.length} icon={BugIcon} sub="이 작업" />
        <StatCard label="전체 의도 수" value={intents.length} icon={AlertTriangleIcon} sub="이 작업의 모든 의도" />
      </div>
    </div>
  );
}

const TASK_RULE_KIND_OPTIONS: { value: AssetInterceptKind; label: string; placeholder: string }[] = [
  { value: "exact_domain", label: "도메인(완전 일치)", placeholder: "example.gov.cn" },
  { value: "exact_ip", label: "IP(완전 일치)", placeholder: "203.0.113.10" },
  { value: "exact_url", label: "URL(완전 일치)", placeholder: "https://example.com/login" },
  { value: "fuzzy_domain", label: "도메인(부분 일치)", placeholder: ".gov.cn" },
  { value: "fuzzy_ip", label: "IP(부분 일치)", placeholder: "203.0.113." },
  { value: "fuzzy_url", label: "URL(부분 일치)", placeholder: "/admin" },
  { value: "cidr", label: "CIDR 네트워크 대역", placeholder: "192.168.0.0/16" },
];

const TASK_RULE_KIND_LABEL: Record<AssetInterceptKind, string> = Object.fromEntries(
  TASK_RULE_KIND_OPTIONS.map((o) => [o.value, o.label]),
) as Record<AssetInterceptKind, string>;

// TaskInterceptRulesCard는 작업 상세 개요의 작업별 자산 차단/허용 규칙을 관리합니다.
// 목록/추가/인라인 편집/삭제/활성화를 제공하며 규칙은 이 작업에만 적용하고 전역에는 저장하지 않습니다.
function TaskInterceptRulesCard({ taskId }: { taskId: string }) {
  const [rules, setRules] = React.useState<AssetInterceptRule[]>([]);
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState("");
  const [newAction, setNewAction] = React.useState<"block" | "allow">("block");
  const [newKind, setNewKind] = React.useState<AssetInterceptKind>("fuzzy_domain");
  const [newPattern, setNewPattern] = React.useState("");
  const [newNote, setNewNote] = React.useState("");
  const [editId, setEditId] = React.useState<number | null>(null);
  const [editAction, setEditAction] = React.useState<"block" | "allow">("block");
  const [editKind, setEditKind] = React.useState<AssetInterceptKind>("fuzzy_domain");
  const [editPattern, setEditPattern] = React.useState("");
  const [editNote, setEditNote] = React.useState("");

  const load = React.useCallback(async () => {
    try {
      setRules(await api.taskInterceptRules(taskId));
    } catch {
      // 일시적 오류 무시
    }
  }, [taskId]);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function add() {
    if (!newPattern.trim()) return;
    setBusy(true);
    setErr("");
    try {
      await api.createTaskInterceptRule(taskId, {
        action: newAction,
        kind: newKind,
        pattern: newPattern.trim(),
        note: newNote.trim(),
        enabled: true,
      });
      setNewPattern("");
      setNewNote("");
      await load();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  function startEdit(r: AssetInterceptRule) {
    setEditId(r.id);
    setEditAction(r.action ?? "block");
    setEditKind(r.kind);
    setEditPattern(r.pattern);
    setEditNote(r.note);
    setErr("");
  }

  async function saveEdit(r: AssetInterceptRule) {
    if (!editPattern.trim()) return;
    setBusy(true);
    setErr("");
    try {
      await api.updateTaskInterceptRule(taskId, r.id, {
        action: editAction,
        kind: editKind,
        pattern: editPattern.trim(),
        note: editNote.trim(),
        enabled: r.enabled,
      });
      setEditId(null);
      await load();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function remove(r: AssetInterceptRule) {
    setRules((prev) => prev.filter((x) => x.id !== r.id));
    try {
      await api.deleteTaskInterceptRule(taskId, r.id);
    } catch {
      await load();
    }
  }

  async function toggle(r: AssetInterceptRule) {
    setRules((prev) => prev.map((x) => (x.id === r.id ? { ...x, enabled: !x.enabled } : x)));
    try {
      await api.toggleTaskInterceptRule(taskId, r.id, !r.enabled);
    } catch {
      await load();
    }
  }

  const placeholder = TASK_RULE_KIND_OPTIONS.find((o) => o.value === newKind)?.placeholder ?? "";

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <ShieldCheckIcon className="size-4 text-sky-500" /> 작업별 자산 차단 / 허용
          <span className="text-muted-foreground text-xs font-normal">
            (이 작업에만 적용되며 전역에는 반영하지 않습니다. 차단 규칙 적용 후 허용 규칙 적용, 총 {rules.length} 건)
          </span>
        </CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {/* 추가 폼 */}
        <div className="flex flex-wrap items-center gap-2">
          <NativeSelect size="sm" value={newAction} onChange={(e) => setNewAction(e.target.value as "block" | "allow")}>
            <NativeSelectOption value="block">차단</NativeSelectOption>
            <NativeSelectOption value="allow">허용</NativeSelectOption>
          </NativeSelect>
          <NativeSelect size="sm" value={newKind} onChange={(e) => setNewKind(e.target.value as AssetInterceptKind)}>
            {TASK_RULE_KIND_OPTIONS.map((o) => (
              <NativeSelectOption key={o.value} value={o.value}>
                {o.label}
              </NativeSelectOption>
            ))}
          </NativeSelect>
          <Input
            className="h-7 min-w-56 flex-1 text-sm"
            placeholder={placeholder}
            value={newPattern}
            onChange={(e) => setNewPattern(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void add();
            }}
            disabled={busy}
          />
          <Input
            className="h-7 w-36 text-sm"
            placeholder="비고(선택 사항)"
            value={newNote}
            onChange={(e) => setNewNote(e.target.value)}
            disabled={busy}
          />
          <Button size="sm" variant="outline" disabled={busy || !newPattern.trim()} onClick={() => void add()}>
            <PlusIcon className="size-3.5" /> 추가
          </Button>
          {err && <span className="text-xs text-red-500">{err}</span>}
        </div>
        {/* 규칙 목록 */}
        {rules.length > 0 ? (
          <div className="flex flex-col gap-1.5">
            {rules.map((r) =>
              editId === r.id ? (
                <div key={r.id} className="flex flex-wrap items-center gap-2 rounded-md border px-2.5 py-1.5">
                  <NativeSelect
                    size="sm"
                    value={editAction}
                    onChange={(e) => setEditAction(e.target.value as "block" | "allow")}
                  >
                    <NativeSelectOption value="block">차단</NativeSelectOption>
                    <NativeSelectOption value="allow">허용</NativeSelectOption>
                  </NativeSelect>
                  <NativeSelect
                    size="sm"
                    value={editKind}
                    onChange={(e) => setEditKind(e.target.value as AssetInterceptKind)}
                  >
                    {TASK_RULE_KIND_OPTIONS.map((o) => (
                      <NativeSelectOption key={o.value} value={o.value}>
                        {o.label}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                  <Input
                    className="h-7 min-w-56 flex-1 text-sm"
                    value={editPattern}
                    onChange={(e) => setEditPattern(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") void saveEdit(r);
                      if (e.key === "Escape") setEditId(null);
                    }}
                    disabled={busy}
                    autoFocus
                  />
                  <Input
                    className="h-7 w-36 text-sm"
                    placeholder="비고(선택 사항)"
                    value={editNote}
                    onChange={(e) => setEditNote(e.target.value)}
                    disabled={busy}
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    className="h-6 shrink-0 px-1.5"
                    disabled={busy || !editPattern.trim()}
                    onClick={() => void saveEdit(r)}
                  >
                    <CheckIcon className="size-3.5 text-emerald-500" />
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="h-6 shrink-0 px-1.5"
                    disabled={busy}
                    onClick={() => setEditId(null)}
                  >
                    <XIcon className="size-3.5" />
                  </Button>
                </div>
              ) : (
                <div key={r.id} className="flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-sm">
                  <span
                    className={`shrink-0 rounded px-1.5 py-0.5 text-xs ${
                      r.action === "allow"
                        ? "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
                        : "bg-red-500/15 text-red-600 dark:text-red-400"
                    }`}
                  >
                    {r.action === "allow" ? "허용" : "차단"}
                  </span>
                  <span className="text-muted-foreground shrink-0 text-xs">{TASK_RULE_KIND_LABEL[r.kind]}</span>
                  <code className="bg-muted min-w-0 flex-1 truncate rounded px-1.5 py-0.5 text-xs">{r.pattern}</code>
                  {r.note && (
                    <span className="text-muted-foreground max-w-[120px] shrink-0 truncate text-xs">{r.note}</span>
                  )}
                  <Switch checked={r.enabled} onCheckedChange={() => void toggle(r)} />
                  <Button
                    size="sm"
                    variant="ghost"
                    className="h-6 shrink-0 px-1.5"
                    disabled={busy}
                    onClick={() => startEdit(r)}
                  >
                    <PencilIcon className="size-3.5" />
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="h-6 shrink-0 px-1.5"
                    disabled={busy}
                    onClick={() => void remove(r)}
                  >
                    <Trash2Icon className="size-3.5 text-red-500" />
                  </Button>
                </div>
              ),
            )}
          </div>
        ) : (
          <p className="text-muted-foreground text-sm">
            작업별 규칙 없음. 「차단」 규칙과 일치하면 테스트를 금지합니다. 「허용」은 허용 목록으로, 설정하면 일치하는 자산만 허용합니다(미설정 시 허용 목록 비활성).
          </p>
        )}
      </CardContent>
    </Card>
  );
}
