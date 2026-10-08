"use client";

// LLM 재시도 공통 설정: 다섯 계층의 횟수와 간격.
//
// 내부부터 연결(SDK) → 빈 응답(SDK) → 동일 공급자 안전 구간 → 순환 선택 회로 차단 → 의도 재실행.
// 앞 세 계층은 엔드포인트별로 전역값을 덮어쓸 수 있고 뒤 두 계층은 프로세스 전체에 하나만 있습니다.
//
// 모든 입력은 백엔드 db.RetryRule과 같은 빈 값=미설정 의미를 사용합니다.
//   횟수: 빈 값/0=기본값, -1=재시도 비활성화, 양수=지정 횟수
//   간격: 빈 값/0=기본 지수 백오프, 양수=지정한 고정 밀리초 간격

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** 이 재시도 계층의 실행 위치와 실행 주체 */
  where: string;
  /** 이 계층으로 들어오는 오류를 상태 코드까지 구체적으로 설명 */
  trigger: string;
  /** 비슷하지만 이 계층에서 처리하지 않는 오류를 구분 bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 횟수를 비워 두었을 때의 기본값. 입력 안내에 사용 */
  defAttempts: number;
  /** 간격을 비워 두었을 때의 기본 정책. 입력 안내에 사용 */
  defInterval: string;
  /** 횟수 입력값 -1 의 의미 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "연결 재시도",
    where: "SDK · 수신 대상 200 이전",
    trigger:
      "연결에 실패했거나 HTTP 200을 아직 받지 못한 경우: 연결 재설정, 읽기·쓰기 시간 초과, DNS 실패 등의 네트워크 오류와 HTTP 408·429·500·502·503·504.",
    skips: "나머지 상태 코드(400 / 401 / 403 / 404 / 413 / 422 등)는 확정적 거부이므로 재전송해도 실패합니다. 즉시 상위로 전달합니다.",
    desc: "동일한 요청을 그대로 재전송합니다. HTTP 200을 받고 스트림이 시작된 이후의 연결 끊김은 이 계층에서 처리하지 않습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 지수 증가(상한 8s)",
    offHint: "-1 = 재시도하지 않고 실패를 즉시 상위로 전달합니다",
  },
  empty: {
    title: "빈 응답 재시도",
    where: "SDK · 전용 openai 형식",
    trigger:
      "HTTP 200이고 finish_reason이 정상적인 stop이지만 응답 전체에 콘텐츠 블록이 없는 경우입니다. 게이트웨이의 빈 프레임, 추론 필드 누락, 일시적인 샘플링 오류 등이 해당합니다.",
    skips: "max_tokens 한도로 잘려 내용이 없는 경우는 제외합니다. 출력 한도를 늘려야 하며 재전송해도 같은 한도에 걸립니다.",
    desc: "전체 프롬프트를 다시 전송하므로 긴 컨텍스트에서는 비용이 큽니다. 횟수를 높이지 않는 것이 좋습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 지수 증가(상한 8s)",
    offHint: "-1 = 빈 응답을 그대로 전달합니다",
  },
  stream: {
    title: "동일 provider 안전 구간 재시도",
    where: "이 프로젝트 · 출력 전달 전",
    trigger:
      "HTTP 200을 받고 스트림이 연결된 뒤 문제가 발생했지만 토큰을 아직 호출자에게 전달하지 않은 경우입니다. 연결 중단, 공급자의 overloaded, 스트림 내부 429 / 5xx 오류 이벤트가 해당합니다.",
    skips:
      "할당량 소진(402 / insufficient_quota, 순환 선택으로 설정 전환), 컨텍스트 초과(413 / context length, 압축으로 처리)、400 / 401 / 403 / 404 / 422 확정적 거부는 재시도하지 않습니다.",
    desc: "동일한 설정에서 동일한 요청을 재생합니다. 아직 출력이 전달되지 않았으므로 모델 출력이나 도구 실행이 중복되지 않습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s 지수 증가(상한 4s)",
    offHint: "-1 = 스트림 끊김을 바깥 계층의 의도 재실행에 전달합니다",
  },
  breaker: {
    title: "순환 선택 회로 차단",
    where: "이 프로젝트 · 프로세스 전체에서 공유",
    trigger:
      "일시적 실패(429、5xx、네트워크 오류)가 연속 누적되어 임계값에 도달하면 회로를 차단합니다. 잔액 부족(402)、유효하지 않은 키(401 / 403)、존재하지 않는 모델(404)같은 확정적 실패는 임계값과 관계없이 첫 발생 시 차단합니다.",
    skips: "한 번 성공하면 누적값을 초기화하므로 간헐적 오류만으로 서서히 차단되지 않습니다.",
    desc: "차단 후 대기 기간에 들어가며 해당 기간에는 순환 선택에서 건너뜁니다. 상태를 데이터베이스에 저장하므로 재시작해도 유지됩니다.",
    attemptsLabel: "회로 차단까지의 연속 실패 횟수",
    defAttempts: 3,
    defInterval: "1min→5min→30min 단계 증가",
    offHint: "-1 = 일시적 실패는 회로를 차단하지 않음(확정적 실패는 여전히 차단)",
  },
  intent: {
    title: "의도 재실행",
    where: "이 프로젝트 · 프로세스 전체에서 공유",
    trigger:
      "앞선 계층에서 처리하지 못해 worker가 model_error로 종료된 경우입니다. 내부 재시도를 모두 소진했거나 출력 전달 후 연결이 끊겨 안전한 재생 대신 전체 재실행이 필요한 경우입니다.",
    skips: "할당량 소진은 순환 선택의 설정 전환으로 처리하며 여기서 재실행하지 않습니다. 작업이 일시 중지되거나 / 종료 / 종료 절차에 들어가면 백오프를 기다리지 않고 즉시 양보합니다.",
    desc: "의도 전체를 처음부터 다시 실행합니다. 가장 바깥 계층이므로 재실행할 때마다 내부 계층의 재시도도 다시 발생합니다.",
    attemptsLabel: "재실행 횟수",
    defAttempts: 2,
    defInterval: "고정 3s",
    offHint: "-1 = 재실행하지 않으며 의도를 바로 다음 상태로 판단: blocked",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** 밀리초를 읽기 쉽게 변환. 입력 옆 표시 전용。 */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 제어형 숫자 입력: 빈 문자열 ↔ 0，중간 상태（"-"、"1e"）상위 상태에 전달하지 않고 로컬에 그대로 유지。 */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 정책 조회나 설정 전환으로 부모 값 전체가 바뀌면 동기화합니다. 직접 입력할 때는
  // value가 이미 로컬 텍스트의 파싱 결과와 같으므로 이 경로를 타지 않습니다.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 재시도 계층의 두 설정。idPrefix 같은 페이지에 여러 번 표시되어도 유지하기 위한 값 label 의 htmlFor。 */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 모델 설정 패널의 간략형: 상세 설명을 생략하고 대상 오류만 표시 */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* 이 계층에서 처리하는 오류와 상태 코드. 설정해도 효과가 없다면 해당 계층 밖 오류일 수 있습니다. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">발생 조건</span>：{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">이 계층에서 처리하지 않음</span>：{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`기본값 ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            간격 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="기본 백오프"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `고정 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">비워 두기 = 기본값 사용; {meta.offHint}。</p>}
    </div>
  );
}

/** 모델 설정 패널의 세 계층별 재정의(엔드포인트를 따르는 계층）。 */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">재시도 재정의</Label>
        <p className="text-muted-foreground text-xs">
          이 설정에만 적용하며 「재시도 및 백오프」의 전역 기본값을 덮어씁니다. 항목을 비워 두면 전역값을 사용하며, 횟수를 -1로 설정하면 해당 계층의 재시도가 비활성화됩니다.
          간격을 입력하면 지수 백오프 대신 고정 간격을 사용합니다. 회로 차단과 의도 재실행은 프로세스 수준이므로 전역 페이지에서만 조정할 수 있습니다.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「재시도 및 백오프」tab：다섯 계층의 전역 기본값。 */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`재시도 정책을 읽지 못했습니다: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // 백엔드가 범위를 벗어난 값을 보정해 반환하므로 반환값으로 갱신하여 표시와 저장값을 맞춥니다.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("저장 즉시 적용됩니다(현재 진행 중인 호출에는 기존 값 사용)");
    } catch (e) {
      toast.error(`저장 실패: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> 재시도 정책 읽기…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        모델 호출 실패는 안쪽에서 바깥쪽으로 다섯 계층을 거칩니다:
        <span className="text-foreground"> 연결 → 빈 응답 → 동일 provider 안전 구간 → 순환 선택 회로 차단 → 의도 재실행</span>
        .내부 계층을 모두 소진한 후 외부 계층이 실행되므로 횟수는
        <span className="text-foreground">곱해집니다</span>
        . 각 계층을 최대로 설정하면 한 번의 일시적 장애로 수십 번의 요청이 발생할 수 있습니다.
        모두 비우면 현재 기본값을 사용하여 기존 동작과 동일합니다. 앞의 세 계층은 각 모델 설정에서 개별 재정의할 수 있습니다.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          저장
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          모두 기본값 복원
        </Button>
      </div>
    </div>
  );
}
