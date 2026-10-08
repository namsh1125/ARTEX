// 채널 필드 정의와 설정 값 파싱 도구.
//
// 화면이 아닌 데이터이므로 페이지와 분리합니다. 채널별 필드와 컨트롤,
// 폼 텍스트와 JSON 설정 값의 양방향 변환을 정의합니다.
// 별도 파일로 두어 새 채널 추가 시 페이지를 수정하지 않아도 됩니다.
// 채널 표시 이름과 설명은 UI 문구에만 영향을 주므로 프런트엔드에 둡니다.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Feishu",
  wecom: "WeCom",
  webhook: "일반 Webhook",
  telegram: "Telegram",
  email: "이메일",
};

// 채널별 설정 필드 정의.
//
// 백엔드 schema를 내려받는 대신 프런트엔드 필드 테이블을 유지합니다. 백엔드는 필수/형식 검증,
// UI는 배치와 컨트롤 유형을 담당하므로 관심사가 다릅니다.
// 유일한 연결점은 secret_keys입니다. 암호 입력란으로 표시할 필드를 백엔드가 알려줍니다.
// 자격 증명 여부는 채널 구현만 정확히 알 수 있습니다. WeCom은 전체 Webhook이 자격 증명이고,
// DingTalk은 secret 하나만 해당합니다. 새 채널 정의가 누락되면 폼이 비지만
// 아래 hasFields가 안내하므로 조용히 오류가 생기지는 않습니다.
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "서명 키",
      kind: "password",
      help: "봇 보안 설정에서 「서명」을 선택한 경우 입력합니다. 「사용자 지정 키워드」를 선택했거나 보안 설정이 꺼져 있으면 비워 두세요",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "서명 검증 키", kind: "password", help: "봇의 「서명 검증」이 켜져 있으면 입력하고 그렇지 않으면 비워 두세요" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "목표 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "요청 메서드",
      kind: "select",
      options: [
        { value: "POST", label: "POST(요청 본문 포함)" },
        { value: "PUT", label: "PUT(요청 본문 포함)" },
        { value: "PATCH", label: "PATCH(요청 본문 포함)" },
        { value: "GET", label: "GET(요청 본문 없음)" },
      ],
    },
    { key: "headers", label: "사용자 지정 요청 헤더", kind: "kv", help: "한 줄에 하나 KEY=VALUE, 예: Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "요청 본문 템플릿",
      kind: "textarea",
      help:
        "비워 두면 기본 템플릿을 사용합니다. 변수: {{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}},  " +
        "및 range .Items 아래의 .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel." +
        "문자열에는 {{.Xxx}} 대신 {{json .Xxx}}를 사용하세요. 그렇지 않으면 제목의 따옴표로 JSON 형식이 깨질 수 있습니다.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API 주소",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "비워 두면 공식 주소를 사용합니다. 직접 구축한 Bot API 리버스 프록시를 사용하는 경우 입력하세요",
    },
  ],
  email: [
    { key: "host", label: "SMTP 서버", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "포트",
      kind: "number",
      placeholder: "587",
      help: "587 사용 STARTTLS; 465 「암시적 TLS」열기",
    },
    { key: "username", label: "계정", kind: "text" },
    { key: "password", label: "비밀번호 / 인증 코드", kind: "password" },
    { key: "from", label: "보내는 사람", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "받는 사람", kind: "list", help: "여러 주소는 쉼표로 구분" },
    { key: "tls", label: "암시적 TLS", kind: "switch", help: "465 포트에서는 활성화; 587 비활성 상태 유지(자동 전환: STARTTLS)" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "제한 없음" },
  { value: "low", label: "낮음 이상" },
  { value: "medium", label: "중간 이상" },
  { value: "high", label: "높음 이상" },
  { value: "critical", label: "심각만" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV는 줄마다 KEY=VALUE인 텍스트를 파싱합니다.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs는 쉼표 또는 공백으로 구분한 ID 목록을 파싱합니다.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords는 공백을 포함할 수 있는 취약점 이름을 줄 또는 쉼표로 구분합니다.
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
