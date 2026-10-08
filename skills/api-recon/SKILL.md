---
name: api-recon
description: 웹사이트의 API 인터페이스를 수집할 때 사용하는 스킬입니다.
---

# API Recon(프런트엔드 인터페이스 정찰)

**승인된 범위 안에서** **백엔드 API**(경로, 메서드, 매개변수, 응답 본문), **프런트엔드 라우트**, **UI 기능 실행 지점**(탭, 대화상자, 테이블 작업 등)을 최대한 빠짐없이 발견합니다.

---

## 범위와 금지 사항(에이전트 필독: 위반 시 범위 이탈)

이 스킬은 **API와 매개변수 조사만** 수행합니다. 취약점 탐색이나 침투·악용 단계가 아닙니다.

### 작업 범위

| 범위 | 허용 | 금지 |
|---|---|---|
| **대상** | 경로, 메서드, 매개변수, 라우트, UI 실행 지점 열거 | SQLi/XSS/권한 우회/무차별 대입/퍼징 취약점 테스트, 패킷 변조 공격, 파괴적 작업 |
| **인증** | Hook + stub/mock으로 **클라이언트 측** 로그인 게이트 우회 | 사용자에게 계정·암호 요청, 추측, 실제 로그인 폼 제출 |
| **런타임** | 자격 증명 없이 인터페이스를 후킹하고 mock 응답으로 SPA의 로그인 후 화면 틀 렌더링 | 실제 백엔드 세션이 있어야 진행할 수 있는 흐름 |

### 자격 증명 없는 동적 분석(Phase 3 기본값)

1. `preload.js` / `runtime_harvest.js`로 로그인, 권한, 메뉴 등의 초기화 인터페이스를 **가로채 stub 처리**합니다.
2. 업무 조회 인터페이스에는 **올바른 구조와 성공 업무 코드를 갖되 데이터는 비어 있어도 되는** mock 본문을 반환합니다.
3. 백엔드가 없거나 401이 발생해도 프런트엔드가 로그인 후 화면을 렌더링하게 하여 더 많은 XHR/fetch/WebSocket을 발생시킵니다.
4. **빈 데이터, 빈 테이블, 자리표시자 UI는 정상적인 결과**입니다. 이를 이유로 실제 로그인이나 취약점 테스트로 전환하지 마세요.

**핵심**: mock으로 프런트엔드 라우트와 컴포넌트를 마운트하고 **발신 요청만 기록**합니다. 백엔드 반환값보다 **프런트엔드가 어떤 인터페이스를 더 호출하는지**가 중요합니다.

### 절대 금지되는 진행 방식

| 금지 | 대안 |
|---|---|
| Phase 1 완료 전에 주 진입점 `index-*.js`를 grep/curl/Read로 읽어 API 경로 추출 | `OUTDIR/harvest_static.py` 실행 |
| harvest 대신 `extract_apis.py` 등의 스크립트 직접 작성 | `OUTDIR/harvest_static.py` 수정 후 재실행 |
| 같은 grep/명령이 2회 이상 실패해도 반복 | tool_logs 확인, harvest 수정, reference 조회 등으로 전략 변경 |
| 게이트 A/B를 생략하고 `scripts/` 원본을 바로 실행 | OUTDIR로 복사하고 대상에 맞게 수정 |
| 실제 사용자 이름/암호, OTP, OAuth 등으로 인증 | stub/mock 사용(위 설명 참조) |
| 실제 데이터를 얻겠다는 이유로 stub을 생략하고 권한 우회/주입 테스트 | 정찰 범위 안에서 발신 요청만 기록 |
| 삭제, 민감 데이터 내보내기, 대량 쓰기 등 되돌릴 수 없는 작업 | coverage 클릭에도 같은 금지 적용 |
| 런타임 분석과 동적 열거를 끝내지 않고 모든 페이지·인터페이스를 확보했다고 주장 | 완료 정의를 따르거나 한계 명시 |
| 매개변수 실행 매트릭스와 diff 없이 모든 매개변수를 파악했다고 주장 | Phase 3b 매트릭스 + Phase 5 diff 수행 |
| 런타임 표본 하나로 필수/선택 여부 추론 | 여러 표본의 diff 또는 검증 규칙·오류로 역추론 |

---

## 두 계층 모델과 실행 모드

| 계층 | 산출물 | 한계 |
|---|---|---|
| **정적**(JS 번들) | 전체 엔드포인트 경로, 라우트 초안, 요청 구성 지점의 필드 후보 | HTTP 메서드 없음. 매개변수는 Phase 1b 필요. 런타임에 조합되는 URL 누락 |
| **런타임**(실행 중인 세션) | 메서드 + 본문 + 응답 + 동적 URL + WS/SSE, 여러 표본의 diff로 매개변수 보완 | 화면이 실제로 렌더링되어야 요청 발생. 표본 하나로 필수/선택 여부 확정 불가 |

| 실행 모드 | 엔진 | 용도 |
|---|---|---|
| **depth** | `runtime_harvest.js`(Puppeteer) | API 목록, METHOD/매개변수/응답 본문, WS/SSE, 재현 가능한 일괄 실행 |
| **coverage** | browser + `preload.js` | 탭/대화상자/테이블을 클릭해 기능 실행 지점을 더 깊이 조사 |
| **both** | depth 이후 coverage | 가장 완전하지만 가장 오래 걸림 |

**매개변수 조사 방법**(범용 스크립트 없음): 경로는 harvest/정규식으로, 매개변수는 **기준점 주변 범위 확장 + UI 바인딩 추적 + 여러 표본의 diff + 오류 역추론**으로 조사합니다(grep 예시는 [reference.md](reference.md)의 J절 참조).

---

## 완료 정의

다음을 모두 충족해야 정찰 완료를 선언할 수 있습니다.

- [ ] **정적**: Phase 1 harvest에서 `api_static.txt`, `routes.txt`, `js/` 생성
- [ ] **런타임**: depth 또는 coverage 중 하나 이상 수행. coverage/both는 **Hook 적용 확인 + 동적 열거 반복** 필요
- [ ] **화면 진입**: 업무 경로 방문 시 `/login`이 아님(hash 라우트 주의)
- [ ] **매개변수**: coverage/both에서 실행 매트릭스 + `param_samples.json` 완성. Phase 5에서 `params_merged.json` 병합
- [ ] **깊이**(모듈 화면이 비어 있는 경우): Phase 4 권한 트리 복원 후 **모듈 수준 API**가 나올 때까지 재실행(locale/bootstrap만으로는 부족)
- [ ] **전달**: Phase 5 산출물 전체 생성(산출물 표 참조). `insert_assets`로 서비스와 엔드포인트 자산 저장

---

## 스크립트와 게이트

`scripts/`는 참고 템플릿입니다. **원본을 바로 실행하고 최종 결과로 삼으면 안 됩니다.**

**규칙**: 먼저 읽기 → 대상에 맞게 수정 → `OUTDIR`(예: `recon/`)에 저장 → `CHANGES.md` 기록. 맞지 않으면 구조만 참고하고 조사 방법에 따라 다시 작성합니다.

| 게이트 | 시점 | 참고 스크립트 → OUTDIR 사본 | 주로 수정할 항목 |
|---|---|---|---|
| **A(정적)** | Phase 0 이후, harvest/spider **최초** 실행 전 | `harvest_static.py` / `spider_mpa.py` | **대부분 기본 regex로 실행 가능**. manifest/구문이 맞지 않을 때만 endpoint 정규식, webpack/Vite `publicPath`, MPA exclude/cookie 수정 |
| **B(런타임)** | Phase 2 이후, depth/coverage 실행 전 | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage 키, neutralize 성공 값, stubs, login 정규식, api 접두사, hash/history |

**SPA 필수 순서**(순서 변경 불가. ‘먼저 탐색하고 스크립트 실행’보다 Phase 번호 우선):

| 단계 | 필수 | 금지 |
|---|---|---|
| Phase 0 완료 후 | 다음 Bash 명령 = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | 주 진입점 `index-*.js`(보통 500KB 초과)를 curl/grep/Read로 읽기 |
| 게이트 A | 스크립트 복사 → 필요한 부분만 수정 → **즉시 실행** | API를 수동 추출한 뒤 harvest 실행 여부 결정 |
| Phase 1 완료 전 | `wc -l`로 산출물 확인. 404이면 harvest 수정 후 재시도 | 추출 스크립트 직접 작성, 미다운로드 URL을 반복 grep |
| Phase 1b 이후 | grep은 `OUTDIR/js/*.js`에만 수행 | 주 번들로 harvest 대체 |

- ✅ `harvest_static.py` 복사 → 필요시 regex 수정 → **즉시 실행**
- ❌ 주 번들 curl → 여러 번 grep → 임시 추출 스크립트 작성 → 마지막에 harvest
- **MPA**: Phase 0 다음 Bash 명령 = `python3 OUTDIR/spider_mpa.py ...`

---

## 도구와 출력 제약

| 제약 | 설명 |
|---|---|
| 대용량 파일 | 100KB 초과 `index-*.js`를 Read/grep으로 컨텍스트에 넣지 않음. OUTDIR 스크립트로 일괄 처리 |
| grep 출력 | 반드시 `\| head -20` 또는 `-m 5` 사용. 대화에는 경로 요약만 남기고 번들 조각을 붙이지 않음 |
| 검증 | `wc -l`, `ls \| wc -l` 사용. 디렉터리 전체를 Read하지 않음 |
| regex 사전 탐색 | 선택 사항, 최대 1회, 50KB 이하 작은 chunk 또는 HTML만 허용. 정적 분석의 기준은 harvest |
| reference | 예시/템플릿/문제 해결은 [reference.md](reference.md) 참조. 전문을 인라인으로 반복하지 않음 |

---

## 실행 로드맵

```
Phase 0 분류 + OUTDIR
  → 게이트 A → Phase 1 harvest(★ 즉시 실행 ★)
  → Phase 1b 매개변수 역분석
  → Phase 2 인증 게이트 3개 → config.json
  → 게이트 B → Phase 3 런타임 + 매개변수 매트릭스
  → Phase 4 권한 트리(필요시) → Phase 3 재실행
  → Phase 5 보고서 병합 + insert_assets로 발견한 모든 서비스·엔드포인트 API 자산 일괄 저장. 발견한 자산을 빠뜨리지 않습니다.
```

순서대로 확인합니다. **앞 항목을 완료하기 전에는 다음 Phase로 넘어가지 않습니다.**

1. [ ] **Phase 0**: SPA/MPA 예비 조사, `OUTDIR` 생성 → [Phase 0](#phase-0--분류)
2. [ ] **게이트 A + Phase 1**: 스크립트 복사 → **즉시** harvest → `wc -l` 검증 → [Phase 1](#phase-1--정적-분석)
3. [ ] **Phase 1b**: 기준점 범위 확장 + 바인딩 계층 → `param_candidates.json` → [Phase 1b](#phase-1b--매개변수-역분석)
4. [ ] **Phase 2**: 인증 게이트 3개 → `config.json` → [Phase 2](#phase-2--인증-게이트-3개)
5. [ ] **게이트 B**: 런타임 스크립트 조정 → [Phase 3](#phase-3--런타임)
6. [ ] **Phase 3**: depth / coverage / both, 화면 진입 확인, 매개변수 실행 매트릭스 → `param_samples.json`
7. [ ] **Phase 4**(필요시): 권한 트리 → stub 패치 → Phase 3 재실행 → [Phase 4](#phase-4--권한-트리-복원)
8. [ ] **Phase 5**: 산출물 병합 + 보고서 + `insert_assets` → [Phase 5](#phase-5--병합과-보고)

---

## Phase 0 — 분류

진입 HTML을 가져오고 **`OUTDIR`를 생성**합니다(스킬 내부 `scripts/`를 수정하지 않음).

- **SPA**: 빈 화면 틀 + `<div id=app>` + chunk → Phase 1–5
- **MPA**: SSR + `<form>`, endpoint 번들 없음 → 게이트 A 이후:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

`forms.txt`, `links.txt`, `api_inline.txt`를 생성합니다. SPA에서 forms ≈ 0이면 Phase 1로 전환합니다.

---

## Phase 1 — 정적 분석

[스크립트와 게이트](#스크립트와-게이트), [도구와 출력 제약](#도구와-출력-제약)을 따릅니다.

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: HTML script 분석 → webpack/Vite manifest → 모든 lazy chunk 다운로드 → `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` 생성.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk 수와 manifest 비교: 404이면 harvest를 수정해 재시도하며 chunk마다 수동 curl하지 않습니다.
- `api_static.txt`가 너무 적으면 OUTDIR의 endpoint 정규식을 넓혀 재실행합니다(reference 참조).

### Phase 1b — 매개변수 역분석

경로는 Phase 1에서 얻지만 매개변수 필드는 별도 조사해야 합니다. grep 규칙은 [도구와 출력 제약](#도구와-출력-제약)을 참조하세요.

**완료 기준**: 주요 인터페이스의 필드 이름, 전송 위치, 추정 자료형, 필수 여부, 표본 값, 신뢰도를 설명할 수 있어야 합니다.

#### 1b.0 — 전송 형태

| 형태 | 매개변수 위치 | 정적 분석에서 우선 확인 |
|---|---|---|
| REST JSON | body + query | 경로 기준점 옆 `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql 템플릿, `$page: Int` |
| 일반 form | urlencoded | `<form>`, `FormData` |
| 파일 업로드 | multipart | `FormData.append` |
| 경로 매개변수 | `/user/:id` | 라우트 테이블 + `useParams` / `$route.params` |
| 암호화/서명 | `sign`/`data` 내부 | 암호화 함수 인자 후킹(reference D절) |

산출물: 인터페이스마다 `transport: query|json|form|graphql|encrypted` 표시.

#### 1b.1 — 기준점 주변 범위 확장

알려진 경로를 기준점으로 삼고 주변 범위를 넓혀 요청 구성 객체를 찾습니다.

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| 래퍼 계층 | 매개변수 단서 |
|---|---|
| axios 인스턴스 | `data` / `params` |
| 공통 request | 인터셉터가 주입하는 전역 필드 |
| OpenAPI 클라이언트 | 생성된 method 시그니처 |
| React Query / SWR | hook의 두 번째 인자 |
| Vue composable | composable 인자 |

남아 있는 자료형 단서: `yup`/`zod`/rules, `Form.Item name=`, 내장 Swagger.

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — 바인딩 계층

```
Form field → onFinish/handleSubmit → transform → API payload
```

| 바인딩 소스 | 방법 |
|---|---|
| 폼 submit | submit → transform → API 추적 |
| 테이블 검색 | `getFieldsValue()` → `params` |
| 라우트 | `:id` / `?tab=` |
| 인터셉터 | 전역 `tenantId`, 페이지 구분, sign |
| 열거형 select | `options` → API 열거 값 |

DevTools 호출 스택에서 `fetch`/`XHR.send`를 시작점으로 요청 구성 함수를 역추적합니다.

#### 1b.3 — 요청 구성의 세 가지 질문(Phase 2 인증 게이트와 다름)

| 질문 | 확인할 내용 |
|---|---|
| **구성** | payload 생성 위치와 transform 흔적 |
| **검증** | required, pattern, enum |
| **전송** | path / query / body / multipart / 헤더 |

인터셉터 게이트(Phase 2)에서 전역 주입 필드(Authorization, `X-Tenant-Id`, sign)도 확인합니다.

#### 1b.4 — Phase 3 연결

후보 필드는 정적/바인딩 계층에서 얻습니다. **필수/선택/조건부 의존성**은 Phase 3 매개변수 매트릭스 + diff + Phase 5 오류 역추론으로 확인해야 합니다.

---

## Phase 2 — 인증 게이트 3개

`OUTDIR/js/`에서 `head`를 붙인 grep으로 검색하고 `config.json`에 기록합니다(예시는 reference 참조).

| 게이트 | 질문 | 키워드 |
|---|---|---|
| **렌더링 게이트** | 로그인 상태를 어떻게 판단하는가? | `isLogin`, `getToken`, Cookie/localStorage |
| **인터셉터 게이트** | 무엇이 `/login` 이동을 유발하는가? | `response_code`, `errno`, axios interceptor |
| **콘텐츠 게이트** | 메뉴/권한은 어디서 오는가? | `menu`, `permission`, `role`, `acl`, `routes` |

localStorage 키 이름을 자격 증명으로 간주하지 마세요. chunk/요청 흐름으로 확인해야 합니다.

**종료 조건 = 게이트 B**: 결론을 `config.json`에 반영하고 `OUTDIR/runtime_harvest.js` / `preload.js`를 수정합니다.

### Phase 2b — API 관찰(선택 사항)

OUTDIR의 `preload.js`로 세션 키 이름, Authorization, 중첩된 API URL을 확인합니다.

| 설정 | 산출물 |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | 헤더 관찰 |
| `extractUrlsFromResponse: true` | 응답 내부의 하위 API |
| `observe.storageReads/cookieReads: true` | config에 반영 |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage의 각 반복에서 `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`를 내보냅니다.

---

## Phase 3 — 런타임

게이트 B를 통과해야 합니다. [범위와 금지 사항](#범위와-금지-사항에이전트-필독-위반-시-범위-이탈)과 자격 증명 없는 mock 전략을 따릅니다.

`config.json`에 `"runtimeMode": "depth" | "coverage" | "both"`를 설정합니다(템플릿은 reference 참조).

### Hook과 stub(depth + coverage 공통)

| 계층 | 범위 | 목적 |
|---|---|---|
| L1 정확 일치 | auth/권한/bootstrap stub | 첫 화면 인증 통과 |
| L2 실패 응답 보정 | 모든 JSON 응답 | 미로그인 코드 → 성공 |
| L3 기본 처리 | L1에 일치하지 않는 `/api` 등 | 빈 성공 본문으로 UI 렌더링 |

- **depth**: fake auth + `forward`로 업무 코드 수정 + `stubs`. `routes` 순회(hash/history), `runtime_api.json` 생성
- **coverage**: **document-start**에 `preload.js` 주입(CDP `addScriptToEvaluateOnNewDocument` 또는 Userscript)

검증: `window.__API_RECON_PRELOAD__`가 존재하며 업무 경로에서 `/login`으로 돌아가지 않아야 합니다.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage 동적 열거(필수)

1. 주 탐색/사이드바: 각 항목을 클릭하고 네트워크를 1–3초 기다립니다.
2. 탭: `role=tab`, `.ant-tabs-tab`
3. 테이블: 첫 행의 보기/편집/상세
4. 도구 모음: 내보내기, 필터, 새로 만들기(**되돌릴 수 없는 삭제 금지**)
5. 모듈에 진입할 때마다 API/라우트 병합
6. SPA: `routes.txt`의 미조사 경로에 통제된 `pushState` 사용(MPA는 금지)

**매개변수 실행 매트릭스**(필수): 모듈별로 작업 유형마다 기록하고 **여러 표본의 diff를 비교**합니다.

| 작업 | 보통 추가되는 매개변수 |
|---|---|
| 목록 첫 화면 | 페이지 구분 + 기본 필터 |
| 검색 클릭 | keyword, filter |
| 고급 필터 | 더 많은 선택 매개변수 |
| 생성/편집 | 전체 entity |
| 일괄 처리/내보내기/정렬 | `ids[]`, `exportType`, `sortField` |

**stub 사용 시에도 발신 body/headers는 실제 프런트엔드 값**이므로 요청을 기준으로 합니다. 기록 → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + 사이드바 클릭 + `pushState`
- **both**: 3a depth 후 3b coverage

---

## Phase 4 — 권한 트리 복원

**실행 조건**: 모듈 화면이 비어 있거나 모든 라우트에서 bootstrap(예: locale)만 호출되면 콘텐츠 게이트를 통과하지 못한 것입니다.

| 현상 | 의미 |
|---|---|
| 화면 틀 진입 성공 | 렌더링 + 인터셉터 게이트 통과 |
| 사이드바 항목 누락/클릭 시 빈 화면 | stub 구조 또는 권한 코드 불완전 |
| 라우트별 API가 같고 극히 적음 | `v-if permission` 미충족 |
| `routes.txt`가 번들보다 훨씬 적음 | auth 모듈에서 보완 필요 |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

일반적인 흐름: `role_permissions`(평면 코드) + `permissions/all`(트리) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

중간 산출물: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

stub 확인: 바깥 `response_code`는 인터셉터 게이트와 일치하고 평면 코드와 트리가 대응해야 하며, `routes`는 `route_map`의 모든 링크를 포함해야 합니다.

`config.json` 수정 후 **Phase 3를 다시 실행**합니다. 대형 SPA는 `waitUntil`, `routeTimeout`, `perRouteMs`를 조정할 수 있습니다(reference A3/I절 참조).

---

## Phase 5 — 병합과 보고

### 산출물 표

| 파일 | 단계 | 내용 |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | 정적 번들과 경로 |
| `param_candidates.json` | 1b | 정적 매개변수 필드 후보 |
| `config.json` | 2 | 게이트 3개 + 런타임 설정 |
| `runtime_api.json` | 3a | depth 상세 기록(WS/SSE 포함) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | 여러 표본, 클릭 로그, 상세 정보 |
| `route_map.json` 등 | 4 | 권한 트리 중간 파일(실행한 경우) |
| `params_merged.json` | 5 | 병합된 매개변수 필드 + 신뢰도 |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | 라우트, API, 매개변수, 기능 지점, 한계 |
| **insert_assets** | 5 | 모든 서비스와 엔드포인트 자산을 자산 저장소에 기록 |

### 5b — 매개변수 병합

`param_samples.json`의 diff를 비교합니다. **범용 병합 스크립트는 없습니다.** 신뢰도 규칙은 reference J7(높음/중간/낮음/실행 대기)을 참조하세요.

### 5c — 오류 역추론

승인된 범위에서 불완전한 요청을 보내 400 오류를 읽을 수 있습니다(**매개변수 조사이며 취약점 테스트가 아님**). 예: `field 'x' is required`, 열거형 오류. `data` 래퍼, `variables`, 암호화 전 `bizData`에 주의하세요.

보고서에 runtimeMode, 정적/런타임 API 수, 매개변수 신뢰도, 미조사 모듈, 참고 스크립트 대비 `CHANGES.md` 요약을 명시합니다.

권장 `site_map.json` 구조:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

추가 필드와 grep 예시는 [reference.md](reference.md)를 참조하세요.

---

## 공통 설명

- **프레임워크 독립적**: webpack/Vite/Angular의 지연 로딩 조사 방법은 동일합니다.
- **전송**: REST/JSON, GraphQL, WebSocket, SSE 지원. gRPC-web은 범위 밖입니다.
- **SSR**: 클라이언트 fetch는 기록할 수 있지만 RSC/Server Actions는 완전히 열거할 수 없습니다.
- **사각지대**: JSVMP, WASM, 강한 HMAC/mTLS 검증 → 정적 분석과 한계 명시
- **매개변수 사각지대**: 조건 연동, hidden params, WASM 요청 구성 → ‘실행 대기’/‘도달 불가’ 표시
- **정적 분석은 안전망**: 런타임 분석이 막혀도 정적으로 endpoint를 열거할 수 있습니다.

---

## 추가 자료

- grep 예시, `config.json` 템플릿, 문제 해결, Hook, 매개변수 역분석 J절, site_map 템플릿: **[reference.md](reference.md)**
- 참고 스크립트 경로는 [스크립트와 게이트](#스크립트와-게이트) 표를 참조하세요.
