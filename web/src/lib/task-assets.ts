import type { NewAssetType } from "@/lib/types";

const ASSET_TYPE_LABELS: Record<NewAssetType, string> = {
  app: "애플리케이션",
  endpoint: "인터페이스",
  ip: "IP",
  root_domain: "루트 도메인",
  service: "서비스",
  subdomain: "하위 도메인",
};

const TASK_ASSET_SOURCE_LABELS: Record<string, string> = {
  agent: "Agent 발견 사항",
  anchor: "공유 보드 앵커",
  api: "자산 API",
  company: "기업 연결",
  legacy: "과거 연결",
  manual: "수동 추가",
  system: "시스템 연결",
  task: "작업 초기화",
};

export function taskAssetTypeLabel(type: NewAssetType): string {
  return ASSET_TYPE_LABELS[type];
}

export function taskAssetSourceLabel(source: string): string {
  return TASK_ASSET_SOURCE_LABELS[source] ?? source;
}
