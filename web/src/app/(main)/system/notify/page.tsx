"use client";

import * as React from "react";

import { BellIcon, PlusIcon, SendIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { NotificationChannel, NotificationFilter, NotificationMeta } from "@/lib/types";

import {
  CHANNEL_FIELDS,
  type ChannelForm,
  emptyForm,
  KIND_LABEL,
  parseIDs,
  parseKeywords,
  parseKV,
  SEVERITY_OPTIONS,
} from "./_components/channel-fields";
import { ConfigField, FilterSummary } from "./_components/channel-form";
import { DeliveryList } from "./_components/delivery-list";
import { formatBacklog, StatTile } from "./_components/stat-tile";

// 이 페이지는 데이터 로드, 폼 상태, API 호출만 조율합니다.
// 필드 정의와 파싱은 _components/channel-fields.ts, 컨트롤과 필터 요약은
// _components/channel-form.tsx, 전송 기록은 _components/delivery-list.tsx에서 담당합니다.
// 각 구성요소를 독립적으로 이해할 수 있도록 분리했습니다. 합쳐 두면 페이지가 약 1,100줄이 됩니다.
export default function NotifyPage() {
  const [meta, setMeta] = React.useState<NotificationMeta | null>(null);
  const [channels, setChannels] = React.useState<NotificationChannel[]>([]);
  const [tab, setTab] = React.useState<"channels" | "deliveries">("channels");

  const [open, setOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<NotificationChannel | null>(null);
  const [form, setForm] = React.useState<ChannelForm>(emptyForm("dingtalk"));
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);

  const [globalSaving, setGlobalSaving] = React.useState(false);
  const [baseURL, setBaseURL] = React.useState("");
  const [digestMin, setDigestMin] = React.useState("");

  const load = React.useCallback(() => {
    api
      .notifyMeta()
      .then((m) => {
        setMeta(m);
        setBaseURL(m.public_base_url);
        setDigestMin(m.digest_interval_min);
      })
      .catch((e) => toast.error("알림 설정을 읽지 못했습니다: " + (e as Error).message));
    // 채널 조회 실패를 반드시 표시합니다. 조용히 실패하면 채널이 없는 것처럼 보여
    // 사용자가 설정을 잃었다고 오해할 수 있습니다.
    api
      .notifyChannels()
      .then(setChannels)
      .catch((e) => toast.error("채널 목록을 읽지 못했습니다: " + (e as Error).message));
  }, []);
  React.useEffect(() => {
    load();
  }, [load]);

  function setF(patch: Partial<ChannelForm>) {
    setForm((f) => ({ ...f, ...patch }));
  }
  function setCfg(key: string, value: unknown) {
    setForm((f) => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function openAdd() {
    setEditing(null);
    setForm(emptyForm(meta?.kinds[0]?.kind ?? "dingtalk"));
    setOpen(true);
  }

  function openEdit(ch: NotificationChannel) {
    setEditing(ch);
    // 백엔드 filter는 Go 구조체로 항상 객체로 직렬화되므로 null 대체 처리가 필요하지 않습니다.
    const f = ch.filter;
    setForm({
      name: ch.name,
      kind: ch.kind,
      mode: ch.mode,
      enabled: ch.enabled,
      ratePerMin: String(ch.rate_per_min),
      // 백엔드 config의 자격 증명은 마스킹 값입니다. 폼에 유지하고 그대로 제출하면
      // 백엔드가 DB의 원래 값을 보존합니다.
      config: { ...ch.config },
      minSeverity: f.min_severity ?? "",
      includeText: (f.vulnclass_include ?? []).join("\n"),
      excludeText: (f.vulnclass_exclude ?? []).join("\n"),
      taskIDsText: (f.task_ids ?? []).join(","),
      assetIDsText: (f.asset_ids ?? []).join(","),
      onStatusChange: f.on_status_change ?? false,
    });
    setOpen(true);
  }

  // buildConfig는 폼 상태를 채널 config로 변환합니다.
  //
  // 값은 다음 두 가지 규칙만 적용합니다.
  //   - __masked__ 접두사 값은 그대로 반환하여 백엔드가 미변경으로 보고 기존 값 유지
  //   - 나머지는 사용자 입력 그대로 제출하며 빈 문자열은 필드 지우기
  //
  // 자격 증명이 비었을 때 생략하는 예외를 두면 잘못 설정한 키를 사용자가
  // 지울 방법이 없어집니다. 입력란을 비우면 해당 필드도 비워지는
  // 일관되고 사용자가 제어할 수 있는 규칙을 사용합니다.
  // ConfigField는 마스킹 값을 입력란에 표시하지 않으므로 입력란의 텍스트는
  // 항상 사용자가 직접 입력한 값입니다.
  function buildConfig(): Record<string, unknown> {
    const defs = CHANNEL_FIELDS[form.kind] ?? [];
    const out: Record<string, unknown> = {};
    for (const d of defs) {
      const raw = form.config[d.key];
      if (d.kind === "switch") {
        out[d.key] = raw === true;
        continue;
      }
      if (typeof raw === "string" && raw.startsWith("__masked__")) {
        out[d.key] = raw;
        continue;
      }
      if (d.kind === "number") {
        const n = Number(raw);
        out[d.key] = Number.isFinite(n) && n > 0 ? n : 0;
        continue;
      }
      if (d.kind === "kv") {
        out[d.key] = parseKV(String(raw ?? ""));
        continue;
      }
      if (d.kind === "list") {
        out[d.key] = String(raw ?? "")
          .split(/[\s,，]+/)
          .map((s) => s.trim())
          .filter(Boolean);
        continue;
      }
      out[d.key] = String(raw ?? "").trim();
    }
    return out;
  }

  function buildFilter(): NotificationFilter {
    return {
      min_severity: form.minSeverity || undefined,
      vulnclass_include: parseKeywords(form.includeText),
      vulnclass_exclude: parseKeywords(form.excludeText),
      task_ids: parseIDs(form.taskIDsText),
      asset_ids: parseIDs(form.assetIDsText),
      on_status_change: form.onStatusChange,
    };
  }

  async function saveForm() {
    if (!form.name.trim()) {
      toast.error("채널 이름을 입력해 주세요");
      return;
    }
    setSaving(true);
    try {
      const payload = {
        name: form.name.trim(),
        kind: form.kind,
        mode: form.mode,
        enabled: form.enabled,
        config: buildConfig(),
        filter: buildFilter(),
        rate_per_min: form.ratePerMin.trim() === "" ? undefined : Number(form.ratePerMin),
      };
      if (editing) {
        await api.notifyUpdateChannel(editing.id, payload);
        toast.success("저장됨");
        setOpen(false);
      } else {
        await api.notifyCreateChannel(payload);
        toast.success("채널을 추가했습니다");
        setOpen(false);
      }
      load();
    } catch (e) {
      toast.error("저장 실패: " + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function testChannel() {
    if (!editing) return;
    setTesting(true);
    try {
      const r = await api.notifyTestChannel(editing.id);
      toast.success(`테스트 메시지를 보냈습니다(${r.latency_ms} ms), 그룹에서 확인해 주세요`);
    } catch (e) {
      // 설정 오류 진단에 필요한 채널의 원래 오류를 백엔드가 반환하므로 그대로 표시합니다.
      toast.error("테스트 실패: " + (e as Error).message, { duration: 12000 });
    } finally {
      setTesting(false);
    }
  }

  async function removeChannel(ch: NotificationChannel) {
    try {
      await api.notifyDeleteChannel(ch.id);
      toast.success(`삭제됨: ${ch.name}`);
      setOpen(false);
      load();
    } catch (e) {
      toast.error("삭제 실패: " + (e as Error).message);
    }
  }

  async function toggleEnabled(ch: NotificationChannel) {
    try {
      await api.notifyUpdateChannel(ch.id, { enabled: !ch.enabled });
      load();
    } catch (e) {
      toast.error("작업 실패: " + (e as Error).message);
    }
  }

  async function toggleGlobal(on: boolean) {
    setGlobalSaving(true);
    try {
      await api.setSettings({ notify_enabled: on });
      setMeta((m) => (m ? { ...m, enabled: on } : m));
      toast.success(on ? "알림을 활성화했습니다" : "알림을 일시 중지했습니다");
    } catch (e) {
      toast.error("작업 실패: " + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  async function saveGlobal() {
    setGlobalSaving(true);
    try {
      const patch: Record<string, unknown> = { notify_public_base_url: baseURL.trim() };
      const n = Number(digestMin);
      if (Number.isFinite(n) && n > 0) patch.notify_digest_interval_min = n;
      await api.setSettings(patch);
      toast.success("저장됨");
      load();
    } catch (e) {
      toast.error("저장 실패: " + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  const fields = CHANNEL_FIELDS[form.kind] ?? [];
  const secretKeys = new Set(meta?.kinds.find((k) => k.kind === form.kind)?.secret_keys ?? []);
  const defaultRate = meta?.kinds.find((k) => k.kind === form.kind)?.default_rate_per_min ?? 0;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">알림 전송</h1>
          <p className="text-muted-foreground text-sm">
            취약점 발견 시 DingTalk, Feishu, WeCom 등의 채널로 알립니다. 채널별 전송 시점과 필터 규칙을 설정할 수 있습니다.
          </p>
        </div>
        {meta && (
          // Switch에 aria-label이 있으므로 외부는 label 대신 div를 사용합니다.
          // 연결할 네이티브 컨트롤도 없고 텍스트 클릭이 전환될 것 같은 오해도 피합니다.
          <div className="flex shrink-0 items-center gap-2 text-sm">
            <span className="text-muted-foreground">전체 스위치</span>
            <Switch
              checked={meta.enabled}
              disabled={globalSaving}
              onCheckedChange={toggleGlobal}
              aria-label="알림 전체 스위치"
            />
          </div>
        )}
      </div>

      {meta && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StatTile label="채널" value={`${meta.stats.channels_on} / ${meta.stats.channels}`} hint="활성화 / 총수" />
          <StatTile label="오늘 전달됨" value={String(meta.stats.sent_today)} />
          <StatTile label="전송 대기" value={String(meta.stats.pending)} />
          <StatTile label="실패" value={String(meta.stats.failed)} tone={meta.stats.failed > 0 ? "red" : undefined} />
          <StatTile
            label="최장 대기"
            value={formatBacklog(meta.stats.backlog_age_ms)}
            // 적체 3건은 3초에서 3시간까지 가능하므로 개수보다 오래된 정도가 유용합니다.
            hint={meta.stats.backlog_age_ms > 5 * 60_000 ? "알림 전송이 멈췄을 수 있습니다" : undefined}
            tone={meta.stats.backlog_age_ms > 5 * 60_000 ? "red" : undefined}
          />
        </div>
      )}

      <Card className="gap-3">
        <CardHeader>
          <CardTitle className="text-base">전역 설정</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="n-base">상세 페이지 주소</Label>
            <Input
              id="n-base"
              placeholder="https://artex.example.com"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">메시지의 「상세 보기」 버튼이 가리킬 주소입니다. 비워 두면 버튼을 표시하지 않습니다.</p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="n-digest">취합 주기(분)</Label>
            <Input
              id="n-digest"
              type="number"
              min={1}
              max={1440}
              placeholder="30"
              value={digestMin}
              onChange={(e) => setDigestMin(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">「취합」 모드 채널에만 적용.</p>
          </div>
          <div className="sm:col-span-2">
            <Button onClick={saveGlobal} disabled={globalSaving}>
              전역 설정 저장
            </Button>
          </div>
        </CardContent>
      </Card>

      <Tabs value={tab} onValueChange={(v) => setTab(v as "channels" | "deliveries")} className="flex flex-col gap-4">
        <TabsList>
          <TabsTrigger value="channels">채널</TabsTrigger>
          <TabsTrigger value="deliveries">전송 기록</TabsTrigger>
        </TabsList>

        <TabsContent value="channels">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <button
              type="button"
              onClick={openAdd}
              className="text-foreground/70 border-foreground/70 hover:bg-muted/60 hover:shadow-sm flex min-h-[130px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed transition"
            >
              <PlusIcon className="size-6" />
              <span className="text-sm">채널 추가</span>
            </button>

            {channels.map((ch) => (
              <Card
                key={ch.id}
                onClick={() => openEdit(ch)}
                className="hover:border-primary/60 cursor-pointer gap-3 transition hover:shadow-sm"
              >
                <CardHeader>
                  <div className="flex items-center gap-2">
                    <BellIcon className="text-muted-foreground size-4 shrink-0" />
                    <CardTitle className="truncate text-base">{ch.name}</CardTitle>
                    {/* 카드 전체가 편집 버튼이므로 두 컨트롤은 각각 이벤트 전파를 막아야 하며,
                        그렇지 않으면 스위치/삭제 시 편집도 실행됨. 다음을 stopPropagation 컨트롤 자체에 연결
                        하고 별도 요소로 감싸지 않음 div：감싸면 div 상호작용 가능해 보이지만 다음이 없는 요소 생성:
                        역할을 가진 정적 요소가 되어 다음을 발생시킴 a11y 경고를 유발하고 의미상으로도 부적절함。 */}
                    <div className="ml-auto flex items-center gap-2">
                      <Switch
                        checked={ch.enabled}
                        onCheckedChange={() => toggleEnabled(ch)}
                        onClick={(e) => e.stopPropagation()}
                        aria-label="활성화"
                      />
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="삭제"
                        onClick={(e) => {
                          e.stopPropagation();
                          // removeChannel이 자체 catch/toast하므로 void로 Promise를 명시적으로 무시합니다.
                          // onClick은 async가 아니므로 여기서 await할 필요가 없습니다.
                          void removeChannel(ch);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="grid gap-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">{KIND_LABEL[ch.kind] ?? ch.kind}</Badge>
                    <Badge variant="outline">{ch.mode === "digest" ? "취합" : "실시간"}</Badge>
                    {!ch.enabled && <Badge variant="outline">비활성화됨</Badge>}
                  </div>
                  <FilterSummary filter={ch.filter} />
                </CardContent>
              </Card>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="deliveries">
          <DeliveryList channels={channels} />
        </TabsContent>
      </Tabs>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="right" className="w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader>
            <SheetTitle>{editing ? editing.name : "알림 채널 추가"}</SheetTitle>
            <SheetDescription>
              {KIND_LABEL[form.kind] ?? form.kind}
              {defaultRate > 0 ? ` · 기본 요청 제한 ${defaultRate} 건/분` : " · 요청 제한 없음"}
            </SheetDescription>
          </SheetHeader>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4">
            <div className="grid gap-4 py-4">
              <div className="grid gap-2">
                <Label>채널 유형</Label>
                <Select
                  value={form.kind}
                  onValueChange={(v) => {
                    // 유형 변경은 자격 증명 필드 전체 변경이므로 이전 설정을 병합하지 않습니다.
                    setF({ kind: v, config: {} });
                  }}
                  disabled={!!editing}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(meta?.kinds ?? []).map((k) => (
                      <SelectItem key={k.kind} value={k.kind}>
                        {KIND_LABEL[k.kind] ?? k.kind}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {editing && (
                  <p className="text-muted-foreground text-xs">
                    채널 유형은 변경할 수 없습니다. 유형이 바뀌면 인증 정보도 달라지므로 새 채널을 만드세요.
                  </p>
                )}
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-name">채널 이름</Label>
                <Input
                  id="n-name"
                  placeholder="사고 대응 그룹 / 일상 알림 그룹"
                  value={form.name}
                  onChange={(e) => setF({ name: e.target.value })}
                />
              </div>

              {fields.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  이 채널의 양식이 정의되지 않았습니다(프런트엔드에 누락된 항목: CHANNEL_FIELDS 항목). 추가한 후 다시 시도하세요.
                </p>
              ) : (
                fields.map((d) => (
                  <ConfigField
                    key={d.key}
                    def={d}
                    value={form.config[d.key]}
                    isSecret={secretKeys.has(d.key)}
                    onChange={(v) => setCfg(d.key, v)}
                  />
                ))
              )}

              <div className="grid gap-2">
                <Label>전송 시점</Label>
                <Select value={form.mode} onValueChange={(v) => setF({ mode: v as "realtime" | "digest" })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="realtime">실시간 · 취약점마다 개별 전송</SelectItem>
                    <SelectItem value="digest">취합 · 주기별로 모아서 전송</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-muted-foreground text-xs">
                  「높음은 실시간, 나머지는 취합」하려면 채널 두 개를 만드세요. 하나는 실시간 + 최소 심각도를 높음으로, 다른 하나는 취합 + 심각도 제한 없음.
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-rate">요청 제한(건/분)</Label>
                <Input
                  id="n-rate"
                  type="number"
                  min={0}
                  placeholder={defaultRate > 0 ? String(defaultRate) : "0 = 제한 없음"}
                  value={form.ratePerMin}
                  onChange={(e) => setF({ ratePerMin: e.target.value })}
                />
                <p className="text-muted-foreground text-xs">
                  비워 두면 채널 기본값 사용; 0 이면 제한하지 않습니다. 한도를 넘어도 메시지는 유실되지 않고 전송이 지연됩니다.
                </p>
              </div>

              <div className="border-t pt-4">
                <p className="mb-3 text-sm font-medium">필터 규칙(비워 두면 필터링하지 않음)</p>
                <div className="grid gap-4">
                  <div className="grid gap-2">
                    <Label>최소 심각도</Label>
                    <Select
                      value={form.minSeverity || "all"}
                      onValueChange={(v) => setF({ minSeverity: v === "all" ? "" : v })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {SEVERITY_OPTIONS.map((o) => (
                          <SelectItem key={o.value || "all"} value={o.value || "all"}>
                            {o.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-inc">이 취약점 유형만 전송</Label>
                    <Textarea
                      id="n-inc"
                      placeholder={"SQL인젝션\n명령 실행"}
                      value={form.includeText}
                      onChange={(e) => setF({ includeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      한 줄에 키워드 하나. 대소문자를 구분하지 않는 부분 문자열로 비교합니다. 비워 두면=전체 유형.
                    </p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-exc">제외할 취약점 유형</Label>
                    <Textarea
                      id="n-exc"
                      placeholder={"정보 노출"}
                      value={form.excludeText}
                      onChange={(e) => setF({ excludeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">제외가 포함보다 우선합니다. 둘 다 일치하면 제외합니다.</p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-tasks">작업 제한 ID</Label>
                    <Input
                      id="n-tasks"
                      placeholder="1, 2, 3"
                      value={form.taskIDsText}
                      onChange={(e) => setF({ taskIDsText: e.target.value })}
                    />
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-assets">자산 제한 ID</Label>
                    <Input
                      id="n-assets"
                      placeholder="10, 11"
                      value={form.assetIDsText}
                      onChange={(e) => setF({ assetIDsText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">작업/자산을 비워 두면=제한 없음. 입력하면 취약점과 공통 자산이 있어야 합니다.</p>
                  </div>
                  <div className="flex items-center gap-2 text-sm">
                    <Switch
                      checked={form.onStatusChange}
                      onCheckedChange={(v) => setF({ onStatusChange: v })}
                      aria-label="상태 변경 수신"
                    />
                    취약점 처리 상태가 바뀔 때도 알림(실시간 모드만)
                  </div>
                </div>
              </div>

              <div className="flex items-center gap-2 text-sm">
                <Switch checked={form.enabled} onCheckedChange={(v) => setF({ enabled: v })} aria-label="활성화" />
                이 채널 활성화
              </div>
            </div>

            <div className="flex gap-2 pt-2 pb-6">
              <Button onClick={saveForm} disabled={saving}>
                {editing ? "저장" : "추가"}
              </Button>
              {editing && (
                <Button variant="outline" onClick={testChannel} disabled={testing}>
                  <SendIcon /> 테스트 메시지 보내기
                </Button>
              )}
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
