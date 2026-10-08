import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// Sheet/Dialog의 onInteractOutside 닫힘 판단 보조 함수.
//
// 서랍 내부의 Radix Select/DropdownMenu/Popover는 서랍 밖으로 포털됩니다.
// 팝오버를 닫으려고 배경을 누르면 Select와 Sheet의 DismissableLayer가 같은 pointerdown을 처리합니다.
// Select가 먼저 닫히고 discrete 이벤트로 React가 즉시 flush하므로
// Sheet 처리 시에는 이미 data-state가 closed입니다. 따라서 그 시점에 열린 상태를
// 검사하는 방식은 신뢰할 수 없으며 실제 테스트로 확인했습니다.
//
// Radix는 버블링 단계에서 pointerdown을 처리하므로 그보다 앞선 capture 단계에서
// 열린 팝오버 유무를 기록하고 onInteractOutside에서 그 기록으로 닫힘 여부를 판단합니다.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture:다음보다 먼저 Radix 버블링 단계의 pointerdown 핸들러보다 먼저 기록
  );
}

// radixOverlayWasOpenAtPointerDown은 최근 pointerdown 당시 Radix 팝오버가 열려 있었는지 반환합니다.
// 이를 이용해 팝오버가 열린 상태에서 배경 클릭 시 팝오버만 닫고 서랍/모달은 유지합니다.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText는 클립보드에 텍스트를 쓰고 성공 여부를 반환합니다.
// navigator.clipboard는 HTTPS/localhost에서만 제공되며 IP+HTTP에서는
// undefined이므로 execCommand("copy")로 대체합니다.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 대체 방식으로 계속 진행합니다.
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "ko-KR", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
