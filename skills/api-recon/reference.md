# api-recon — 참고 설명서

grep 예시, `config.json` 템플릿, 문제 해결을 다룹니다. 모든 grep은 `js/` 디렉터리에서 실행합니다. 번들이 한 줄이면 먼저 `js-beautify` 또는 `sed 's/}/}\n/g'`를 사용할 수 있지만, 보통 주변 문맥을 포함한 원본 grep으로 충분합니다.

## 스크립트 설명

`scripts/`의 모든 파일은 **참고 템플릿**이며 실행 전에 대상 사이트에 맞게 조정해야 합니다. 일반적인 수정 항목은 다음과 같습니다.

| 스크립트 | 주로 조정할 항목 |
|---|---|
| `harvest_static.py` | endpoint 정규식, webpack/Vite manifest 분석, 마이크로 프런트엔드 publicPath, 재시도/동시 실행 |
| `runtime_harvest.js` | neutralize 필드 이름과 성공 값, stub 일치 규칙과 body 구조, routes 소스, WS 기록, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, L3 활성화 여부, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | 파괴적 링크 `--exclude`, cookie, depth/max, 동일 도메인 필터 |
| `extract_route_map.py` | `routeMap` / `routeLink` 정규식, KEY 명명 패턴 |
| `build_perm_tree.py` | `userRouteAuth` 분석, `ROOTS`/`PREFIX_PARENT` 계층 추정, stub 바깥 필드 이름 |
| `config.json` | 위의 모든 사이트 전용 매개변수를 모으는 공통 진입점 |

수정한 파일은 작업 디렉터리(예: `recon/`)에 두고, 보고서에 참고 스크립트와의 구체적인 차이를 기록하는 것이 좋습니다.

---

## A. 인증 게이트 3개 역분석

### A1. 렌더링 게이트 — ‘로그인 상태를 어떻게 판단하는가?’

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

`isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` 흐름을 찾아 **저장 키**, **저장소**(Cookie/localStorage), **인코딩**을 확인합니다.

| 인코딩 | config 모의 값 구성 방법 |
|---|---|
| 평문 문자열 / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | 서명 없는/`alg:none` JWT 또는 번들 내부 키로 서명 |
| 암호화(SM2/AES/RSA) | 하드코딩된 키 확인. 렌더링 게이트가 디코딩 가능한 blob만 요구하면 모의 값 구성, 아니면 정적 분석 사용 |

→ `cookies` / `localStorage`에 기록합니다.

### A2. 인터셉터 게이트 — ‘무엇이 /login 이동을 유발하는가?’

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

**필드 이름**, **성공 값**(보통 `0` 또는 `200`), **이동을 유발하는 실패 값**을 확인합니다. 임의 세션으로 검증합니다.

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ `neutralize.fields` + `neutralize.success`에 기록합니다.

### A3. 콘텐츠 게이트 — ‘메뉴/권한은 어디서 오는가?’

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**두 계층의 데이터**(일반적인 기업 관리 화면):

| API | 일반적인 payload | 사용 지점 |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | 라우트 가드, 버튼 수준 ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | 사이드바 메뉴 렌더링 |
| 번들 내부 `userRouteAuth` | `{ CODE: { url, name? } }` | code → 프런트엔드 경로 |
| 번들 내부 `routeMap` | `{ KEY: { name, link } }` | 별칭 해석(webpack `o.DASHBOARD`) |

사용 지점 코드를 읽어 `getResultTree(tree, permissions)`의 필터 방식과 `v-if` / `hasAuth(code)`가 확인하는 필드를 파악합니다.

**수동 모의 값 구성**(소규모 사이트): 허용적인 payload 생성 → `stubs`.

**전체 권한 트리 복원**(대규모 사이트, 사이드바/하위 모듈이 여전히 빈 경우): **I절** 참조.

---

## B. config.json 템플릿

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

필드 설명:
- `runtimeMode`: `depth`(Puppeteer), `coverage`(browser MCP), `both`
- `cookies[].value` 접두사: `b64json:` → base64(JSON), `json:` → 원본 JSON, 접두사 없음 → 리터럴
- `forward: true`는 실제 요청을 전달하고 코드 필드를 수정합니다. `false`는 완전한 오프라인 stub입니다.
- `mockTier`: coverage 모드 preload에서 활성화할 계층. 예: `L1+L2`, `L1+L2+L3`
- `routes`는 `routes.txt`에서 가져옵니다. 메뉴 모의 값 구성 후 harness가 `<a href>`를 자동 추가합니다.
- `captureResponses` / `recordWs`는 depth 모드에서만 적용됩니다.
- `waitUntil`: 대형 SPA는 `networkidle2` 대기를 피하도록 `domcontentloaded`를 사용합니다.
- `routeTimeout`: 라우트별 `page.goto` 제한 시간(밀리초)
- `proxy`: Puppeteer `--proxy-server`. `HTTP_PROXY` / `HTTPS_PROXY`도 설정할 수 있습니다.

### B1. 이중 stub 템플릿(role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

바깥 필드 이름(`response_code` / `code` / `data`)은 A2 인터셉터 게이트와 일치해야 하며, `permissions`는 트리의 모든 말단 코드를 포함해야 합니다.

---

## C. coverage 모드: preload 설정

`scripts/preload.js` 상단의 `CONFIG` 객체를 수정하거나 CDP로 주입하기 전에 교체합니다.

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* 同 config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

검증: `window.__API_RECON_PRELOAD__ === true`이고 pathname이 안정적으로 유지되어야 합니다.

기록 결과 내보내기:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook 기능

preload(coverage)와 runtime_harvest(depth)에 내장된 브라우저 Hook 기능과 지원 범위:

| Hook 기능 | API 발견에 주는 가치 | 지원 |
|---|---|---|
| fetch / XHR.open 후킹 | 요청 URL/메서드 기록 | ✅ `recordDetail` + `__API_RECON_LOG__` |
| XHR.setRequestHeader 후킹 | Authorization 등의 헤더 발견 | ✅ `observe.xhrHeaders` |
| localStorage/cookie 읽기 후킹 | 세션 키 이름 확인 | ⚠️ 선택 사항 `observe.storageReads/cookieReads` |
| Vue 라우트 수집 | frontendRoutes 보완 | ✅ `__API_RECON_ROUTES__`(로드된 라우트) |
| Vue 라우트 가드 무력화 / 로그인 이동 차단 | 모듈을 렌더링해 API 실행 | ✅ `neutralizeVueRouter` + 기본 이동 무력화 |
| React 라우트 수집 | 라우트 보완 | ⚠️ 정적 분석 + 클릭. 전용 Hook 없음 |
| 페이지 이동 차단(로그인 경로) | 현재 페이지에서 분석 유지 | ⚠️ 업무 탐색을 막지 않도록 로그인 경로만 차단 |
| 암호화 라이브러리 후킹(CryptoJS/SM 등) | 암호화된 매개변수 → 평문 API body | ❌ 암호화 함수 인자를 수동 후킹해야 함. 결과는 config에 기록 |
| 안티디버깅 우회 | 런타임 API 기록 차단 해소 | ❌ 수동 처리 필요. 정적 분석은 계속 사용 가능 |

---

## E. Endpoint 추출 정규식(정적 결과가 적을 때)

`harvest_static.py`의 `extract_endpoints`를 넓히거나 수동으로 처리합니다.

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. 문제 해결

| 현상 | 원인 → 조치 |
|---|---|
| 정적 API가 매우 적음 | endpoint 구문 불일치 → 정규식 확대(E절) |
| chunk 수 ≪ manifest | CSS 전용이거나 배포되지 않은 chunk, 404 재시도 여부 확인 |
| 런타임에서도 로그인 화면 표시 | 렌더링 게이트 오류 → A1의 키 이름, 저장소, 인코딩, domain 재확인 |
| 화면 틀에 진입했지만 모듈이 비어 있음 | 콘텐츠 게이트 → 메뉴 모의 값 구성(A3). `routes` 경로도 확인 |
| 라우트마다 bootstrap/locale만 있음 | 권한 코드 불완전 → I절 권한 트리 복원. `role_permissions` + `permissions/all` 이중 stub 확인 |
| 사이드바는 있지만 하위 화면이 비어 있음 | 트리 중간 노드 누락 또는 code와 `userRouteAuth` 불일치 |
| 모든 API에서 로그인으로 이동 | 인터셉터 게이트 → `neutralize` 확인. 중첩 필드는 순회 로직 확장 필요 |
| WS 프레임이 0개 | 사용자 상호작용 후에만 subscribe할 수 있음. `perRouteMs` 증가 |
| 응답 본문이 비어 있음 | 실제 응답은 `forward: true`에서만 제공 |
| Chromium 없음 | chromium 설치 또는 `config.chromium` / `CHROMIUM` 설정 |
| mock이 많아도 로그인으로 돌아감 | Hook이 늦거나 `location.href` setter 누락 → document-start + preload |
| 목록이 모두 비어 있음 | L3의 빈 배열은 정상. 탭/설정/상세 클릭 계속 |
| Redux action을 라우트로 오인 | get/set/change/clear/toggle/upload가 포함된 내부 경로 필터링 |
| Vue에서 계속 로그인으로 이동 | preload가 document-start가 아님 → 주입 시점 변경. `neutralizeVueRouter: false`이면 가드 수동 제거 |
| 응답에 URL이 있으나 log에 없음 | `extractUrlsFromResponse` 활성화 또는 `__API_RECON_DETAIL__`에서 수동 추출 |
| Authorization 헤더 이름을 모름 | `observe.xhrHeaders` 활성화 또는 DevTools 요청 헤더 확인 |
| 런타임이 매우 느림/시간 초과 | `waitUntil: domcontentloaded`로 변경, `routeTimeout` 감소, `networkidle2` 사용 금지 |
| 프록시 연결 실패 | `proxy` / 환경변수 확인. Puppeteer와 curl의 프록시 포트 일치 확인 |

---

## G. 보안이 강화된 대상

서버가 세션을 단계별로 검증하면(모의 값을 만들 수 없는 서명 cookie, 서버 렌더링되며 stub 불가능한 메뉴) 런타임이 화면 틀에서 멈춥니다. 예상 동작:

- **정적 분석으로 endpoint 열거 가능**: 모듈 경로는 코드 안에 있습니다.
- 승인 범위가 허용하면 **실제 세션**으로 같은 harness를 실행할 수 있습니다. `forward: true`, neutralize 없이 실제 methods/params/responses를 수집합니다.

---

## H. 단일 작업 체크리스트

1. 승인 범위 확인
2. `scripts/harvest_static.py` **읽기** → 대상에 맞게 조정 → 실행 → `api_static.txt`, `routes.txt` 검토
3. **Phase 1b**: 경로 기준점 주변 범위 확장 + 바인딩 계층 → `param_candidates.json`(J절)
4. A1/A2/A3 역분석 → 사이트 전용 `config.json` 작성
5. `runtime_harvest.js` / `preload.js`를 **읽고 조정한 뒤** 실행
6. `runtimeMode=depth`: `npm install` → 조정한 harvest 스크립트 실행
7. `runtimeMode=coverage/both`: 조정한 preload를 document-start에 주입 → browser MCP 동적 열거 + **매개변수 실행 매트릭스**
8. 모듈이 렌더링되지 않음 → **I절 권한 트리 복원** → stub 패치 → 재실행
9. 여러 매개변수 표본의 diff + 오류 역추론 → `params_merged.json`
10. 병합 → `site_map.json` + `api_merged.txt`. 조사 범위, 누락, 스크립트 변경점을 정확히 명시

---

## I. 권한 트리 복원(Phase 4 심화)

단순한 `menus: [{ path, show: true }]` 모의 값이 효과가 없고 하위 모듈이 마운트되지 않을 때 사용합니다.

### I1. auth 모듈 찾기

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

**권한 API 경로**, **응답 필드 이름**, **사용하는 chunk 파일 이름**을 기록합니다.

### I2. routeMap 추출

```bash
python3 scripts/extract_route_map.py recon/js recon/
# 产出 recon/route_map.json
```

`[!] no routeMap pattern found`이면 `extract_route_map.py`의 정규식을 넓히거나 수동 grep합니다.

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. 권한 트리와 stub 생성

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

스크립트 로직:
1. `userRouteAuth={MONITOR:{url:...},...}` 분석(webpack 별칭 `He=o.DASHBOARD` 포함)
2. `route_map.json`으로 별칭 → 실제 경로 해석
3. 코드 접두사로 상위 노드 추정(`MONITOR_ALERT` → `MONITOR`)
4. `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json` 출력
5. `--config` 사용 시 `config.json`의 `stubs`에 자동 기록하고 `routes` 확장

**대상에 맞게 조정**(스크립트 상단):
- `DEFAULT_ROOTS`: 최상위 모듈 코드 목록
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → 상위 노드 매핑
- `DEFAULT_EXTRA_PARENT`: 접두사 관계가 없는 고립 노드

### I4. stub 일관성 검증

```bash
# permissions 数量应 ≈ userRouteAuth 条目数
wc -l recon/perm_codes_all.txt
# routes 应覆盖 route_map 全部 link
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. 런타임 재실행 및 비교

```bash
node recon/runtime_harvest.js recon/config.json
# 对比 forge 前后 runtime_api.json 条数；检查 /attack、/asset 等是否出现模块 API
```

| 모의 값 구성 전 | 구성 후(성공) |
|---|---|
| 모든 라우트에서 같은 bootstrap 3–5개 | 라우트별로 서로 다른 모듈 API 실행 |
| `/api/locale/language`만 있음 | `/api/web/...` 모듈 endpoint 등장 |
| `routes.txt`에 한 자릿수 라우트 | route_map에서 `routes` 80–110개 이상 확보 |

### I6. 계속 실패하는 경우

- **coverage 모드**: 사이드바와 탭 클릭. 권한 게이트는 상호작용 후에야 요청할 수 있습니다.
- **stub 필드**: 실제 API(curl + 실제 session)와 stub의 중첩 구조를 비교합니다.
- **추가 가드**: `hasPermission|checkRole|func.` 등 버튼 수준 검사를 grep하고 `role_permissions.permissions`를 확장합니다.
- **정적 분석 대안**: 모듈 API 경로는 `api_static.txt`에 남습니다. 런타임은 METHOD/body를 보완하며, 매개변수는 `param_candidates.json`과 기록한 표본을 유지합니다.

---

## J. 매개변수 역분석(Phase 1b / 5b / 5c)

**조사 방법이며 범용 스크립트가 아닙니다.** 경로는 정규식으로 찾고, 매개변수는 기준점 주변 범위 확장 + UI 바인딩 추적 + 여러 표본의 diff + 오류 역추론으로 찾습니다.

### J1. 기준점 주변 범위 확장 — 경로에서 요청 구성 객체 찾기

```bash
# 以 Phase 1 已知 path 为锚
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. 래퍼 계층과 전송 형태

```bash
# axios / 统一 request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# 路径参数
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. 검증 게이트 — 필수 / 형식 / 열거형

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. 바인딩 계층 — 폼 → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

런타임 보완: DevTools → Network → 요청 → **Initiator**(호출 스택)에서 `fetch`/`send`를 시작점으로 요청 구성 함수를 역추적합니다.

### J5. 암호화된 매개변수

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**암호문으로 필드를 추측하지 마세요.** 암호화 함수의 **인자**를 후킹하여 암호화 전 평문 payload를 기록하고 결과를 `config.json` / `param_candidates.json`에 저장합니다.

### J6. 매개변수 실행 매트릭스(Phase 3 필수)

모듈별 작업을 각각 기록하고 요청 body/query의 diff를 비교합니다.

| 작업 | 확인 항목 |
|---|---|
| 목록 첫 화면 | 페이지 구분 기본값 |
| 검색 | keyword, filters |
| 고급 필터 | 선택 필드 |
| 생성/편집 | 전체 entity |
| 일괄 처리/내보내기 | `ids[]`, `exportType` |
| 정렬/페이지 이동 | `sortField`, `order` |

`param_samples.json` 생성: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. 신뢰도 규칙

| 신뢰도 | 조건 |
|---|---|
| **높음** | 정적 호출 지점과 런타임 표본 2개 이상이 일치 |
| **중간** | 정적 결과만 있거나 런타임 표본 1개뿐 |
| **낮음** | 응답/오류에서 역추론했고 추가 검증 없음 |
| **실행 대기** | 정적으로 확인한 필드이지만 UI/권한 경로에 도달하지 못함 |

### J8. 상황별 빠른 설정

| 상황 | 순서 |
|---|---|
| REST 목록 화면 | J1 요청 구성 객체 → J6 diff 4회 → J3 rules |
| 생성/편집 폼 | J3 Form name → J4 submit 흐름 → 런타임 제출 + 일부러 비워 400 확인 |
| GraphQL | J2 variables 선언 → 런타임에서 각 operation의 variables 기록 |
| 암호화 body | J5 인자 후킹 → 암호화 전 필드가 실제 params |

### J9. api-recon 단계 대응

| api-recon | 매개변수 조사 |
|---|---|
| Phase 1 정적 분석 | J1 기준점 주변 범위 확장 |
| Phase 2 A2 인터셉터 | 전역 주입 필드(tenantId, sign) |
| Phase 3 런타임 | J6 실행 매트릭스 + `param_samples.json` |
| Phase 4 권한 트리 | 모듈마다 폼이 다르므로 충분한 권한이 있어야 모든 필드 실행 |
| Phase 5 병합 | `params_merged.json` + 신뢰도. 표본 하나로 필수 여부 확정 금지 |

### J10. 문제 해결

| 현상 | 조치 |
|---|---|
| 정적 필드 이름이 런타임에 전혀 나타나지 않음 | ‘실행 대기’ 표시. 권한 트리 보완 / 고급 필터 클릭 / 연동 select의 각 option 실행 |
| 같은 경로에 서로 다른 body 구조 | 정상. `action`별로 기록하고 schema를 억지로 병합하지 않음 |
| stub 응답은 가짜지만 params를 보고 싶음 | **발신 요청**의 body/headers 확인. stub 응답으로 역추론하지 않음 |
| 400에서 nested field 오류 | 바깥 래퍼 `data`/`bizData`/`variables` 확인 |
| GraphQL operation 이름만 보임 | `variables` JSON 펼치기. 정적으로 `$var: Type` 검색 |

---
