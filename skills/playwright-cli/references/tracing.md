# 트레이싱

디버깅과 분석을 위해 상세 실행 추적을 기록합니다. 추적에는 DOM 스냅샷, 스크린샷, 네트워크 활동, 콘솔 로그가 포함됩니다.

## 기본 사용법

```bash
# Start trace recording
playwright-cli tracing-start

# Perform actions
playwright-cli open https://example.com
playwright-cli click e1
playwright-cli fill e2 "test"

# Stop trace recording
playwright-cli tracing-stop
```

## 추적 출력 파일

추적을 시작하면 Playwright는 여러 파일이 포함된 `traces/` 디렉터리를 만듭니다.

### `trace-{timestamp}.trace`

**작업 로그**: 기본 추적 파일에 포함되는 내용:
- 수행한 모든 작업(클릭, 입력, 탐색)
- 각 작업 전후의 DOM 스냅샷
- 각 단계의 스크린샷
- 시간 정보
- 콘솔 메시지
- 소스 위치

### `trace-{timestamp}.network`

**네트워크 로그**: 전체 네트워크 활동:
- 모든 HTTP 요청과 응답
- 요청 헤더와 본문
- 응답 헤더와 본문
- 시간 정보(DNS, 연결, TLS, TTFB, 다운로드)
- 리소스 크기
- 실패한 요청과 오류

### `resources/`

**리소스 디렉터리**: 캐시된 리소스:
- 이미지, 글꼴, 스타일시트, 스크립트
- 재생에 사용할 응답 본문
- 페이지 상태 재구성에 필요한 자산

## 추적에 기록되는 내용

| 분류 | 상세 |
|----------|---------|
| **작업** | 클릭, 입력, 호버, 키보드 입력, 탐색 |
| **DOM** | 각 작업 전후의 전체 DOM 스냅샷 |
| **스크린샷** | 각 단계의 시각적 상태 |
| **네트워크** | 모든 요청, 응답, 헤더, 본문, 시간 정보 |
| **콘솔** | 모든 console.log, warn, error 메시지 |
| **시간** | 각 작업의 정확한 시간 정보 |

## 사용 사례

### 실패한 작업 디버깅

```bash
playwright-cli tracing-start
playwright-cli open https://app.example.com

# This click fails - why?
playwright-cli click e5

playwright-cli tracing-stop
# Open trace to see DOM state when click was attempted
```

### 성능 분석

```bash
playwright-cli tracing-start
playwright-cli open https://slow-site.com
playwright-cli tracing-stop

# View network waterfall to identify slow resources
```

### 증거 기록

```bash
# Record a complete user flow for documentation
playwright-cli tracing-start

playwright-cli open https://app.example.com/checkout
playwright-cli fill e1 "4111111111111111"
playwright-cli fill e2 "12/25"
playwright-cli fill e3 "123"
playwright-cli click e4

playwright-cli tracing-stop
# Trace shows exact sequence of events
```

## 추적·동영상·스크린샷 비교

| 기능 | 추적 | 동영상 | 스크린샷 |
|---------|-------|-------|------------|
| **형식** | .trace 파일 | .webm 동영상 | .png/.jpeg 이미지 |
| **DOM 검사** | 가능 | 불가 | 불가 |
| **네트워크 상세** | 있음 | 없음 | 없음 |
| **단계별 재생** | 가능 | 연속 재생 | 단일 프레임 |
| **파일 크기** | 중간 | 큼 | 작음 |
| **적합한 용도** | 디버깅 | 데모 | 빠른 캡처 |

## 권장 사항

### 1. 문제 발생 전에 추적 시작

```bash
# Trace the entire flow, not just the failing step
playwright-cli tracing-start
playwright-cli open https://example.com
# ... all steps leading to the issue ...
playwright-cli tracing-stop
```

### 2. 오래된 추적 정리

추적은 상당한 디스크 공간을 사용할 수 있습니다.

```bash
# Remove traces older than 7 days
find .playwright-cli/traces -mtime +7 -delete
```

## 한계

- 추적은 자동화에 부하를 추가합니다.
- 큰 추적 파일은 상당한 디스크 공간을 사용할 수 있습니다.
- 일부 동적 콘텐츠는 완벽하게 재생되지 않을 수 있습니다.
