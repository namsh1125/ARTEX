# 저장소 관리

쿠키, localStorage, sessionStorage, 브라우저 저장소 상태를 관리합니다.

## 저장소 상태

쿠키와 저장소를 포함한 전체 브라우저 상태를 저장하고 복원합니다.

### 저장소 상태 저장

```bash
# Save to auto-generated filename (storage-state-{timestamp}.json)
playwright-cli state-save

# Save to specific filename
playwright-cli state-save my-auth-state.json
```

### 저장소 상태 복원

```bash
# Load storage state from file
playwright-cli state-load my-auth-state.json

# Reload page to apply cookies
playwright-cli open https://example.com
```

### 저장소 상태 파일 형식

저장된 파일에는 다음 내용이 포함됩니다.

```json
{
  "cookies": [
    {
      "name": "session_id",
      "value": "abc123",
      "domain": "example.com",
      "path": "/",
      "expires": 1893456000,
      "httpOnly": true,
      "secure": true,
      "sameSite": "Lax"
    }
  ],
  "origins": [
    {
      "origin": "https://example.com",
      "localStorage": [
        { "name": "theme", "value": "dark" },
        { "name": "user_id", "value": "12345" }
      ]
    }
  ]
}
```

## 쿠키

### 모든 쿠키 조회

```bash
playwright-cli cookie-list
```

### 도메인별 쿠키 필터링

```bash
playwright-cli cookie-list --domain=example.com
```

### 경로별 쿠키 필터링

```bash
playwright-cli cookie-list --path=/api
```

### 특정 쿠키 조회

```bash
playwright-cli cookie-get session_id
```

### 쿠키 설정

```bash
# Basic cookie
playwright-cli cookie-set session abc123

# Cookie with options
playwright-cli cookie-set session abc123 --domain=example.com --path=/ --httpOnly --secure --sameSite=Lax

# Cookie with expiration (Unix timestamp)
playwright-cli cookie-set remember_me token123 --expires=1893456000
```

### 쿠키 삭제

```bash
playwright-cli cookie-delete session_id
```

### 모든 쿠키 삭제

```bash
playwright-cli cookie-clear
```

### 고급: 여러 쿠키 또는 사용자 정의 옵션

여러 쿠키를 한꺼번에 추가하는 등 복잡한 작업에는 `run-code`를 사용합니다.

```bash
playwright-cli run-code "async page => {
  await page.context().addCookies([
    { name: 'session_id', value: 'sess_abc123', domain: 'example.com', path: '/', httpOnly: true },
    { name: 'preferences', value: JSON.stringify({ theme: 'dark' }), domain: 'example.com', path: '/' }
  ]);
}"
```

## 로컬 저장소

### 모든 localStorage 항목 조회

```bash
playwright-cli localstorage-list
```

### 단일 값 조회

```bash
playwright-cli localstorage-get token
```

### 값 설정

```bash
playwright-cli localstorage-set theme dark
```

### JSON 값 설정

```bash
playwright-cli localstorage-set user_settings '{"theme":"dark","language":"en"}'
```

### 단일 항목 삭제

```bash
playwright-cli localstorage-delete token
```

### localStorage 전체 삭제

```bash
playwright-cli localstorage-clear
```

### 고급: 여러 작업

여러 값을 한꺼번에 설정하는 등 복잡한 작업에는 `run-code`를 사용합니다.

```bash
playwright-cli run-code "async page => {
  await page.evaluate(() => {
    localStorage.setItem('token', 'jwt_abc123');
    localStorage.setItem('user_id', '12345');
    localStorage.setItem('expires_at', Date.now() + 3600000);
  });
}"
```

## 세션 저장소

### 모든 sessionStorage 항목 조회

```bash
playwright-cli sessionstorage-list
```

### 단일 값 조회

```bash
playwright-cli sessionstorage-get form_data
```

### 값 설정

```bash
playwright-cli sessionstorage-set step 3
```

### 단일 항목 삭제

```bash
playwright-cli sessionstorage-delete step
```

### sessionStorage 삭제

```bash
playwright-cli sessionstorage-clear
```

## IndexedDB

### 데이터베이스 목록

```bash
playwright-cli run-code "async page => {
  return await page.evaluate(async () => {
    const databases = await indexedDB.databases();
    return databases;
  });
}"
```

### 데이터베이스 삭제

```bash
playwright-cli run-code "async page => {
  await page.evaluate(() => {
    indexedDB.deleteDatabase('myDatabase');
  });
}"
```

## 자주 사용하는 패턴

### 인증 상태 재사용

```bash
# Step 1: Login and save state
playwright-cli open https://app.example.com/login
playwright-cli snapshot
playwright-cli fill e1 "user@example.com"
playwright-cli fill e2 "password123"
playwright-cli click e3

# Save the authenticated state
playwright-cli state-save auth.json

# Step 2: Later, restore state and skip login
playwright-cli state-load auth.json
playwright-cli open https://app.example.com/dashboard
# Already logged in!
```

### 저장 후 복원

```bash
# Set up authentication state
playwright-cli open https://example.com
playwright-cli eval "() => { document.cookie = 'session=abc123'; localStorage.setItem('user', 'john'); }"

# Save state to file
playwright-cli state-save my-session.json

# ... later, in a new session ...

# Restore state
playwright-cli state-load my-session.json
playwright-cli open https://example.com
# Cookies and localStorage are restored!
```

## 보안 참고 사항

- 인증 토큰이 포함된 저장소 상태 파일은 절대 커밋하지 마세요.
- `.gitignore`에 `*.auth-state.json`을 추가하세요.
- 자동화가 끝나면 상태 파일을 삭제하세요.
- 민감 데이터에는 환경변수를 사용하세요.
- 기본적으로 세션은 메모리 모드로 실행되므로 민감한 작업에 더 안전합니다.
