# 동영상 녹화

디버깅, 문서화, 검증을 위해 브라우저 자동화 세션을 동영상으로 녹화합니다. WebM(VP8/VP9 코덱)으로 출력합니다.

## 기본 녹화

```bash
# Open browser first
playwright-cli open

# Start recording
playwright-cli video-start demo.webm

# Add a chapter marker for section transitions
playwright-cli video-chapter "Getting Started" --description="Opening the homepage" --duration=2000

# Navigate and perform actions
playwright-cli goto https://example.com
playwright-cli snapshot
playwright-cli click e1

# Add another chapter
playwright-cli video-chapter "Filling Form" --description="Entering test data" --duration=2000
playwright-cli fill e2 "test input"

# Stop and save
playwright-cli video-stop
```

## 권장 사항

### 1. 내용을 알 수 있는 파일 이름 사용

```bash
# Include context in filename
playwright-cli video-start recordings/login-flow-2024-01-15.webm
playwright-cli video-start recordings/checkout-test-run-42.webm
```

### 2. 주요 시나리오 전체 녹화

사용자에게 보여 주거나 작업 결과를 증명할 동영상은 코드 조각을 작성하고 run-code로 실행하는 것이 좋습니다.
작업 사이에 적절한 대기를 넣고 동영상에 주석을 추가할 수 있습니다. 이를 위한 새로운 Playwright API가 제공됩니다.

1) CLI로 시나리오를 수행하며 모든 로케이터와 작업을 기록합니다. 강조 표시용 경계 상자를 얻을 때 해당 로케이터가 필요합니다.
2) 아래 예시처럼 녹화용 스크립트 파일을 만듭니다. 자연스러운 입력에는 delay가 있는 pressSequentially를 사용하고 적절한 대기를 넣습니다.
3) `playwright-cli run-code --filename your-script.js`를 사용합니다.

**중요**: 오버레이는 `pointer-events: none`이므로 페이지 상호작용을 방해하지 않습니다. 고정 오버레이가 보이는 상태에서도 클릭, 입력 등 모든 작업을 안전하게 수행할 수 있습니다.

```js
async page => {
  await page.screencast.start({ path: 'video.webm', size: { width: 1280, height: 800 } });
  await page.goto('https://demo.playwright.dev/todomvc');

  // Show a chapter card — blurs the page and shows a dialog.
  // Blocks until duration expires, then auto-removes.
  // Use this for simple use cases, but always feel free to hand-craft your own beautiful
  // overlay via await page.screencast.showOverlay().
  await page.screencast.showChapter('Adding Todo Items', {
    description: 'We will add several items to the todo list.',
    duration: 2000,
  });

  // Perform action
  await page.getByRole('textbox', { name: 'What needs to be done?' }).pressSequentially('Walk the dog', { delay: 60 });
  await page.getByRole('textbox', { name: 'What needs to be done?' }).press('Enter');
  await page.waitForTimeout(1000);

  // Show next chapter
  await page.screencast.showChapter('Verifying Results', {
    description: 'Checking the item appeared in the list.',
    duration: 2000,
  });

  // Add a sticky annotation that stays while you perform actions.
  // Overlays are pointer-events: none, so they won't block clicks.
  const annotation = await page.screencast.showOverlay(`
    <div style="position: absolute; top: 8px; right: 8px;
      padding: 6px 12px; background: rgba(0,0,0,0.7);
      border-radius: 8px; font-size: 13px; color: white;">
      ✓ Item added successfully
    </div>
  `);

  // Perform more actions while the annotation is visible
  await page.getByRole('textbox', { name: 'What needs to be done?' }).pressSequentially('Buy groceries', { delay: 60 });
  await page.getByRole('textbox', { name: 'What needs to be done?' }).press('Enter');
  await page.waitForTimeout(1500);

  // Remove the annotation when done
  await annotation.dispose();

  // You can also highlight relevant locators and provide contextual annotations.
  const bounds = await page.getByText('Walk the dog').boundingBox();
  await page.screencast.showOverlay(`
    <div style="position: absolute;
      top: ${bounds.y}px;
      left: ${bounds.x}px;
      width: ${bounds.width}px;
      height: ${bounds.height}px;
      border: 1px solid red;">
    </div>
    <div style="position: absolute;
      top: ${bounds.y + bounds.height + 5}px;
      left: ${bounds.x + bounds.width / 2}px;
      transform: translateX(-50%);
      padding: 6px;
      background: #808080;
      border-radius: 10px;
      font-size: 14px;
      color: white;">Check it out, it is right above this text
    </div>
  `, { duration: 2000 });

  await page.screencast.stop();
}
```

오버레이를 활용해 창의적으로 구성해 보세요.

### 오버레이 API 요약

| 메서드 | 용도 |
|--------|----------|
| `page.screencast.showChapter(title, { description?, duration?, styleSheet? })` | 배경을 흐리게 하는 전체 화면 챕터 카드. 구간 전환에 적합 |
| `page.screencast.showOverlay(html, { duration? })` | 사용자 정의 HTML 오버레이. 설명, 라벨, 강조 표시에 사용 |
| `disposable.dispose()` | duration 없이 추가한 고정 오버레이 제거 |
| `page.screencast.hideOverlays()` / `page.screencast.showOverlays()` | 모든 오버레이를 일시적으로 숨기기/표시 |

## 추적과 동영상 비교

| 기능 | 동영상 | 추적 |
|---------|-------|---------|
| 출력 | WebM 파일 | Trace 파일(Trace Viewer에서 열람) |
| 내용 | 시각적 녹화 | DOM 스냅샷, 네트워크, 콘솔, 작업 |
| 용도 | 데모, 문서화 | 디버깅, 분석 |
| 크기 | 더 큼 | 더 작음 |

## 한계

- 녹화는 자동화에 약간의 부하를 추가합니다.
- 큰 녹화 파일은 상당한 디스크 공간을 사용할 수 있습니다.
