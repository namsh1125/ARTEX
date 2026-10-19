# Codex CLI 연결 서버 (실험 기능)

ARTEX의 OpenAI 호환 요청을 로그인된 `codex exec` 호출로 연결합니다.
API 키 대신 사용자가 로컬에서 로그인한 Codex 계정을 사용합니다.
기존 ARTEX 서버·DB·프런트엔드 코드는 바꾸지 않습니다.

```text
ARTEX OpenAI provider → localhost 연결 서버 → codex exec
                     ← assistant / tool_calls ← 구조화된 JSON 결과
```

도구 호출은 Codex 네이티브 function calling이 아니라 **구조화된 출력으로 변환한 호출 제안**입니다.
연결 서버가 이름·JSON 인자·인자 스키마를 검사한 뒤 ARTEX에 돌려줍니다.
도구 실행과 승인 처리는 기존 ARTEX가 담당합니다. 따라서 모델 응답 품질과 실제 작업 흐름은
사용자 환경에서 추가 확인이 필요합니다. 이 연결 서버를 추가한다고 Codex 사용 한도가 없어지거나
항상 로컬 LLM보다 빨라지는 것은 아닙니다.

## 1. 준비

- Python 3.10 이상
- 현재 Codex CLI: `codex exec --help`에 `--ignore-user-config`, `--ephemeral`,
  `--json`, `--output-schema`, `--output-last-message`가 있는 버전
- 같은 OS 사용자·환경에서 완료한 `codex login`
- macOS / Linux / WSL 환경. Windows 노트북에서는 **WSL에서 CLI·연결 서버·ARTEX 백엔드**를
  함께 실행하는 구성을 사용하세요. Windows 네이티브 실행은 검증하지 않았습니다.

공식 CLI 설치 방법은 [Codex CLI 문서](https://developers.openai.com/codex/cli/)를 참고합니다.
설치 후 로그인 상태를 확인합니다.

```bash
codex login
codex login status
codex exec --help
```

## 2. 연결 서버 실행

저장소 루트에서 실행합니다.

```bash
python3 -m venv tools/codex_bridge/.venv
tools/codex_bridge/.venv/bin/python -m pip install -r tools/codex_bridge/requirements.txt

# 이 값은 로컬 연결 서버 접속용 비밀번호입니다. OpenAI 토큰이 아닙니다.
export ARTEX_CODEX_BRIDGE_TOKEN="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"

tools/codex_bridge/.venv/bin/python tools/codex_bridge/bridge.py --effort low
```

토큰은 ARTEX의 API Key 필드에도 동일하게 넣습니다. 위 환경변수는 현재 터미널에만 남으므로
서버를 시작하기 전에 별도의 로컬 비밀번호 저장소에 보관하세요. 저장소에 커밋하지 마세요.
새 토큰으로 서버를 다시 시작했다면 ARTEX 설정도 갱신해야 합니다.

기본 주소는 `http://127.0.0.1:8766/v1`입니다. 실제 모델을 지정하려면 계정에서 사용 가능한
모델 ID를 `--model`로 지정합니다. 생략하면 Codex의 기본 모델을 사용합니다.
`--ignore-user-config`를 사용하므로 개인 `config.toml`의 모델·MCP·훅 설정은 로드하지 않습니다.
로그인은 CLI가 관리하며 이 프로그램은 인증 파일을 읽거나 복사하지 않습니다.

| 실행 옵션 | 기본값 | 용도 |
| --- | --- | --- |
| `--port` | `8766` | 로컬 포트 |
| `--codex` | `codex` | CLI 실행 파일 경로 |
| `--model` | CLI 기본 모델 | 실제 사용할 모델 |
| `--effort` | CLI 기본값 | `low`, `medium`, `high`, `xhigh` 중 모델이 지원하는 값 |
| `--timeout` | `240` | 요청별 CLI 최대 실행 시간(초) |

## 3. ARTEX LLM 설정

새 LLM 프로필을 만들고 해당 작업 또는 대화에 선택합니다.

| 필드 | 값 |
| --- | --- |
| 형식 / Provider | `OpenAI` (`openai`, Responses 형식 아님) |
| Base URL | `http://127.0.0.1:8766/v1` |
| API Key | `ARTEX_CODEX_BRIDGE_TOKEN`과 같은 값 |
| Model | `codex-cli` (연결 서버의 고정 별칭) |
| 스트리밍 | **끔 권장** |
| Thinking / Reasoning effort | 미설정; 실제 추론 강도는 서버의 `--effort`로 지정 |
| 프록시 | 미설정 |
| 출력 토큰 한도 | 기본값; 이 연결 서버는 API의 토큰 한도를 강제하지 않음 |

첫 검증은 동시 작업 없이 대화 하나로 수행하세요. 연결 서버는 CLI 호출을 한 번에 하나만 처리하고
동시 요청에는 HTTP 429를 반환합니다. 작업 에이전트의 병렬 수를 1로 낮춰도 다른 대화·요약 작업이
겹칠 수 있습니다. 기존 ARTEX 재시도 정책을 무제한 반복으로 설정하지 마세요.

환경변수 방식으로 ARTEX를 시작한다면 같은 터미널에서 다음 설정도 가능합니다.
DB에 저장된 LLM 프로필을 사용하는 경우에는 UI 프로필을 수정해야 합니다.

```bash
export ARTEX_LLM_PROVIDER=openai
export ARTEX_LLM_BASE_URL=http://127.0.0.1:8766/v1
export ARTEX_LLM_MODEL=codex-cli
export ARTEX_LLM_STREAM=false
export OPENAI_API_KEY="$ARTEX_CODEX_BRIDGE_TOKEN"
# 이후 평소 사용하는 명령으로 ARTEX 백엔드를 실행합니다.
```

여기의 `OPENAI_API_KEY`는 ARTEX가 로컬 서버에 보내는 인증 값입니다.
연결 서버는 자식 프로세스에서 `OPENAI_API_KEY`와 `CODEX_API_KEY`를 제거하고
`forced_login_method="chatgpt"`를 사용합니다.

### Docker 구성

컨테이너의 `127.0.0.1`은 호스트가 아닙니다. 현재 연결 서버는 루프백에만 바인딩하며,
기본 Docker bridge 네트워크에서 `host.docker.internal`만 지정한다고 연결되지 않습니다.
첫 실행은 ARTEX 백엔드를 호스트/WSL에서 소스 빌드해 실행하고 DB만 Docker로 운영하세요.
전체 Docker 배포용 네트워크 연결과 로그인 전달은 이번 변경 범위에 포함하지 않습니다.

## 4. 실제 로그인으로 확인

연결 서버가 실행 중인 상태에서 **토큰 환경변수가 있는 다른 터미널**로 테스트합니다.

```bash
curl --max-time 250 http://127.0.0.1:8766/v1/chat/completions \
  -H "Authorization: Bearer $ARTEX_CODEX_BRIDGE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"codex-cli","messages":[{"role":"user","content":"Reply with OK."}],"stream":false}'
```

`choices[0].message.content`가 오면 기본 연결이 성공한 것입니다.
`usage.prompt_tokens`, `usage.completion_tokens`, `usage.total_tokens`도 확인하세요.
캐시 입력은 `usage.prompt_tokens_details.cached_tokens`로 전달합니다.
그다음 ARTEX의 일반 대화 → 무해한 도구 호출 → 도구 결과를 읽는 후속 응답 순으로 확인합니다.
ARTEX의 UI 연결 테스트에는 기존 30초 제한이 있어 CLI 첫 실행이 오래 걸리면 실패할 수 있습니다.
위 curl로 먼저 구분하세요. 이 변경은 기존 전역 연결 테스트 시간을 변경하지 않습니다.

## 지원 범위와 한계

- `POST /v1/chat/completions`, `GET /v1/models` 지원. 텍스트 메시지와 function 도구만 지원합니다.
- 대화 전체와 도구 호출 ID를 매 요청 전달합니다. `resume --last`로 대화를 섞지 않습니다.
- `tool_choice`의 auto/none/required/지정 도구와 `parallel_tool_calls=false`를 검증합니다.
- 스트리밍 요청은 **응답 완성 후** SSE 프레임으로 반환합니다. 실시간 토큰 스트리밍이 아닙니다.
- 토큰 사용량은 `codex exec --json`의 `turn.completed.usage`에서 수집합니다.
  입력·출력·캐시 입력과 CLI가 제공하는 추론 토큰을 Chat Completions `usage`로 전달합니다.
  캐시는 입력의 부분집합, 추론은 출력의 부분집합이므로 총합에 다시 더하지 않습니다.
  스트리밍은 `stream_options: {"include_usage": true}`일 때 `[DONE]` 직전에
  `choices: []`인 사용량 전용 이벤트를 한 번 보냅니다.
  CLI가 사용량 이벤트를 제공하지 않으면 추측하지 않고 `usage`를 생략합니다.
  사용량은 CLI에 전달된 전체 프롬프트·도구 정의·구조화 출력 등을 포함하며 요금이나 구독 잔여량은 아닙니다.
  이전 버전이 기록하지 않은 과거 토큰은 복구하지 않습니다. 업데이트 후 연결 서버를 재시작해야
  이후 호출부터 ARTEX에 집계됩니다. 원본 CLI 이벤트는 임시 파일로만 처리하고 종료 시 정리합니다.
- `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `reasoning_effort`, `user`는
  기존 클라이언트와의 호환을 위해 수신하지만 CLI에 매핑하지 않습니다. 출력 토큰 상한은 보장하지 않습니다.
- Responses API, 이미지·오디오, `response_format`, `stop`, `thinking` 등 미지원 필드는 400으로 거절합니다.
- CLI 시작 비용과 전체 이력 재전송이 매 턴 발생합니다. 실제 계정에서 지연·사용 한도·도구 호출 성공률을
  측정하기 전에는 성능 개선을 보장할 수 없습니다.
- HTTP 클라이언트가 연결을 끊어도 진행 중인 CLI는 완료 또는 서버 타임아웃까지 실행될 수 있습니다.
- 로컬 개발용 서버입니다. 공용 API나 다중 사용자 서비스로 배포하지 않습니다.

## 오류 확인

| 상태 | 의미 / 확인할 사항 |
| --- | --- |
| 400 | `model=codex-cli`, OpenAI 형식, 텍스트 입력, 미지원 설정 확인 |
| 401 | ARTEX API Key와 연결 서버 토큰 일치 여부 |
| 403 | 브라우저 Origin 요청 거절; ARTEX 백엔드에서 호출해야 함 |
| 413 | 요청 본문 4 MiB 초과 등 길이 오류; 대화 이력 축소 |
| 429 | 이전 요청 처리 중; 동시 작업 축소 |
| 502 | CLI 실행 실패 또는 잘못된 구조화 응답. 같은 사용자로 `codex login status`, `codex exec --help`, 모델 접근·사용 한도 확인 |
| 503 | CLI 실행 파일을 찾을 수 없음 |
| 504 | CLI 실행 시간 초과. 이력 축소·추론 강도 조절 또는 `--timeout` 변경 |

CLI stderr와 프롬프트·토큰을 HTTP 오류나 서버 로그로 노출하지 않습니다.
CLI 자체 오류를 조사하려면 별도 터미널에서 무해한 짧은 프롬프트로 직접 실행하세요.

## 개발 검증

```bash
tools/codex_bridge/.venv/bin/python -m unittest discover -s tools/codex_bridge -v
```

테스트는 가짜 CLI 프로세스와 로컬 HTTP 서버를 사용하며 계정·외부 모델 호출이 필요하지 않습니다.
본문/도구 호출 변환, 인자 스키마·호출 선택 검증, 인증, 동시성 제한, JSON/SSE,
프로세스 stdin 전달·비정상 종료·타임아웃·임시 파일 정리, 실제 사용량 이벤트 변환,
캐시 중복 합산 방지, JSON/SSE 사용량 전달·누락·오류 처리를 확인합니다.
ARTEX 전체 시스템과 실제 로그인된 Codex를 묶은 E2E, Windows 네이티브 동작,
로컬 LLM 대비 성능은 별도 검증이 필요합니다.

공식 근거: [비대화형 실행·로그인 재사용·출력 스키마](https://developers.openai.com/codex/noninteractive/),
[CLI 설정](https://developers.openai.com/codex/config-reference/).
