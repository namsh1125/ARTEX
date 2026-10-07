# `/btw` 검증 기록

날짜: 2026-09-10. 브랜치: `codex/btw-side-question`. 기준 커밋: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

이 문서는 원본 작성자가 수행한 당시 검증의 번역입니다. 한국어 번역 작업에서 새로 수행한 검사 결과와는 구분합니다.

독립 PostgreSQL 테스트 DB와 데이터 디렉터리를 사용했습니다. 실제 모델 인증 정보는 독립 테스트 환경에만 주입했고 코드나 이 기록에 저장하지 않았으며 제품 기본 모델도 바꾸지 않았습니다. 환경은 Go 1.26.3, norma v0.3.6, Next.js 16.2.9입니다.

실제 모델 대화, 반환 객체, 프로그램 검증 결과 및 Qwen 원본 검토는 [validation-2026-09-10.json](validation-2026-09-10.json)에 보존되어 있으며 API 인증 정보는 없습니다.

## 구현 검사

| 범위 | 결과 | 근거 |
| --- | --- | --- |
| 구조화 메시지·도구 인수 깊은 복사 | 통과 | `TestCheckpointDeepCopyAndBoundaries` |
| 요약·압축 요청의 덮어쓰기 방지, 완전한 응답·종료 상태 게시, 부분 응답 제외 | 통과 | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 실제 모델 풀 구성원 식별 | 통과 | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 도구 짝, 20개 재생, 예산 축소 및 초과 오류 | 통과 | `TestBuildRequestCompactionToolPairingAndBudget` |
| 주 흐름·별도 질문 병렬 실행 및 양방향 취소 격리 | 통과 | 차단형 Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| 도구 실행 없음, 스트리밍·비스트리밍, 실패 시 확보한 사용량 | 통과 | `TestServiceNoToolsAndUsageOnFailure` |
| 실제 norma ChatAgent와 로컬 Read, 주 transcript·활동 격리 | 통과 | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`의 스트리밍·비스트리밍 하위 사례 |
| 영구 저장, 페이지 조회, 멱등성, 재시작 후 부분 답변 유지 | 통과 | `TestSideHistoryIdempotencyPagingAndRecovery` |
| 기록 비우기와 늦은 쓰기 경쟁, 부모 삭제, 버전 비교 | 통과 | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent·Worker 보관 및 복원, v1/v2/v3 | 통과 | `TestSideTaskArchiveVersions` |
| 세 부모 API, 인증, 리소스 소속, Worker 논리 삭제 | 통과 | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| 실행 중인 주 세션의 별도 질문, 독립 SSE 재연결·해제·취소·비우기 | 통과 | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 부모당 1개 / 전역 4개 동시 실행 | 통과 | 두 `TestSideHTTP…` 사례 |
| 제출 전 스냅샷 저장, 재시작 후 후속 질문, 이전 세션의 스냅샷 위조 방지 | 통과 | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| 캐시 설정 삭제·모델 변경 시 계속 실행 거부 | 통과 | `TestSideRejectsDeletedOrChangedCachedProfile` |
| 보관 전 취소 및 최종 답변·사용량 저장 대기 | 통과 | `TestSideTaskDrainPersistsBeforeArchive` |
| 스트림 소비자 조기 취소 시 사용량 한 번 기록 및 별도 질문 귀속 | 통과 | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| 재시작으로 복원한 Worker·deadline 실행의 새 스냅샷 게시 | 통과 | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| 관련 패키지 race 검사 | 통과 | 아래 명령 |
| TypeScript 및 운영 빌드 | 통과 | `npx tsc --noEmit`, `npm run build` |
| 추가 프런트엔드 모듈 Biome | 통과 | 추가 모듈 3개의 `biome check` |

삭제 가능한 별도 DB를 `ARTEX_PG_DSN`으로 지정하면 자동 검사를 재현할 수 있습니다. 운영 DB를 지정하지 마세요.

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

전체 Go 회귀 검사가 모두 통과한 것은 아닙니다. `server`의 기존 테스트 두 개가 임시 디렉터리 정리 중 `TempDir RemoveAll … directory not empty`로 실패했습니다.

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

변경하지 않은 위 기준 커밋의 소스로 같은 격리 환경에서 `server`를 재실행해 두 실패를 재현했습니다. 기준 실행에서는 `TestCoreTaskLifecyclePG`의 목표 노드 수 검사도 실패했으나 최종 변경본에서는 해당 실패가 없었습니다. 다른 패키지와 이번 별도 질문 관련 사례 및 race 검사는 통과했습니다. 기준 커밋의 문제를 이번 통과 항목으로 표시하거나 숨기기 위해 기존 검증 조건을 바꾸지 않았습니다.

Next.js는 기존의 여러 lockfile 및 workspace root 추론 경고를 출력했으나 빌드와 전체 페이지 생성은 완료했습니다.

## 브라우저 검사

Codex In-app Browser로 독립 로컬 Go 서비스와 Next.js 개발 서버에 연결했습니다. 데스크톱 및 390×844 화면에서 다음을 조작하고 스크린샷·브라우저 로그를 확인했습니다.

- 일반 대화 실행 중 `/btw`를 입력하면 주 내용과 별도 질문이 동시에 표시되고 데스크톱 사이드바가 정상 동작함.
- 연속 후속 질문 및 별도 질문 중지 후 부분 답변 유지. 주 흐름은 계속 실행됨.
- 패널을 닫아도 요청이 계속되고 다시 열면 완료된 답변 복원. 새로 고침 후 빈 `/btw`로 기록 복원.
- 좁은 화면의 Drawer 입력, 버튼, 기록 및 닫기 정상. 가로 넘침 없음.
- 확인 대화상자로 기록을 비우면 별도 질문 기록만 사라지고 주 transcript와 스냅샷은 유지됨.
- 작업 MainAgent와 Worker 두 개에서 각각 질문·전환하여 에이전트 표시와 기록이 섞이지 않음을 확인.
- 차단형 로컬 모델 픽스처로 Worker 실행을 유지하고 주 입력창에서 `/btw` 제출. 별도 질문을 중지해도 Worker는 실시간 실행 상태와 자체 일시 중지 버튼을 유지하며 부분 답변 저장.
- 브라우저 오류·경고 로그 없음.

동시 실행 타이밍을 정확히 확인하기 위해 실제 모델 출력 속도와 무관한 제어 가능한 픽스처를 사용했습니다. 초기 두 Worker 검사는 작업 또는 답변이 먼저 종료되어 유효한 동시 실행 구간을 만들지 못했습니다. 픽스처를 수정한 뒤 재검사하여 통과했으며 초기 시도는 통과로 계산하지 않았습니다.

## 실제 모델 대화

OpenAI 호환 엔드포인트 `http://127.0.0.1:12580/tingly/openai`에서 `grok-4.6`을 우선 확인했습니다. HTTP 200, 모델명 `grok-4.6`, `READY`를 2.82초 만에 반환했습니다. 우선 모델이 작동하여 Tingly `glm` 및 Zhipu `glm-5.3` 대체 체인은 사용하거나 검증하지 않았습니다.

| 상황 | 실제 결과 |
| --- | --- |
| 주 세션 실행 중 자산·목표·표식 질문 | `redhaze.top`, 홈페이지 읽기·요약 목표, `BTW-REAL-0910` 반환. 16.97초 |
| 홈페이지 읽기 완료 후 도구 근거 질문 | WebFetch 200, curl 301 → 302 → 200 및 페이지 제목을 정확히 인용. 7.24초 |
| 별도 질문에서 Bash로 테스트 파일 생성 요청 | 실행 거부, 파일 생성되지 않음. 7.74초 |
| 별도 질문 완료 후 주 컨텍스트 불변 | 주 transcript SHA-256 및 활동 기록 동일, 별도 질문 도구 실행 0회 |
| Go 서비스 실제 종료·재시작 후 후속 질문 | 이전 질문 3개 유지, 주 에이전트 재실행 없이 저장된 스냅샷으로 자산·표식·제목 답변 |
| 새 세션의 Grok 비스트리밍 설정 | 자산과 `ATOMIC-0910`에 정확히 답변. input 11734, output 138, cache_read 11520 저장 |

자산 사례의 주 세션은 WebFetch와 Bash/curl로 공개 홈페이지를 읽었습니다. 최종 페이지는 `https://id.redhaze.top/home`이며 제목은 원문 그대로 “红幕科技 RedHaze Group · 全球综合集团门户”였습니다. Bash는 응답을 로컬 테스트 파일에 저장했지만 원격 쓰기는 하지 않았습니다. 이는 「별도 질문에서 도구를 실행하지 않음」과 별도로 검증했습니다.

주 transcript 검증값: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**사용량 한계:** Tingly의 Grok 스트리밍 응답에는 usage가 없었습니다. `stream_options.include_usage=true`로 직접 확인한 결과 HTTP 200, 데이터 프레임 12개, usage 프레임 0개였습니다. 따라서 스트리밍 테스트의 0은 사용량을 제공하지 않았다는 뜻이며 과금이 없었다는 뜻이 아닙니다. 비스트리밍 사용량과 픽스처의 실패·취소 사용량은 정상 저장했습니다.

## Qwen 검토

OpenAI 호환 엔드포인트 `https://dashscope.aliyuncs.com/compatible-mode/v1`의 `qwen-flash`에서 HTTP 200을 받았습니다. 앞의 실제 별도 질문 세 사례, 주 세션의 도구 근거 및 프로그램 검증 결과를 제공했습니다. 결과는 `verdict: accept`, `concerns: []`로 자산·표식·페이지 읽기 근거와 답변이 일치하고 도구 실행 거부가 제약에 맞는다고 판단했습니다. 사용량은 prompt 6625, completion 312, total 6937입니다.

이 검토에는 이후 추가한 서비스 재시작 및 비스트리밍 테스트가 포함되지 않았습니다. Qwen의 「쓰기 없음」이라는 요약은 지나치게 넓었습니다. 주 세션의 curl은 위에서 명시한 대로 로컬 임시 응답 파일을 만들었습니다. 동시 실행, 도구 실행 0회 및 transcript 격리는 프로그램 검증으로 판단하며 모델 검토는 답변 품질 평가를 보조할 뿐입니다.
