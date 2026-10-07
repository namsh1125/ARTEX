// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: 意图 / 覆盖 / 任务 / 严重度 each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose 用作「严重」——实心高强调,视觉上明显高于「高危」的软红描边。
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "할당 대기", tone: "slate" },
  running: { label: "실행 중", tone: "blue" },
  paused: { label: "일시 중지됨", tone: "amber" },
  done: { label: "완료됨", tone: "green" },
  // blocked = 模型/API/网络故障重试用尽，这条意图基本没真正探成（非目标拦截）。
  blocked: { label: "실행 오류", tone: "red" },
  // exhausted = 达到步数/时间预算被中途掐断、只写回部分结果（非方向已探尽）。
  exhausted: { label: "예산 소진", tone: "violet" },
  // stopped = 历史软删除状态（保留,历史数据）。
  stopped: { label: "중지됨", tone: "slate" },
  // deleted = 用户假删除了该意图（保留节点与血缘，删除原因见 delete_reason 字段）。
  deleted: { label: "삭제됨", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "생성됨", tone: "slate" },
  queued: { label: "대기 중", tone: "amber" },
  running: { label: "실행 중", tone: "blue" },
  paused: { label: "일시 중지됨", tone: "amber" },
  done: { label: "완료됨", tone: "green" },
  failed: { label: "실패", tone: "red" },
  timeout: { label: "시간 초과", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "심각", tone: "rose" },
  high: { label: "높음", tone: "red" },
  medium: { label: "중간", tone: "amber" },
  low: { label: "낮음", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "처리 대기", tone: "amber" },
  in_progress: { label: "처리 중", tone: "blue" },
  confirmed: { label: "확인됨", tone: "red" },
  resolved: { label: "처리됨", tone: "green" },
  fixed: { label: "수정됨", tone: "green" },
  false_positive: { label: "오탐", tone: "slate" },
  ignored: { label: "무시", tone: "neutral" },
  duplicate: { label: "중복", tone: "neutral" },
  risk_accepted: { label: "위험 수용", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "탐색 중", tone: "blue" },
  paused: { label: "일시 중지됨", tone: "amber" },
  stalled: { label: "정체", tone: "red" },
  idle: { label: "유휴", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "진행 중", tone: "blue" },
  met: { label: "달성됨", tone: "green" },
  abandoned: { label: "포기됨", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "허용", tone: "green" },
  block: { label: "차단", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "관찰", tone: "slate" },
  confirmed: { label: "확인", tone: "green" },
  tombstoned: { label: "폐기", tone: "neutral" },
};

// 推送投递状态。sending 用 blue 而不是 amber：它不是「有问题」，
// 而是「已被领取、正在发」，与 pending 的等待语义要能区分开。
const delivery: Record<string, StatusMeta> = {
  pending: { label: "전송 대기", tone: "amber" },
  sending: { label: "전송 중", tone: "blue" },
  sent: { label: "전달됨", tone: "green" },
  failed: { label: "실패", tone: "red" },
  skipped: { label: "건너뜀", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}
