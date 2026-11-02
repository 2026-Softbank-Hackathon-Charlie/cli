# charlie

HopCharlie 플랫폼 API를 터미널에서 쓰는 CLI입니다. 웹 콘솔과 같은 API를 부르고, 응답 타입은 platform 레포의 `pkg/apiv1`(OpenAPI 명세에서 생성)을 그대로 씁니다.

## 설치

platform 레포가 private이라 한 번 설정이 필요합니다.

```bash
go env -w GOPRIVATE=github.com/2026-Softbank-Hackathon-Charlie
go install github.com/2026-Softbank-Hackathon-Charlie/cli/cmd/charlie@latest
```

`go install`로 한 번 설치하면 그다음부터는 스스로 업데이트합니다(아래 "자동 업데이트"). 직접 빌드할 때는 다음 명령을 씁니다.

```bash
go build -o bin/charlie -ldflags "-X main.version=v0.1.0" ./cmd/charlie
```

## 자동 업데이트

`go install`로 설치한 charlie는 스스로 최신 버전을 유지합니다.

- 명령이 끝나면 백그라운드에서 새 버전이 있는지 확인합니다. 확인은 최대 10분에 한 번이고, 명령 출력을 기다리게 하지 않습니다.
- 새 버전이 있으면 `go install`로 빌드해서 실행 파일을 바꿉니다. 바뀌면 다음 명령을 실행할 때 한 줄로 알려 줍니다.
- 지금 바로 확인하려면 `charlie update`를 실행합니다.
- `CHARLIE_UPDATE_INTERVAL`로 확인 주기를 바꿉니다(예: `1h`, `0`이면 매번). `CHARLIE_NO_UPDATE=1`이면 자동 확인을 끕니다.
- 백그라운드 기록은 `%AppData%\charlie\update.log`(Windows) 또는 `~/.config/charlie/update.log`에 남습니다.
- `go` 명령이 없거나 `go build`로 직접 만든 개발 빌드는 업데이트하지 않습니다.

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
| `apps` | 플랫폼에 등록된 앱과, 앱이 있는 환경 |
| `plan new --app APP --from ENV --to ENV [--stream NAME] [--submit]` | 등록된 설정으로 엔진 Plan 초안을 만든다. `--submit`이면 바로 제출 |
| `plan submit -f FILE` | 엔진 Plan 제출 (`-f -`는 표준 입력) |
| `plan attach REF --name NAME -f FILE` | 승인 전 Plan에 파일(대상 CNPG 매니페스트 등)을 붙인다. Plan에서는 `attachment:NAME`으로 가리킨다. 올릴 때마다 Plan의 revision이 1 늘어난다 |
| `plan list`, `plan show REF` | Plan 목록과 상세 (상세에는 DB 비밀번호가 전송될 호스트, revision, 첨부 이름과 SHA-256도 표시) |
| `plan approve REF [--comment] [--revision N]`, `plan reject REF --comment` | 승인, 거절. 승인은 아래에 따로 설명 |
| `written-records STREAM` | 부하 생성기가 보낸 쓰기 성공 기록 수. Plan에서는 `stream:STREAM`으로 가리켜 검증에 쓴다 |
| `migrate start PLANREF [--watch]` | 이전 시작. 옵션은 아래에 따로 설명 |
| `migrate list`, `migrate status RUNID` | 실행 목록과 상세 |
| `migrate watch RUNID` | 단계, 다운타임, 복제 지연, 실패 요청 수, 누락 수를 실시간으로 표시 |
| `migrate switch RUNID [--watch]` | `awaiting_switch` 상태인 run의 전환 승인 |
| `migrate abort RUNID [--reason]` | 중단 |
| `migrate resume RUNID [--watch]` | interrupted, failed, aborted run 재개 |
| `migrate rollback RUNID [--reason] [--watch]` | 성공한 이전을 반대 방향으로 되돌린다 (operator). run 하나는 한 번만 되돌리고, 앞선 롤백의 시작이 실패했으면 그 Plan을 다시 시작한다 |
| `traffic [--watch] [--interval 1s]` | 게이트웨이 상태와 환경별 요청 통계 |
| `matrix` | 6개 경로 결과표 |
| `audit [--limit N] [--before ID]` | 감사 로그 |
| `version` | 버전 |

`plan approve`는 Plan을 먼저 읽어 승인할 revision과 첨부(이름, 크기, SHA-256)를 출력하고, 그 revision으로 승인합니다. 그 사이에 첨부가 다시 올라오면 서버가 409로 거부하므로, 출력된 내용과 다른 파일이 승인되는 일은 없습니다. `plan show`로 미리 검토했다면 `--revision N`으로 그 revision을 지정합니다. 그 뒤 Plan이 바뀌었으면 요청을 보내지 않고 `changed since you reviewed it (revision N, now M)`으로 끝납니다. `--json`이면 이 안내는 표준 오류로 나갑니다.

`migrate start` 옵션은 다음과 같습니다.

- `--auto-switch`: 데이터가 따라잡히면 승인을 기다리지 않고 바로 전환합니다.
- `--canary 10,50 --canary-step 20s`: 전환 전에 읽기 요청을 대상으로 10%, 50% 순서로 보내고 게이트를 판정합니다.
- `--max-error-rate`, `--max-p95`, `--min-requests`: 게이트 기준을 바꿉니다. 지정하지 않으면 서버 기본값을 씁니다.
- `--idempotency-key`: 같은 키로 다시 보내면 새 run을 만들지 않고 처음 run을 돌려받습니다. 지정하지 않으면 자동으로 만들어 출력합니다.

모든 명령에 `--json`을 붙이면 API 응답을 JSON으로 출력합니다. `migrate watch --json`은 메시지마다 `{"event", "id", "data"}` 한 줄을 출력합니다. 색은 `--no-color`나 `NO_COLOR`로 끕니다.

## 이전 한 번 해 보기

```bash
charlie plan submit -f plan.json                  # agent 또는 operator
charlie plan show pln_xxx                         # revision과 첨부 확인
charlie plan approve pln_xxx --revision 2 --comment "확인함"    # operator
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

## 보안 뷰와 에이전트 기록

| 명령 | 설명 |
| --- | --- |
| `security [--environment E] [--severity S] [--refresh]` | 여러 환경의 보안 항목을 환경별로 묶어 심각한 것부터 보여 주고, 소스마다 읽었는지와 이유를 표로 보여 줌 |
| `agent list [--plan REF] [--run ID] [--limit N]` | AI 에이전트 세션 목록 (최신순, 이벤트 수, 도구 호출 수, 토큰, 소요 시간) |
| `agent show ID` | 세션 하나와 기록된 이벤트 전부. 서버에서 2000개씩 나눠 받음 |
| `agent watch ID` | 도구 호출과 결과를 실시간으로 한 줄씩 표시. 연결이 끊기면 마지막으로 받은 이벤트 번호부터 다시 받음 |

- `security`의 소스 상태는 `ok`, `unavailable`(응답 없음), `not_configured`(설정이나 자격 증명 없음), `error`(권한 부족 등)입니다. 서버는 결과를 60초 동안 캐시하고, `--refresh`는 다시 읽게 합니다(10초에 한 번까지).
- `--severity high`는 critical과 high만 보여 줍니다. `--environment`에는 `gke` 같은 환경 이름이나 `aws-account`, `platform`을 씁니다.
- `agent watch`는 세션이 `succeeded`로 끝나면 0, `failed`나 `cancelled`로 끝나면 3, Ctrl-C로 멈추면 130으로 끝납니다. 에이전트가 이벤트 없이 30분(서버의 `PLATFORM_AGENT_IDLE`) 동안 멈추면 플랫폼이 세션을 `cancelled`로 닫으므로, `watch`도 그 이유(`no activity for 30m; closed by the platform`)를 보여 주고 3으로 끝납니다.
- `--json`을 붙이면 API 응답을 그대로 출력합니다. `agent show --json`은 `{"session", "events"}`, `agent watch --json`은 메시지마다 `{"event", "id", "data"}` 한 줄입니다.
