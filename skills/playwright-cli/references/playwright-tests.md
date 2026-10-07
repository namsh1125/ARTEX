# Playwright 테스트 실행

`npx playwright test` 명령 또는 패키지 관리자 스크립트로 Playwright 테스트를 실행합니다. 대화형 HTML 보고서가 열리지 않도록 `PLAYWRIGHT_HTML_OPEN=never` 환경변수를 사용하세요.

```bash
# Run all tests
PLAYWRIGHT_HTML_OPEN=never npx playwright test

# Run all tests through a custom npm script
PLAYWRIGHT_HTML_OPEN=never npm run special-test-command
```

# Playwright 테스트 디버깅

실패한 테스트를 디버깅하려면 `--debug=cli` 옵션으로 실행합니다. 테스트 시작 지점에서 일시 정지하고 디버깅 안내를 출력합니다.

**중요**: 명령을 백그라운드로 실행하고 ‘Debugging Instructions’가 나올 때까지 출력을 확인하세요. 작업이 끝나면 반드시 명령을 중지합니다.

세션 이름이 포함된 안내가 출력되면 `playwright-cli`로 세션에 연결하여 페이지를 살펴봅니다.

```bash
# Run the test
PLAYWRIGHT_HTML_OPEN=never npx playwright test --debug=cli
# ...
# ... debugging instructions for "tw-abcdef" session ...
# ...

# Attach to the test
playwright-cli attach tw-abcdef
```

원인을 조사하고 수정하는 동안 테스트는 백그라운드에서 계속 실행해 둡니다.
테스트가 시작 지점에서 멈추므로 단계별로 진행하거나,
문제가 발생할 가능성이 높은 위치에서 일시 정지합니다.

`playwright-cli`로 수행한 각 작업은 대응하는 Playwright TypeScript 코드를 생성합니다.
이 코드는 출력에 표시되며 테스트에 바로 복사할 수 있습니다. 대부분 특정 로케이터나 검증 조건을 갱신하면 되지만 앱의 버그일 수도 있으므로 상황에 맞게 판단하세요.

수정 후 백그라운드 테스트를 중지하고 다시 실행하여 통과하는지 확인합니다.
