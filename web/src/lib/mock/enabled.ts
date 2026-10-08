// Mock 스위치는 빌드 시 주입하는 공개 변수이며 NEXT_PUBLIC_ 접두사가 있어야 브라우저에서 읽을 수 있습니다.
// Vercel에서 NEXT_PUBLIC_MOCK=1이면 백엔드 없이 전체 사이트가 mock을 사용합니다.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
