# 테스트 생성(plan → generate → heal)

`playwright-cli`로 Playwright 테스트를 작성하고 유지하는 전체 작업 흐름입니다. 모든 `playwright-cli` 작업은 대응하는 Playwright TypeScript를 출력하며 이 코드가 테스트의 기초가 됩니다. 아래 절은 각각 독립적으로 사용할 수 있습니다.

- **생성 원리**: 작업을 TypeScript로 바꾸고 검증문을 추가하는 공통 원리
- **계획(Plan)**: 앱을 탐색하고 테스트할 내용을 명세 파일로 작성
- **생성(Generate)**: 명세를 Playwright 테스트 파일로 변환. 모호하거나 오래된 명세는 갱신
- **수정(Heal)**: 실패한 테스트를 진단하고 코드를 수정하며 명세를 실제 동작과 일치시킴

plan / generate / heal은 같은 방식을 사용합니다. `npx playwright test --debug=cli`를 백그라운드에서 실행한 다음 `playwright-cli attach tw-XXXX`로 일시 정지된 페이지를 조작합니다. 디버깅과 연결 방식은 [playwright-tests.md](playwright-tests.md)를 참조하세요.

---

## 0. 생성 원리

`playwright-cli`로 수행한 각 작업은 대응하는 Playwright TypeScript 코드를 생성합니다. 이 코드는 출력에 표시되며 테스트 파일에 바로 복사할 수 있습니다.

```bash
# Start a session
playwright-cli open https://example.com/login

# Take a snapshot to see elements
playwright-cli snapshot
# Output shows: e1 [textbox "Email"], e2 [textbox "Password"], e3 [button "Sign In"]

# Fill form fields - generates code automatically
playwright-cli fill e1 "user@example.com"
# Ran Playwright code:
# await page.getByRole('textbox', { name: 'Email' }).fill('user@example.com');

playwright-cli fill e2 "password123"
# Ran Playwright code:
# await page.getByRole('textbox', { name: 'Password' }).fill('password123');

playwright-cli click e3
# Ran Playwright code:
# await page.getByRole('button', { name: 'Sign In' }).click();
```

### 테스트 파일 구성

생성된 코드를 모아 Playwright 테스트를 만듭니다.

```typescript
import { test, expect } from '@playwright/test';

test('login flow', async ({ page }) => {
  // Generated code from playwright-cli session:
  await page.goto('https://example.com/login');
  await page.getByRole('textbox', { name: 'Email' }).fill('user@example.com');
  await page.getByRole('textbox', { name: 'Password' }).fill('password123');
  await page.getByRole('button', { name: 'Sign In' }).click();

  // Add assertions
  await expect(page).toHaveURL(/.*dashboard/);
});
```

### 의미 기반 로케이터 사용

생성 코드는 가능하면 변경에 더 강한 역할 기반 로케이터를 사용합니다.

```typescript
// Generated (good - semantic)
await page.getByRole('button', { name: 'Submit' }).click();

// Avoid (fragile - CSS selectors)
await page.locator('#submit-btn').click();
```

### 기록하기 전에 탐색

작업을 기록하기 전에 스냅샷으로 페이지 구조를 파악합니다.

```bash
playwright-cli open https://example.com
playwright-cli snapshot
# Review the element structure
playwright-cli click e5
```

### 검증문 직접 추가

생성된 코드는 작업만 기록하며 검증문은 포함하지 않습니다. 권장 매처를 사용하여 테스트에 기대 조건을 추가하세요.

- `toBeVisible()`: 요소가 렌더링되어 보이는지 확인
- `toHaveText(text)`: 요소 텍스트 일치 확인
- `toHaveValue(value) / toBeEmpty()`: input/select 값 확인
- `toBeChecked() / toBeUnchecked()`: 체크박스 상태 확인
- `toMatchAriaSnapshot(snapshot)`: 페이지 또는 로케이터가 부분 접근성 스냅샷과 일치하는지 확인

`playwright-cli generate-locator <target>`으로 검증에 사용할 로케이터 표현식을 생성하고 snapshot/eval 명령으로 기대 값을 확인합니다.

텍스트 내용을 검증할 때 생성된 로케이터에 요소 자체의 텍스트가 들어가지 않도록 하세요. `getByTestId()`나 `getByLabel()`이 보통 적합합니다. 텍스트 기반 로케이터라면 `toBeVisible()`을 권장합니다.

비교할 스냅샷은 모든 정보를 포함할 필요 없이 검증에 필요한 내용만 담으면 됩니다. 변하는 값에는 정규식을 사용할 수 있습니다.

```bash
# Get a stable locator for an element ref to use in the assertion
playwright-cli --raw generate-locator e5
# getByRole('button', { name: 'Submit' })

# Capture expected text content for toHaveText
playwright-cli --raw eval "el => el.textContent" e5

# Capture expected input value for toHaveValue/toBeEmpty
playwright-cli --raw eval "el => el.value" e5

# Capture expected aria snapshot for toMatchAriaSnapshot/toBeChecked
# (whole page, or use a ref to scope to a region)
playwright-cli --raw snapshot
playwright-cli --raw snapshot e5
```

```typescript
// Generated action
await page.getByRole('button', { name: 'Submit' }).click();

// Manual assertions using the outputs above:
await expect(page.getByRole('alert', { name: 'Success' })).toBeVisible();
await expect(page.getByTestId('main-header')).toHaveText('Welcome, user');
await expect(page.getByRole('textbox', { name: 'Email' })).toHaveValue('user@example.com');
await expect(page.getByRole('checkbox', { name: 'Enable notifications' })).toBeChecked();

// toMatchAriaSnapshot on the whole page, finds a matching region
await expect(page).toMatchAriaSnapshot(`
  - heading "Welcome, user"
  - link /\\d+ new messages?/
  - button "Sign out"
`);

// toMatchAriaSnapshot scoped to a region
await expect(page.getByRole('navigation')).toMatchAriaSnapshot(`
  - link "Home"
  - link /\\d+ new messages?/
  - link "Profile"
`);
```

---

## 1. 계획

목표: 테스트할 시나리오를 나열하는 명세 파일(예: `specs/<feature>.plan.md`)을 만듭니다. 명세는 **항상 파일로 저장**합니다.

### 1.1 사전 조건: 작업 공간

먼저 작업 공간에 Playwright가 설치되어 있는지 확인합니다.

```bash
# Either of these confirms a workspace:
test -f playwright.config.ts || test -f playwright.config.js
npx --no-install playwright --version
```

설치되어 있지 않으면 초기 설정을 진행하고 사용자가 기본값을 선택하게 합니다.

```bash
npm init playwright@latest
```

### 1.2 사전 조건: 초기 상태 테스트

**초기 상태 테스트(seed test)**는 모든 시나리오의 시작 상태를 만드는 최소 테스트입니다. 앱 탐색, 필요한 로그인, 기능 플래그 등을 설정합니다. 시나리오는 이 테스트가 끝난 직후의 새로운 상태에서 시작합니다. `--debug=cli`는 이 테스트 **안에서** 멈추므로 모든 계획·생성 세션의 출발점입니다.

최소 초기 상태 테스트:

```ts
// tests/seed.spec.ts
import { test } from '@playwright/test';

test('seed', async ({ page }) => {
  await page.goto('https://example.com/');
});
```

권장 방식: 탐색을 fixture로 옮겨 시나리오 테스트에서 재사용합니다.

```ts
// tests/fixtures.ts
import { test as baseTest } from '@playwright/test';
export { expect } from '@playwright/test';

export const test = baseTest.extend({
  page: async ({ page }, use) => {
    await page.goto('https://example.com/');
    await use(page);
  },
});
```

```ts
// tests/seed.spec.ts
import { test } from './fixtures';

test('seed', async ({ page }) => {
  // Fixture already navigates. This empty body tells agents where to start.
});
```

초기 상태 테스트가 없으면 최소한 앱으로 이동하는 테스트를 만듭니다.

### 1.3 앱 탐색

초기 상태 테스트로 앱을 백그라운드에서 실행한 뒤 연결합니다.

```bash
PLAYWRIGHT_HTML_OPEN=never npx playwright test tests/seed.spec.ts --debug=cli
# wait for "Debugging Instructions" and the session name tw-XXXX
playwright-cli attach tw-XXXX
```

실행을 재개하여 초기 설정을 마치고 앱을 조사합니다.

```bash
playwright-cli resume                   # resume so that seed test runs fully
playwright-cli snapshot                 # inventory of interactive elements
playwright-cli click e5                 # follow a flow
playwright-cli eval "location.href"     # read URL / state
playwright-cli show --annotate          # ask the user to point at something
```

다음을 파악합니다.

- 상호작용 요소: 폼, 버튼, 목록, 필터, 모달
- 주요 사용자 작업의 처음부터 끝까지의 흐름
- 경계 사례: 빈 상태, 검증 오류, 매우 긴 입력, 경계 값
- 상태 유지: 새로고침, local/session storage, URL 프래그먼트
- 탐색: URL을 바꾸는 컨트롤과 뒤로/앞으로 동작

**중요**: playwright-cli로 앱 URL만 열지 마세요. 테스트의 사용자 정의 설정을 적용하려면 반드시 테스트를 거쳐야 합니다.
**중요**: 탐색이 끝나면 백그라운드 테스트를 중지합니다.

### 1.4 명세 파일 작성

`specs/<feature>.plan.md`에 다음 구조로 저장합니다.

```markdown
# <Feature> Test Plan

## Application Overview

<One paragraph describing what the feature does and why it matters.>

## Test Scenarios

### 1. <Group Name>

**Seed:** `tests/seed.spec.ts`

#### 1.1. <kebab-case-scenario-name>

**File:** `tests/<group>/<kebab-case-scenario-name>.spec.ts`

**Steps:**
  1. <Concrete user step>
    - expect: <observable outcome>
    - expect: <another observable outcome>
  2. <Next step>
    - expect: <outcome>

#### 1.2. <next-scenario>
...

### 2. <Next Group>

**Seed:** `tests/seed.spec.ts`
...
```

작성 지침:

- 각 시나리오는 독립적이며 초기 상태 테스트 직후의 새 상태에서 시작합니다. 시나리오를 이어 붙이지 마세요.
- 시나리오 이름은 kebab-case이며 테스트 파일 이름과 일치해야 합니다(`should-add-single-todo` → `should-add-single-todo.spec.ts`).
- 정상 흐름, 경계 사례, 검증, 실패 흐름, 상태 유지를 포함합니다.
- API 수준(‘`fill` 호출’)이 아니라 사용자 수준(‘입력란에 Buy milk 입력’)으로 단계를 씁니다.
- 관찰 가능한 결과는 `- expect:` 항목에 씁니다. 생성 시 각 항목이 검증문이 됩니다.

---

## 2. 생성

목표: 명세 파일을 Playwright 테스트 파일로 변환합니다. 실제 동작과 달라졌다면 명세도 갱신합니다.

### 2.1 입력

- **명세 파일**: 예를 들어 `specs/basic-operations.plan.md`
- **대상**: 단일 시나리오(예: `1.2`), 그룹 전체(`1`), 또는 모두
- **초기 상태 파일**: 시나리오 그룹의 `**Seed:**` 줄에서 확인

### 2.2 시나리오 하나 생성

대상 시나리오마다 순서대로 수행합니다(같은 초기 상태 세션을 공유하므로 병렬 실행 금지).

```bash
PLAYWRIGHT_HTML_OPEN=never npx playwright test <seed-file> --debug=cli   # background
playwright-cli attach tw-XXXX
# resume
```

playwright-cli로 앱 URL만 열지 **마세요**. 사용자 정의 설정을 적용하려면 반드시 테스트를 거쳐야 합니다.

명세를 계획으로, 실행 중인 앱을 실제 동작의 기준으로 삼아 `Steps:`를 `playwright-cli`로 하나씩 수행합니다. 단계가 모호하거나(‘버튼 클릭’인데 어떤 버튼인지 불명확), 사라진 요소를 가리키거나, 실제 동작과 충돌하면 판단하여 명세를 갱신하고 계속 진행합니다. 생성 도중 명세를 수정하는 것은 정상입니다.

각 작업은 대응하는 Playwright TypeScript를 출력합니다([생성 원리](#0-생성-원리) 참조).

```bash
playwright-cli snapshot                         # find refs
playwright-cli fill e3 "John Doe"               # -> page.getByRole('textbox', {...}).fill(...)
playwright-cli press Enter
playwright-cli click e7
```

각 `- expect:` 항목에 명시적인 검증문을 추가합니다. 자세한 내용은 [생성 원리](#0-생성-원리)를 참조하세요.

생성된 코드를 모아 명세에 지정된 경로에 테스트 파일을 작성합니다.

```ts
// spec: specs/basic-operations.plan.md
// seed: tests/seed.spec.ts
import { test, expect } from './fixtures';   // or '@playwright/test' if no fixtures file

test.describe('Signing in and out', () => {
  test('should sign in', async ({ page }) => {
    // 1. Navigate to the application
    // (handled by the seed fixture)

    // 2. Type 'John Doe' into the username field
    await page.getByRole('textbox', { name: 'username' }).fill('John Doe');

    // 3. Type password
    await page.getByRole('textbox', { name: 'password' }).fill('TestPassword');

    // 4. Press Enter to submit
    await page.getByRole('textbox', { name: 'password' }).press('Enter');

    await expect(page.getByRole('heading')).toContainText('Welcome, John Doe!');
  });
});
```

규칙:

- **파일 하나에 테스트 하나.** 파일 경로, describe 이름, 테스트 이름은 번호를 제외하고 명세 그대로 사용합니다.
- 각 번호가 있는 단계의 작업 앞에 `// N. <단계 설명>` 주석을 붙입니다.
- describe 그룹 이름은 명세 그대로 사용하되 `1.` 같은 번호는 제외합니다.
- 프로젝트에 fixture가 있으면 `./fixtures`에서, 없으면 `@playwright/test`에서 가져옵니다.
- **중요**: 다음 시나리오 전에 CLI 세션을 닫고 백그라운드 테스트를 중지합니다.

### 2.3 여러 시나리오 생성

대상 시나리오에 2.2를 하나씩 반복하고, 매번 초기 상태 테스트를 재시작하여 깨끗한 페이지에서 시작합니다. 생성된 세션 이름은 고유하므로 독립된 실행끼리는 병렬화할 수 있지만 각 테스트 실행을 반드시 중지해야 합니다.

### 2.4 생성된 테스트 실행

생성 후 새 테스트를 한 번 실행합니다.

```bash
PLAYWRIGHT_HTML_OPEN=never npx playwright test tests/<group>/<scenario>.spec.ts
```

실패하면 3절로 진행합니다.

---

## 3. 수정

목표: 실패한 테스트를 고치고 앱의 의도된 동작이 바뀌었다면 명세도 갱신합니다.

### 3.1 실패한 테스트 찾기

```bash
PLAYWRIGHT_HTML_OPEN=never npx playwright test
```

실패한 `<file>:<line>` 목록을 기록하고 하나씩 처리합니다. 공유 상태와 단일 CLI 세션 때문에 불안정해질 수 있으므로 병렬 수정하지 마세요.

### 3.2 실패 하나 디버깅

실패한 테스트 하나를 백그라운드 디버그 모드로 실행하고 연결합니다.

```bash
PLAYWRIGHT_HTML_OPEN=never npx playwright test tests/<group>/<scenario>.spec.ts:<line> --debug=cli
# wait for "Debugging Instructions" and the tw-XXXX session name
playwright-cli attach tw-XXXX
```

테스트는 시작 지점에서 멈춥니다. 단계별로 진행하거나 실패하는 작업/검증 직전까지 실행한 뒤 진단합니다.

```bash
playwright-cli snapshot                # did the element change / move / rename?
playwright-cli console                 # app-side errors?
playwright-cli requests                # failed request? wrong payload?
playwright-cli show --annotate         # ask the user to point somewhere
```

흔한 원인: 선택자 변경, 새로운 래퍼 요소, label/ARIA 이름 변경, 타이밍(전환/비동기 로드), 앱의 검증 대상 텍스트 변경, 실행 간 테스트 데이터 누출.

`playwright-cli`로 수정한 상호작용을 연습하고 출력에 생성된 코드를 테스트에 반영합니다.

### 3.3 수정 적용

올바른 동작에 맞게 테스트 파일의 로케이터, 검증문, 단계 순서, 입력을 수정합니다. 백그라운드 디버그 실행을 중지한 뒤 해당 테스트만 재실행하여 통과를 확인합니다.

해결책으로 hook을 건너뛰거나 sleep을 추가하지 마세요. `networkidle`도 사용하지 마세요.

### 3.4 명세와 일치시키기

테스트 파일의 `// spec:` 헤더가 가리키는 명세를 열고 해당 시나리오를 찾습니다.

- **기술적인 수정만 한 경우**(로케이터 변경, 검증 방식 개선)이며 사용자 관점의 명세 동작이 앱과 여전히 일치하면 명세를 유지합니다.
- **명세에 적힌 사용자 단계, 입력, 순서, 기대 결과가 바뀐 경우**에는 실제 동작에 맞게 명세를 갱신합니다. 시나리오 ID와 파일 경로는 유지하고 step / expect 줄만 수정합니다.
- **의도된 앱 변경**(명세가 오래됨)인지 **회귀 버그**(테스트는 맞고 앱이 잘못됨)인지 불명확하면 **멈추고 사용자에게 질문**합니다. 다음을 제공합니다.
  - 시나리오 ID(예: `2.3`)
  - 더 이상 일치하지 않는 명세 줄
  - 관찰한 앱 동작(스냅샷 일부 또는 구체적인 결과 인용)

사용자 답변 후 의도된 변경이면 명세를 갱신하고, 회귀 버그이면 버그를 확인하는 테스트로 기록하거나 표시합니다.

### 3.5 반복과 중단

- 실패를 하나씩 수정하고 매번 재실행합니다.
- 충분히 조사한 결과 테스트가 맞고 앱이 잘못되었다고 확신하며 **사용자도 버그임을 확인했다면**, 사용자 결정이나 이슈 링크를 주석에 적고 `test.fixme(...)`로 표시합니다. 말없이 건너뛰지 마세요.

---

## 관련 문서

| 내용 | 참고 |
|---|---|
| `--debug=cli` / 연결 방식 | [playwright-tests.md](playwright-tests.md) |
| 탐색/생성 중 요청 모킹 | [request-mocking.md](request-mocking.md) |
| CLI 브라우저 세션 관리 | [session-management.md](session-management.md) |
