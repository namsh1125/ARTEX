"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// 대화 입력란의 전송/줄바꿈 키 설정. localStorage에만 저장하며 계정과 동기화하지 않으므로
// 브라우저를 바꾸면 다시 설정해야 합니다. issue #39: 0.3.2에서 Ctrl+Enter를 Enter로 바꾼 뒤
// 이전 키 설정을 선택 옵션으로 다시 제공합니다.
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter 보내기, Shift+Enter 줄바꿈" },
  { value: "ctrl-enter", label: "Ctrl+Enter 보내기, Enter 줄바꿈" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// 같은 탭의 구독자 집합. storage 이벤트는 다른 탭에서만 발생하므로
// emit으로 현재 탭 입력란에 알려 새로고침 없이 설정을 적용합니다.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// 문자열 리터럴은 Object.is가 값으로 비교하므로 useSyncExternalStore가 반복되지 않습니다.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// 서버에는 localStorage가 없어 기본값으로 렌더링하고 hydration 후 getSnapshot으로 보정합니다.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey는 키 입력이 전송 동작인지 판단합니다.
// isComposing/keyCode 229는 한글·중국어 등의 조합 중이므로 Enter가 문자 확정 대신 전송되지 않게 합니다.
// enter 모드는 Shift만 제외하여 0.3.2와 같은 기본 조작을 유지합니다.
// ctrl-enter 모드는 Ctrl과 macOS Cmd 모두 허용합니다.
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
