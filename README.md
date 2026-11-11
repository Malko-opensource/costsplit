# graybox

코딩 에이전트가 부르는 CLI 도구입니다. 작고 저렴한 모델이 코드를 고치다가 실패하는 것을 줄이는 게 목적입니다. 설계는 원본 문서와 실행 명세 r3를 따릅니다(결정 ID D-001~D-020).

- 큰 모델이 그레이박스를 쓰고 사람이 승인합니다. 그레이박스는 포트, 시그니처, 상태 전이표, 요구사항별 테스트 이름을 담습니다.
- 작은 모델은 계획을 내고, 함수 본문만 제출합니다. 도구가 그 본문을 끼워 넣습니다.
- 도구는 기계 검사를 먼저 돌립니다. 모두 통과해야 경량 결정 모델에게 판정을 묻습니다.
- 결과로 `next`를 권고합니다. 권고를 따를지는 에이전트가 정합니다.

## 구조

```text
cmd/graybox            CLI: lock, plan, apply, check
internal/decider       결정 API 클라이언트. 응답이 형식에 안 맞으면 ErrUnavailable (D-009, D-012)
internal/verdict       관문별 질문 만들기와 판정 (D-003, D-016)
internal/policy        next 계산: 재시도, 승격, paused (D-011, D-015)
internal/ledger        .graybox/decisions.jsonl 기록과 시도 횟수 세기
internal/manifest      manifest.json, questions.json, plan.json 검증
internal/runner        언어 중립 코어 (D-008, D-018)
internal/adapter       어댑터 인터페이스
internal/goadapter     Go 어댑터 (go/ast)
internal/extadapter    외부 프로세스 어댑터 (JSON stdin/stdout)
adapters/python        Python 어댑터 (표준 ast, pyright strict)
adapters/ts            TypeScript 어댑터 (typescript 5.9 컴파일러 API, tsc, node --test)
adapters/jvm           Java 어댑터 (JavaParser, javac, JUnit 6)와 Kotlin 어댑터 (컴파일러 PSI, kotlinc, JUnit 6)
examples/{go,python,ts,java,kotlin}/order   언어별 주문 도메인 그레이박스
```

## 사용법

```bash
go build -o graybox ./cmd/graybox
(cd adapters/python && npm install)   # pyright
(cd adapters/ts && npm install)       # typescript
adapters/jvm/fetch-libs.sh            # JavaParser, JUnit 콘솔 jar (SHA-1 확인)
export GRAYBOX_HOME=$PWD              # Python, TS, Java, Kotlin 어댑터 위치
```

프로젝트의 `.graybox/`에 다음 파일이 있어야 합니다.

- `config.json`
- `manifest.json`
- `questions.json` (없어도 됩니다)

```bash
graybox lock  -dir PROJECT                                  # 사람이 승인할 때: api.lock 생성
graybox plan  -dir PROJECT -task T1 -model small -plan plan.json
graybox apply -dir PROJECT -task T1 -model small -symbol 'order.(*Service).Apply' -body-file body.txt
graybox check -dir PROJECT                                  # 파일을 직접 고쳤는지 확인
```

모든 명령은 JSON 하나를 출력합니다. 그중 `next`가 권고입니다.

| next | 뜻 |
| --- | --- |
| `proceed` | 계획 통과. 구현으로 넘어감 |
| `done` | 완료 판정 통과 |
| `retry_small` / `retry_large` | `feedback`을 보고 다시 시도 |
| `escalate_large` / `escalate_human` | 한도 초과, `unsure`, 또는 낮은 confidence |
| `paused` | 결정 모델 오류(타임아웃, 429, 형식 오류). pass나 승격으로 처리하지 않음 |

`config.json` 예시입니다. `limits`에는 기본값이 없어서 반드시 적어야 합니다. 명세에서 값이 아직 미정이기 때문입니다(Q-007, Q-012).

```json
{
  "lang": "go | python | ts | java | kotlin",
  "decider": { "url": "...", "model": "...", "api_key_env": "GRAYBOX_DECIDER_KEY", "timeout_seconds": 30 },
  "limits": { "small_retries": 2, "large_retries": 1, "confidence_threshold": 0.8 }
}
```

## 언어 어댑터 프로토콜

`<cmd> <op>` 형태로 실행합니다. stdin으로 JSON을 받고, stdout으로 JSON을 냅니다. 작업 디렉터리는 프로젝트 루트입니다.

| op | 입력 | 출력 |
| --- | --- | --- |
| `resolve` | `{symbol}` | `{symbol:{name,file,is_stub,signature}}` |
| `apply` | `{symbol, body}` | `{}`. 시그니처가 바뀌면 되돌리고 오류 |
| `checks` | `{}` | `{checks:[{name,status,detail}]}` |
| `preconditions` | `{}` | 엄격 타입 설정 확인 (D-019) |
| `list_tests` | `{}` | `{tests:[...]}` |
| `api_snapshot` | `{}` | `{lines:[...]}`. 공개 API와 전이표 |

새 언어를 추가하려면 이 프로토콜을 구현하는 어댑터 하나만 만들면 됩니다.

## 검증 상태

`go test ./...`로 확인합니다. 결정 모델은 가짜로 대체합니다.

| 수용 기준 | Go | Python | TS | Java | Kotlin |
| --- | --- | --- | --- | --- | --- |
| AC-1 기계 검사 실패 시 결정 모델 미호출 | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-2 잘못된 계획은 결정 모델 없이 거부 | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-3 apply 후 공개 API 불변, 완료 | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-4 시그니처 직접 수정을 check가 탐지 | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-5 결정 모델 형식 오류·다운 시 paused | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-6 낮은 confidence는 승격 | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-7 재시도 한도 초과 시 승격 사다리 | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-9 완료 질문 = REQ 집합 + test_gaming | 통과 | 통과 | 통과 | 통과 | 통과 |
| AC-11 엄격 타입을 끄면 lock 거부 | 해당 없음 | 통과 | 통과 | 해당 없음 | 해당 없음 |
| AC-8 기존 에이전트 테스트로 실패율·비용 측정 | 미실행 | 미실행 | 미실행 | 미실행 | 미실행 |

## 아직 안 된 것과 알려진 한계

- **Java/Kotlin은 JDK와 kotlinc가 필요합니다.** 기본 경로는 Homebrew(`/opt/homebrew/opt/openjdk`)이며, `JAVA_HOME`, `KOTLIN_HOME`으로 바꿀 수 있습니다. Kotlin 어댑터는 `KotlinCoreEnvironment`를 리플렉션으로 호출합니다. 컴파일러 내부 API라서 Kotlin 버전이 바뀌면 깨질 수 있습니다(2.4.21에서 확인).
- **JVM 빌드 도구 연동 없음.** Gradle, Maven을 쓰지 않습니다. `src/main/{java,kotlin}`, `src/test/{java,kotlin}`을 javac/kotlinc로 직접 컴파일합니다. 의존성이 있는 실제 프로젝트에서는 빌드 도구를 연동해야 합니다.
- **오버로드된 메서드는 심볼로 지정할 수 없습니다(Java, Kotlin).**
- **Kotlin 검사는 1회 약 7초**로 다른 언어보다 느립니다.
- **MCP 서버 없음.** 지금은 CLI만 있습니다.
- **실제 결정 모델로 검증하지 않음.** HTTP 클라이언트는 로컬 가짜 서버로만 확인했습니다.
- **apply로는 import를 추가하거나 새 헬퍼 함수를 만들 수 없습니다.** 그레이박스에 미리 두어야 합니다(Q-014).
- **공개 API 비교는 자체 스냅샷입니다.** 문서에서 제안한 apidiff, api-extractor, griffe를 쓰지 않고, 선언문을 정규화한 텍스트를 비교합니다.
- **설치돼 있지 않은 린터는 skipped로 처리합니다.** golangci-lint, ruff, eslint, Checkstyle, ktlint/detekt가 해당합니다.
- **Go 테스트는 `-race` 없이 돌립니다.**
