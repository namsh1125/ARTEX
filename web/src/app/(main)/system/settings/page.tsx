"use client";

import * as React from "react";

import { CpuIcon, FlaskConicalIcon, KeyboardIcon, RadioTowerIcon, SearchIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { CHAT_SEND_MODE_OPTIONS, type ChatSendMode, setChatSendMode, useChatSendMode } from "@/lib/chat-send-mode";
import type { Settings } from "@/lib/types";

import { UpdateCard } from "./_components/update-card";

export default function SystemSettingsPage() {
  const [trafficCapture, setTrafficCapture] = React.useState(false);
  const [agentTrafficBinding, setAgentTrafficBinding] = React.useState(false);
  const [webSearch, setWebSearch] = React.useState(false);
  const [backend, setBackend] = React.useState("ddgs");
  const [braveKeySet, setBraveKeySet] = React.useState(false);
  const [braveKeyInput, setBraveKeyInput] = React.useState("");
  const [tavilyKeySet, setTavilyKeySet] = React.useState(false);
  const [tavilyKeyInput, setTavilyKeyInput] = React.useState("");
  const [savingTavilyKey, setSavingTavilyKey] = React.useState(false);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [globalProxyInput, setGlobalProxyInput] = React.useState("");
  const [savingGlobalProxy, setSavingGlobalProxy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [savingKey, setSavingKey] = React.useState(false);
  const [pyInterp, setPyInterp] = React.useState("");
  const [workers, setWorkers] = React.useState("3");
  const [savingWorkers, setSavingWorkers] = React.useState(false);
  // 작업 제약 주입 범위(기본값은 모두 활성화).
  const [injectPlanner, setInjectPlanner] = React.useState(true);
  const [injectWorker, setInjectWorker] = React.useState(true);
  // 실험 기능: noa 컨텍스트 압축(기본 비활성화).
  const [noaCompaction, setNoaCompaction] = React.useState(false);
  // 프런트엔드 전용 설정으로 /api/settings 없이 localStorage를 직접 읽고 씁니다.
  const sendMode = useChatSendMode();

  const apply = React.useCallback((s: Settings) => {
    setTrafficCapture(!!s.traffic_capture);
    setAgentTrafficBinding(!!s.agent_traffic_binding);
    setWebSearch(!!s.web_search_enabled);
    setBackend(s.web_search_backend || "ddgs");
    setBraveKeySet(!!s.brave_key_set);
    setTavilyKeySet(!!s.tavily_key_set);
    setProxyInput(s.web_search_proxy ?? "");
    setGlobalProxyInput(s.global_proxy ?? "");
    setPyInterp(s.python_interpreter ?? "");
    setWorkers(String(s.workers ?? 3));
    setInjectPlanner(s.constraints_inject_planner !== false);
    setInjectWorker(s.constraints_inject_worker !== false);
    setNoaCompaction(!!s.noa_compaction);
  }, []);

  const saveWorkers = () => {
    const n = Number(workers);
    if (!Number.isInteger(n) || n <= 0) {
      toast.error("동시 실행 수는 0보다 큰 정수여야 합니다");
      return;
    }
    setSavingWorkers(true);
    api
      .setSettings({ workers: n })
      .then((s) => {
        apply(s);
        toast.success("동시 실행 작업 수를 저장했습니다 agent 수(이후 시작하는 작업에 적용)");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingWorkers(false));
  };

  const savePython = () => {
    setSaving(true);
    api
      .setSettings({ python_interpreter: pyInterp.trim() })
      .then((s) => {
        apply(s);
        toast.success("저장됨 Python 인터프리터 설정");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSaving(false));
  };
  const detectPython = () => {
    setSaving(true);
    api
      .detectPython()
      .then((r) => setPyInterp(r.python_interpreter))
      .catch(() => undefined)
      .finally(() => setSaving(false));
  };

  React.useEffect(() => {
    api
      .settings()
      .then(apply)
      .catch(() => undefined)
      .finally(() => setLoaded(true));
  }, [apply]);

  const toggleTraffic = (v: boolean) => {
    setTrafficCapture(v); // optimistic
    setSaving(true);
    api
      .setSettings({ traffic_capture: v })
      .then(apply)
      .catch(() => setTrafficCapture(!v)) // revert on failure
      .finally(() => setSaving(false));
  };

  const toggleInjectPlanner = (v: boolean) => {
    setInjectPlanner(v); // optimistic
    api
      .setSettings({ constraints_inject_planner: v })
      .then(apply)
      .catch(() => setInjectPlanner(!v)); // revert on failure
  };

  const toggleAgentTrafficBinding = (v: boolean) => {
    setAgentTrafficBinding(v);
    setSaving(true);
    api
      .setSettings({ agent_traffic_binding: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "켜짐 Agent 트래픽 자동 연결" : "꺼짐 Agent 트래픽 자동 연결");
      })
      .catch((e) => {
        setAgentTrafficBinding(!v);
        toast.error(`저장 실패: ${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleInjectWorker = (v: boolean) => {
    setInjectWorker(v); // optimistic
    api
      .setSettings({ constraints_inject_worker: v })
      .then(apply)
      .catch(() => setInjectWorker(!v)); // revert on failure
  };

  const toggleNoaCompaction = (v: boolean) => {
    setNoaCompaction(v); // optimistic
    api
      .setSettings({ noa_compaction: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "noa 컨텍스트 압축을 켰습니다(이후 시작하는 실행에 적용)" : "noa 컨텍스트 압축을 껐습니다(기본 압축 복원)");
      })
      .catch((e) => {
        setNoaCompaction(!v); // revert on failure
        toast.error(`저장 실패: ${(e as Error).message}`);
      });
  };

  // Persist a web-search patch (enable and/or backend). Optimistic with refetch.
  const saveWebSearch = (patch: Partial<Settings>) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        apply(s);
        toast.success("웹 검색 설정을 저장했습니다");
      })
      .catch((e) => {
        toast.error("저장 실패: " + (e as Error).message);
        api
          .settings()
          .then(apply)
          .catch(() => undefined);
      })
      .finally(() => setSaving(false));
  };

  const saveBraveKey = () => {
    setSavingKey(true);
    api
      .setSettings({ brave_search_api_key: braveKeyInput })
      .then((s) => {
        apply(s);
        setBraveKeyInput("");
        toast.success("저장됨 Brave API Key");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingKey(false));
  };

  const saveTavilyKey = () => {
    setSavingTavilyKey(true);
    api
      .setSettings({ tavily_search_api_key: tavilyKeyInput })
      .then((s) => {
        apply(s);
        setTavilyKeyInput("");
        toast.success("저장됨 Tavily API Key");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingTavilyKey(false));
  };

  const saveProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ web_search_proxy: proxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(proxyInput.trim() ? "외부 연결 프록시를 저장했습니다" : "외부 연결 프록시를 해제했습니다(직접 연결로 전환)");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingProxy(false));
  };

  const saveGlobalProxy = () => {
    setSavingGlobalProxy(true);
    api
      .setSettings({ global_proxy: globalProxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(globalProxyInput.trim() ? "전역 프록시를 저장했습니다" : "전역 프록시를 해제했습니다(직접 연결로 전환)");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingGlobalProxy(false));
  };

  // Run a real "test" search ("test") against the CURRENT form values (backend +
  // proxy + entered key), falling back to saved values server-side. Toasts result.
  const runTest = () => {
    setTesting(true);
    api
      .testWebSearch({
        web_search_backend: backend,
        web_search_proxy: proxyInput.trim(),
        brave_search_api_key: braveKeyInput,
        tavily_search_api_key: tavilyKeyInput,
      })
      .then((r) => {
        if (r.ok) toast.success(`검색 테스트 성공 · ${r.backend} 돌아가기 ${r.count} 개 결과`);
        else toast.error("검색 테스트 실패: " + (r.error || "알 수 없는 오류"));
      })
      .catch((e) => toast.error("검색 테스트 실패: " + (e as Error).message))
      .finally(() => setTesting(false));
  };

  // brave-free selected but no key stored and none being entered → tool stays off.
  const braveNeedsKey = webSearch && backend === "brave-free" && !braveKeySet;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">시스템 설정</h1>
        <p className="text-muted-foreground text-sm">전역 런타임 스위치</p>
      </div>

      {/* grid 대신 여러 열 사용: 검색 카드는 다른 카드보다 높고 brave/tavily 등 공급자에 따라 높이가 달라지므로
          의 key 입력은 조건부 렌더링）。grid 가장 높은 카드 기준으로 행 높이가 정해져 주변에 큰 빈 공간이 생김，
          다단 레이아웃은 내용 높이에 맞춰 균형 있게 채움. 카드 간격은 다음으로 설정 mb 대신 사용하지 않음: gap——다단 레이아웃에서
          column-gap 열 간격만 적용하므로 행 간격은 자식 요소에서 설정。 */}
      <div className="columns-1 gap-4 md:gap-6 lg:columns-2">
        <UpdateCard />

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              트래픽 캡처
            </CardTitle>
            <CardDescription>
              활성화하면 모든 Agent의 HTTP 트래픽을 기록 프록시로 저장합니다. Agent에 traffic_search / traffic_get 도구와 프록시 설정을 주입하고 프롬프트에 프록시 안내를 포함합니다.
              <br />
              비활성화(기본값)하면 트래픽을 기록하지 않습니다: Agent
              <b>하지 않음</b>프록시 설정과 트래픽 도구를 받지 않으며 프롬프트에도<b>포함하지 않음</b>프록시 관련 내용. 전환하면 즉시 다시 생성 Agent 적용.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="traffic-capture" className="text-sm font-normal text-muted-foreground">
              {trafficCapture ? "활성화됨 · 트래픽 기록 및 프록시 설정 주입 중" : "비활성화됨 · 기록 및 프록시 설정 주입 안 함"}
            </Label>
            <Switch
              id="traffic-capture"
              checked={trafficCapture}
              disabled={!loaded || saving}
              onCheckedChange={toggleTraffic}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Agent 트래픽 자동 연결
            </CardTitle>
            <CardDescription id="agent-traffic-binding-description">
              기본적으로 꺼져 있습니다. 켜면 취약점 저장 시 보고서 Agent가 기존 HTTP 요청·응답을 대조하고 관련 트래픽을 연결한 뒤 보고서를 작성합니다.
              <b>패킷 확인과 추가 도구 호출로 토큰 사용량이 증가합니다.</b>
              <br />
              TCP이거나 캡처 또는 일치하는 트래픽이 없어도 보고할 수 있습니다. 트래픽 캡처, 수동 연결, 저장된 증거 보기에는 영향을 주지 않습니다.
              다음 Agent 실행부터 적용되며, 끄면 새 자동 연결 요청을 즉시 거부합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="agent-traffic-binding" className="text-sm font-normal text-muted-foreground">
              {agentTrafficBinding ? "활성화됨 · 토큰 사용량 증가" : "비활성화됨 · 수동 연결 가능"}
            </Label>
            <Switch
              id="agent-traffic-binding"
              aria-describedby="agent-traffic-binding-description"
              checked={agentTrafficBinding}
              disabled={!loaded || saving}
              onCheckedChange={toggleAgentTrafficBinding}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              전역 프록시
            </CardTitle>
            <CardDescription>
              모든 Agent의 <b>대상 트래픽</b>을 이 프록시로 전달합니다(출발지 IP 숨김 / 중계 서버 경유).
              <b>http / https / socks5</b>와 <code>user:pass</code> 인증을 지원합니다. 비워 두면 직접 연결합니다.
              <br />
              <b>트래픽 캡처</b>가 켜져 있으면 기록 프록시의 <b>상위 프록시</b>로 사용하여 트래픽을 저장한 뒤 외부로 연결합니다.
              캡처가 꺼져 있으면 Agent의 bash / WebFetch에 직접 주입합니다. 웹 검색 및 LLM 프록시와는 독립적입니다.
              <br />
              <b>참고</b>: <b>캡처가 꺼진 상태</b>에서 socks5 사용 여부는 각 명령줄 도구의 <code>ALL_PROXY</code> 지원에 달려 있습니다.
              curl은 지원하지만 일부 도구는 무시할 수 있습니다. socks5를 주로 사용한다면 트래픽 캡처를 켜는 것이 좋습니다.
              이 경우 MITM이 직접 연결하므로 도구 설정과 무관하게 적용됩니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <Label htmlFor="global-proxy" className="text-sm font-normal text-muted-foreground">
              프록시 주소
            </Label>
            <div className="flex items-center gap-2">
              <Input
                id="global-proxy"
                autoComplete="off"
                placeholder="socks5://user:pass@host:1080 또는 http://host:port(비워 두기=직접 연결)"
                value={globalProxyInput}
                disabled={!loaded || savingGlobalProxy}
                onChange={(e) => setGlobalProxyInput(e.target.value)}
              />
              <Button type="button" onClick={saveGlobalProxy} disabled={!loaded || savingGlobalProxy}>
                저장
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              {globalProxyInput.trim() ? "설정됨 · 모든 대상 트래픽이 이 프록시를 통해 연결됨" : "미설정 · 대상 트래픽이 직접 연결됨"}
            </p>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <ShieldAlertIcon className="size-4" />
              작업 제약 주입
            </CardTitle>
            <CardDescription>
              활성화하면 각 작업의 <b>작업 제약</b>(작업 개요에서 관리하는 allow/deny 항목)을 Agent의 시스템 프롬프트에 추가하여
              탐색 범위를 제한합니다(예: ‘현재 포트만 테스트’, ‘무차별 대입 금지’).
              <br />
              <b>계획 담당자(planner)</b>와 <b>실행 담당자(worker)</b>의 주입 여부를 각각 설정할 수 있으며 기본적으로 모두 켜져 있습니다.
              다음 읽기부터 즉시 적용되므로 Agent를 다시 만들 필요가 없습니다. 끄면 해당 Agent에 제약이 표시되지 않습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-planner" className="text-sm font-normal text-muted-foreground">
                계획 담당자에 주입(planner){injectPlanner ? " · 켜짐" : " · 꺼짐"}
              </Label>
              <Switch
                id="inject-planner"
                checked={injectPlanner}
                disabled={!loaded}
                onCheckedChange={toggleInjectPlanner}
              />
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-worker" className="text-sm font-normal text-muted-foreground">
                실행 담당자에 주입(worker){injectWorker ? " · 켜짐" : " · 꺼짐"}
              </Label>
              <Switch
                id="inject-worker"
                checked={injectWorker}
                disabled={!loaded}
                onCheckedChange={toggleInjectWorker}
              />
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <FlaskConicalIcon className="size-4" />
              실험 기능
            </CardTitle>
            <CardDescription>
              검증 중인 기능으로 기본적으로 꺼져 있습니다. Agent 동작과 안정성에 영향을 줄 수 있으므로 내용을 이해한 후 켜세요.
              <br />
              <b>noa 컨텍스트 압축</b>: 모델이 긴 대화 기록을 능동적으로 압축합니다(norma v0.4.0). 활성화하면
              <b>계획 담당자 / 실행 담당자 / 주 Agent / 대화</b>의 컨텍스트를 noa로 관리하여 기본 압축을 대체합니다.
              압축 원문은 추적할 수 있도록 작업 디렉터리에 보관합니다. 다음 실행부터 적용되므로 Agent를 다시 만들 필요가 없습니다.
              끄면 즉시 기본 압축으로 돌아갑니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="noa-compaction" className="text-sm font-normal text-muted-foreground">
              noa 컨텍스트 압축{noaCompaction ? " · 켜짐" : " · 꺼짐"}
            </Label>
            <Switch
              id="noa-compaction"
              checked={noaCompaction}
              disabled={!loaded}
              onCheckedChange={toggleNoaCompaction}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <SearchIcon className="size-4" />
              웹 검색
            </CardTitle>
            <CardDescription>
              웹 검색의 <b>전체 스위치와 공급자</b>를 설정합니다. 활성화한 뒤 <b>각 Agent 설정</b>에서 <b>web_search</b>를 개별적으로 켤 수 있습니다.
              제목·링크·요약만 반환하며 본문은 WebFetch가 가져옵니다. 웹 검색은 기록 프록시를 거치지 않으며 트래픽 캡처와 독립적입니다.
              <br />
              공급자는 <b>DuckDuckGo(ddgs)</b>(키 불필요), <b>Brave(무료 버전)</b>(Brave API Key 필요),{" "}
              <b>Tavily</b>(Tavily API Key 필요), <b>DeepSeek</b>(현재 LLM 설정 재사용) 중에서 선택합니다.
              전체 스위치가 꺼져 있으면 각 Agent에서 웹 검색을 켤 수 없습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="web-search" className="text-sm font-normal text-muted-foreground">
                {webSearch ? "전체 스위치 활성화됨 · 각 Agent 설정에서 개별 활성화 가능" : "꺼짐 · 각 Agent 웹 검색을 활성화할 수 없습니다"}
              </Label>
              <Switch
                id="web-search"
                checked={webSearch}
                disabled={!loaded || saving}
                onCheckedChange={(v) => {
                  setWebSearch(v); // optimistic
                  saveWebSearch({ web_search_enabled: v });
                }}
              />
            </div>

            {webSearch && (
              <div className="flex items-center justify-between gap-4">
                <Label className="text-sm font-normal text-muted-foreground">검색 공급자</Label>
                <Select
                  value={backend}
                  disabled={!loaded || saving}
                  onValueChange={(v) => {
                    setBackend(v); // optimistic
                    saveWebSearch({ web_search_backend: v });
                  }}
                >
                  <SelectTrigger className="w-48 shrink-0">
                    <SelectValue placeholder="공급자 선택" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ddgs">DuckDuckGo(ddgs · 무료, 키 불필요)</SelectItem>
                    <SelectItem value="brave-free">Brave(무료 버전 · 키 필요)</SelectItem>
                    <SelectItem value="tavily">Tavily(키 필요)</SelectItem>
                    <SelectItem value="deepseek">DeepSeek(공식)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}

            {webSearch && backend === "deepseek" && (
              <div className="border-border/60 bg-muted/30 flex flex-col gap-2 rounded-md border p-3">
                <p className="text-sm font-medium">DeepSeek 공식 웹 검색</p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  이 공급자는 <b>현재 활성화된 LLM 설정</b>을 재사용합니다. <b>DeepSeek 공식 모델</b>만 지원하며
                  해당 설정은 <b>anthropic 프로토콜을 사용해야 합니다</b>. DeepSeek의 OpenAI 프로토콜 엔드포인트는 서버 검색을 지원하지 않습니다.
                  LLM 설정을 바꾸면 이 공급자를 사용할 수 없게 될 수 있습니다.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  검색은 <b>DeepSeek 서버에서 실행</b>됩니다. 검색마다 모델을 한 번 더 호출하므로 토큰 비용이 발생합니다.
                  검색 요청은 <b>외부 연결 프록시를 거치지 않으며</b> <b>트래픽 기록에도 포함되지 않습니다</b>.
                  결과에는 요약 없이 <b>제목과 링크만 포함</b>됩니다. 본문은 WebFetch로 가져옵니다.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  위 조건 충족 여부는 직접 확인해야 하며 시스템은 차단하지 않습니다. 아래 「검색 테스트」로 실제 호출하여 확인할 수 있습니다.
                </p>
              </div>
            )}

            {webSearch && backend === "brave-free" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="brave-key" className="text-sm font-normal text-muted-foreground">
                  Brave Search API Key
                  {braveKeySet && <span className="ml-2 text-xs text-emerald-500">설정됨</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="brave-key"
                    type="password"
                    autoComplete="off"
                    placeholder={braveKeySet ? "설정됨(비워 두면 유지)" : "Brave API Key 입력"}
                    value={braveKeyInput}
                    disabled={!loaded || savingKey}
                    onChange={(e) => setBraveKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveBraveKey}
                    disabled={!loaded || savingKey || braveKeyInput.trim() === ""}
                  >
                    저장
                  </Button>
                </div>
                {braveNeedsKey && (
                  <p className="text-xs text-amber-500">
                    Brave를 선택했지만 API Key가 설정되지 않았습니다. 키를 저장해야 검색 도구가 활성화됩니다.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  무료 버전 할당량은 약 월 2,000회입니다. https://brave.com/search/api/ 에서 키를 발급받으세요.
                </p>
              </div>
            )}

            {webSearch && backend === "tavily" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="tavily-key" className="text-sm font-normal text-muted-foreground">
                  Tavily Search API Key
                  {tavilyKeySet && <span className="ml-2 text-xs text-emerald-500">설정됨</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="tavily-key"
                    type="password"
                    autoComplete="off"
                    placeholder={tavilyKeySet ? "설정됨(비워 두면 유지)" : "Tavily API Key 입력(tvly-…)"}
                    value={tavilyKeyInput}
                    disabled={!loaded || savingTavilyKey}
                    onChange={(e) => setTavilyKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveTavilyKey}
                    disabled={!loaded || savingTavilyKey || tavilyKeyInput.trim() === ""}
                  >
                    저장
                  </Button>
                </div>
                {webSearch && backend === "tavily" && !tavilyKeySet && (
                  <p className="text-xs text-amber-500">
                    Tavily를 선택했지만 API Key가 설정되지 않았습니다. 키를 저장해야 검색 도구가 활성화됩니다.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">https://tavily.com 에 가입하여 API Key를 발급받으세요.</p>
              </div>
            )}

            {webSearch && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="ws-proxy" className="text-sm font-normal text-muted-foreground">
                  외부 연결 프록시(선택 사항)
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="ws-proxy"
                    autoComplete="off"
                    placeholder="http://host:port 또는 socks5://host:port(비워 두기=직접 연결)"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => setProxyInput(e.target.value)}
                  />
                  <Button type="button" onClick={saveProxy} disabled={!loaded || savingProxy}>
                    저장
                  </Button>
                </div>
                <p className="text-muted-foreground text-xs">
                  검색 엔드포인트 접속에만 사용하는 독립 프록시(VPN/SOCKS 등). 트래픽을 기록하는 MITM 프록시와는 무관합니다. 연결이 안 되면 이 프록시를 통해 접속합니다.
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex items-center justify-between gap-4 border-t pt-4">
                <p className="text-muted-foreground text-xs">
                  현재 설정(공급자 + 프록시 + 키)으로 ‘test’를 실제 검색하여 사용 가능 여부를 확인합니다.
                </p>
                <Button
                  type="button"
                  variant="outline"
                  onClick={runTest}
                  disabled={!loaded || testing}
                  className="shrink-0"
                >
                  {testing ? "테스트 중…" : "검색 테스트"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              사용자 지정 스크립트 · Python 인터프리터
            </CardTitle>
            <CardDescription>
              사용자 지정 <b>script</b> 도구의 Python 실행에 사용합니다. 시작 시 자동 감지하며 python3를 우선합니다.
              venv 또는 특정 버전의 절대 경로를 직접 입력할 수 있습니다. 비워 두면 실행 시 자동 감지합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                className="font-mono text-sm"
                placeholder="/usr/bin/python3(비워 두기=자동 감지)"
                value={pyInterp}
                disabled={!loaded || saving}
                onChange={(e) => setPyInterp(e.target.value)}
              />
              <Button variant="outline" onClick={detectPython} disabled={!loaded || saving}>
                다시 감지
              </Button>
              <Button onClick={savePython} disabled={!loaded || saving}>
                저장
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <CpuIcon className="size-4" />
              작업 동시 실행 · Work Agent 수
            </CardTitle>
            <CardDescription>
              작업마다 동시에 실행할 worker 수입니다(기본값 3). 값이 클수록 동시 탐색 수와 사용량이 증가합니다.
              변경 사항은 <b>이후 시작하는 작업에 적용</b>되며 이미 실행 중인 작업에는 영향을 주지 않습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min={1}
                className="w-32 font-mono text-sm"
                placeholder="3"
                value={workers}
                disabled={!loaded || savingWorkers}
                onChange={(e) => setWorkers(e.target.value)}
              />
              <Button onClick={saveWorkers} disabled={!loaded || savingWorkers}>
                저장
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyboardIcon className="size-4" />
              세션 입력창 전송 단축키
            </CardTitle>
            <CardDescription>
              대화 페이지와 작업 상세의 주 Agent 세션 입력창에서 공유합니다. 선택 즉시 적용되며 저장할 필요가 없습니다.
              <br />
              이 설정은<b>현재 브라우저에만 저장됩니다</b>, 계정에 동기화되지 않으며 브라우저를 바꾸거나 사이트 데이터를 지우면 다시 설정해야 합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="chat-send-mode" className="text-sm font-normal text-muted-foreground">
              전송 방식
            </Label>
            <Select value={sendMode} onValueChange={(v) => setChatSendMode(v as ChatSendMode)}>
              <SelectTrigger id="chat-send-mode" className="w-72">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CHAT_SEND_MODE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
