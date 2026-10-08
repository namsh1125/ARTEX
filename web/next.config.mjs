import { fileURLToPath } from "node:url";

// 정적 내보내기: `NEXT_EXPORT=1 next build`로 web/out에 정적 디렉터리를 생성하며
// nginx 웹 루트에 바로 배치할 수 있습니다. next dev에서는 이 변수를 생략해 /api 프록시와 핫 리로드를 유지합니다.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel 데모: 전체 사이트가 mock을 사용하므로 백엔드와 /api 프록시가 필요하지 않습니다.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 상위 디렉터리 lockfile이 루트 추론 및 리소스 경로 생성에 영향을 주지 않도록 합니다.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // 로컬 네트워크 IP에서 개발 리소스(HMR)에 접근하도록 허용합니다. 필요시 수정하세요.
  // 개발 중 모든 IPv4에서 /_next/*와 HMR 접근 허용(로컬 네트워크 IP가 바뀌어도 동작).
  // Next는 보안상 단독 "*"를 금지하므로 구간별 와일드카드 "*.*.*.*"로 IPv4를 매칭합니다.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 정적 내보내기: Node 런타임과 이미지 최적화 없이 라우트마다 <route>/index.html 생성.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock 데모에는 백엔드와 /api 프록시가 필요하지 않습니다.
          images: { unoptimized: true },
        }
      : {
          // 개발: /api/*를 Go 백엔드로 프록시(기본 :8787, AUTOPENTEST_API로 변경 가능).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
