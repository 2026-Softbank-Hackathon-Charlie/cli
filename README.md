# charlie

HopCharlie 플랫폼 API를 터미널에서 쓰는 CLI입니다. 웹 콘솔과 같은 API를 부르고, 응답 타입은 platform 레포의 `pkg/apiv1`(OpenAPI 명세에서 생성)을 그대로 씁니다.

## 설치

platform 레포가 private이라 한 번 설정이 필요합니다.

```bash
go env -w GOPRIVATE=github.com/2026-Softbank-Hackathon-Charlie
go install github.com/2026-Softbank-Hackathon-Charlie/cli/cmd/charlie@latest
```

직접 빌드할 때는 다음 명령을 씁니다.

```bash
go build -o bin/charlie -ldflags "-X main.version=v0.1.0" ./cmd/charlie
```

## 로그인

```bash
charlie login --server https://<플랫폼 API 주소>   # 토큰을 묻는다. 입력은 화면에 보이지 않는다
charlie whoami
charlie logout
```

- 로그인 정보는 `%AppData%\charlie\config.json`(Windows) 또는 `~/.config/charlie/config.json`에 저장됩니다. Linux와 macOS에서는 권한이 0600입니다.
- `CHARLIE_SERVER`, `CHARLIE_TOKEN` 환경 변수가 저장된 값보다 우선하고, `--server` 플래그가 그보다 우선합니다.
- 저장된 토큰은 로그인한 서버에만 보냅니다. 다른 서버를 지정하면 `CHARLIE_TOKEN`을 주거나 그 서버로 다시 로그인해야 합니다.
- 토큰의 역할(viewer, agent, operator)에 따라 쓸 수 있는 명령이 다릅니다. agent 토큰으로는 승인이나 실행을 할 수 없습니다.

## 명령

| 명령 | 설명 |
| --- | --- |
| `env list`, `env show NAME` | 환경과 클러스터 실시간 상태 (노드, 버전, CNPG, 쓰기를 받는 환경) |
| `plan submit -f FILE` | 엔진 Plan 제출 (`-f -`는 표준 입력) |
| `plan list`, `plan show REF` | Plan 목록과 상세 (상세에는 DB 비밀번호가 전송될 호스트도 표시) |
| `plan approve REF [--comment]`, `plan reject REF --comment` | 승인, 거절 |
| `migrate start PLANREF [--watch]` | 이전 시작. 옵션은 아래에 따로 설명 |
| `migrate list`, `migrate status RUNID` | 실행 목록과 상세 |
| `migrate watch RUNID` | 단계, 다운타임, 복제 지연, 실패 요청 수, 누락 수를 실시간으로 표시 |
| `migrate switch RUNID [--watch]` | `awaiting_switch` 상태인 run의 전환 승인 |
| `migrate abort RUNID [--reason]` | 중단 |
| `migrate resume RUNID [--watch]` | interrupted, failed, aborted run 재개 |
| `traffic [--watch] [--interval 1s]` | 게이트웨이 상태와 환경별 요청 통계 |
| `matrix` | 6개 경로 결과표 |
| `audit [--limit N] [--before ID]` | 감사 로그 |
| `version` | 버전 |

`migrate start` 옵션은 다음과 같습니다.

- `--auto-switch`: 데이터가 따라잡히면 승인을 기다리지 않고 바로 전환합니다.
- `--canary 10,50 --canary-step 20s`: 전환 전에 읽기 요청을 대상으로 10%, 50% 순서로 보내고 게이트를 판정합니다.
- `--max-error-rate`, `--max-p95`, `--min-requests`: 게이트 기준을 바꿉니다. 지정하지 않으면 서버 기본값을 씁니다.
- `--idempotency-key`: 같은 키로 다시 보내면 새 run을 만들지 않고 처음 run을 돌려받습니다. 지정하지 않으면 자동으로 만들어 출력합니다.

모든 명령에 `--json`을 붙이면 API 응답을 JSON으로 출력합니다. `migrate watch --json`은 메시지마다 `{"event", "id", "data"}` 한 줄을 출력합니다. 색은 `--no-color`나 `NO_COLOR`로 끕니다.

## 이전 한 번 해 보기

```bash
charlie plan submit -f plan.json                  # agent 또는 operator
charlie plan approve pln_xxx --comment "확인함"    # operator
charlie migrate start pln_xxx --canary 10,50 --watch
# 다른 터미널에서, awaiting_switch가 되면
charlie migrate switch <run id>
```

`watch`는 run이 `awaiting_switch`에 머무는 동안 계속 따라가고, 연결이 끊기면 마지막으로 받은 이벤트 번호부터 다시 받습니다.

## 종료 코드

| 코드 | 뜻 |
| --- | --- |
| 0 | 성공 |
| 1 | 오류 (API 오류 코드와 메시지를 출력) |
| 2 | 사용법 오류 |
| 3 | 지켜보던 run이 `succeeded`가 아닌 상태로 끝남 |
| 130 | Ctrl-C로 중단 |

스크립트에서는 `charlie migrate start ... --watch --auto-switch; echo $?`로 이전 성공 여부를 판단할 수 있습니다.
