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
- 백그라운드 기록은 설정 디렉터리(아래 "설정 디렉터리와 환경 변수")의 `update.log`에 남습니다. 기본은 `%AppData%\charlie\update.log`(Windows) 또는 `~/.config/charlie/update.log`입니다.
- `go` 명령이 없거나 `go build`로 직접 만든 개발 빌드는 업데이트하지 않습니다.

## 로그인

```bash
charlie login     # 코드를 보여 주고, 관리자가 콘솔에서 승인하면 토큰을 저장한다
charlie whoami
charlie logout
```

`charlie login`은 device code 방식(RFC 8628을 따른다)으로 로그인합니다.

1. charlie가 서버에서 코드를 받아 `BCDF-GHJK` 같은 8글자 코드와 콘솔 주소(`https://console.hopcharlie.dev/#device`)를 보여 줍니다. 코드는 10분 동안 유효합니다.
2. 웹 콘솔에 관리자 아이디와 비밀번호로 로그인한 사람이 그 페이지에 코드를 입력하고, 이 CLI가 받을 역할(operator, agent, viewer)을 골라 승인합니다.
3. charlie는 서버가 정한 간격(5초)으로 승인 여부를 묻다가, 승인되면 30일 동안 유효한 토큰을 받아 저장합니다. 관리자가 거절하거나 코드가 만료되면 오류로 끝납니다.

- 서버는 `--server`, `CHARLIE_SERVER`, 저장된 로그인 순서로 정하고, 아무것도 없으면 팀 배포인 `https://api.hopcharlie.dev`를 씁니다.
- 토큰에는 승인한 관리자의 이름과 그 관리자가 고른 역할이 들어갑니다. 역할에 따라 쓸 수 있는 명령이 다릅니다. viewer는 읽기만 하고, agent는 Plan 제출까지 하며, 승인이나 실행은 operator만 할 수 있습니다.
- 같은 서버에 저장된 토큰이 있는데 다시 로그인하면(다른 역할을 받으려는 경우 등) 새 토큰을 저장한 뒤 예전 토큰을 서버에서 끝냅니다. 끝내지 못하면 그 토큰이 언제까지 유효한지 알려 줍니다.
- 기다리는 동안 Ctrl-C로 멈춰도 코드는 남아 있어서, charlie가 알려 주는 명령을 다시 실행하면 같은 코드로 이어서 기다립니다. 그 서버가 그냥 `charlie login`이 고르는 서버(저장된 로그인의 서버, 없으면 기본 서버)와 다르면 `charlie login --server <주소>`로 알려 줍니다. 다른 서버로 로그인하면 남은 코드는 버리고 새 코드를 받습니다.
- 기다리는 동안 네트워크 오류나 서버 오류(5xx)가 나면 같은 간격으로 계속 다시 묻습니다.
- `charlie whoami`는 토큰의 이름과 역할, 저장된 로그인이 만료되는 시각을 보여 줍니다. 저장된 로그인이 만료되거나 취소되어 서버가 401로 답하면 `charlie login`을 다시 하라고 알려 줍니다.
- `charlie logout`은 먼저 서버에 토큰을 끝내 달라고 요청하고, 그다음 저장된 로그인과 남은 코드를 지웁니다. 서버에 닿지 못하면 파일만 지우고, 그 토큰은 만료 시각까지 서버에서 유효하다고 알려 줍니다. 예전 버전이 저장한 정적 토큰(만료 시각이 없다)은 서버가 끝낼 수 없으므로 파일만 지우고 그렇게 알려 줍니다.
- 저장된 로그인 파일이 깨져 있어도 `charlie login`은 경고를 출력하고 새로 로그인해서 파일을 바꿉니다.

### 봇과 원격 호스트에서 로그인하기

Slack 봇 Rin처럼 실행이 끝나면 프로세스가 사라지는 곳이나 원격 호스트에서는 `--no-wait`로 두 단계에 나눠 로그인합니다.

```bash
charlie login --no-wait   # 코드를 출력하고 승인을 기다리지 않고 끝난다
# 관리자가 콘솔에서 승인한 뒤
charlie login             # 남은 코드로 승인 여부를 확인하고 토큰을 저장한다
```

- 두 번째 `charlie login`은 다른 프로세스여도 됩니다. 같은 설정 디렉터리를 보면 됩니다(아래 `CHARLIE_CONFIG_DIR`).
- 남은 코드가 있으면 charlie는 새 코드를 받기 전에 서버에 한 번 물어봅니다. 로컬 시계로 만료 시각이 지났어도 묻는데, 관리자가 승인하면 서버가 토큰을 받아 갈 시간을 10분 늘려 주기 때문입니다. 서버가 만료라고 답하면 새 코드를 받아 보여 주고(`--no-wait`도 같다), 거절이라고 답하면 오류로 끝납니다.
- 그래서 이미 승인된 코드가 남아 있으면 `charlie login --no-wait`도 토큰을 저장하고 끝납니다.
- 서버는 승인된 코드의 토큰을 한 번만 내줍니다. 같은 설정 디렉터리의 다른 `charlie login`이 먼저 받아 갔으면, 나머지는 새 코드를 받지 않고 그 로그인을 알려 준 뒤 성공으로 끝납니다.
- 남은 코드를 확인할 때 서버가 아닌 곳의 응답(프록시의 403 페이지 같은 것)이 오면 남은 코드를 버리고 새 코드를 받습니다.
- `--json`을 붙이면 `--no-wait`는 객체 하나만 출력합니다. 보통은 `{"userCode", "verificationUri", "expiresAt", "server"}`이고, 남은 코드가 이미 승인되어 토큰을 저장했으면 `{"name", "role", "expiresAt"}`입니다. 기다리는 모드는 안내를 표준 오류로 내보내고, 로그인이 끝나면 `{"name", "role", "expiresAt"}`을 출력합니다. 어느 쪽도 device code와 토큰은 출력하지 않습니다.

### 설정 디렉터리와 환경 변수

- 저장된 로그인(`config.json`), 승인을 기다리는 코드(`pending-login.json`), 자동 업데이트 파일(`update.json`, `update.log`)은 `%AppData%\charlie`(Windows), `~/.config/charlie`(Linux), `~/Library/Application Support/charlie`(macOS)에 둡니다. Linux와 macOS에서는 로그인 파일의 권한이 0600, 디렉터리 권한이 0700입니다.
- `CHARLIE_CONFIG_DIR`을 주면 이 파일을 모두 그 디렉터리에 둡니다. 실행마다 컨테이너가 새로 뜨는 봇은 영구 볼륨의 디렉터리를 가리키게 합니다. 이미 있는 디렉터리는 공유 볼륨일 수 있으므로 권한을 바꾸지 않고, 로그인 파일만 0600으로 씁니다. 없는 디렉터리는 0700으로 만듭니다.
- `CHARLIE_SERVER`, `CHARLIE_TOKEN` 환경 변수가 저장된 값보다 우선하고, `--server` 플래그가 그보다 우선합니다.
- 로그인할 수 없는 프로그램과 CI는 서버의 `PLATFORM_TOKENS`에 등록된 정적 토큰을 `CHARLIE_TOKEN`으로 줍니다. 정적 토큰은 만료되지 않고 `logout`으로 끝낼 수도 없습니다.
- 저장된 토큰은 로그인한 서버에만 보냅니다. 다른 서버를 지정하면 `CHARLIE_TOKEN`을 주거나 그 서버로 다시 로그인해야 합니다.

## 명령

| 명령 | 설명 |
| --- | --- |
| `env list`, `env show NAME` | 환경과 클러스터 실시간 상태 (노드, 버전, CNPG, 쓰기를 받는 환경) |
| `apps` | 서버 앱 파일(`PLATFORM_APPS_FILE`)의 앱과 환경별 DB, 버킷. `plan new`가 쓰는 설정이다. GitHub에서 등록한 앱은 `app list`로 본다 |
| `plan new --app APP --from ENV --to ENV [--stream NAME] [--submit]` | 등록된 설정으로 엔진 Plan 초안을 만든다. `--submit`이면 바로 제출 |
| `plan submit -f FILE` | 엔진 Plan 제출 (`-f -`는 표준 입력) |
| `plan attach REF --name NAME -f FILE` | 승인 전 Plan에 파일(대상 CNPG 매니페스트 등)을 붙인다. Plan에서는 `attachment:NAME`으로 가리킨다. 올릴 때마다 Plan의 revision이 1 늘어난다 |
| `plan list`, `plan show REF` | Plan 목록과 상세 (상세에는 DB 비밀번호가 전송될 호스트, revision, 첨부 이름과 SHA-256도 표시) |
| `plan approve REF [--comment] [--revision N]`, `plan reject REF --comment` | 승인, 거절. 승인은 아래에 따로 설명 |
| `written-records STREAM` | 부하 생성기가 보낸 쓰기 성공 기록 수. Plan에서는 `stream:STREAM`으로 가리켜 검증에 쓴다 |
| `migrate start PLANREF [--watch]` | 이전 시작. 옵션은 아래에 따로 설명 |
| `migrate list`, `migrate status RUNID` | 실행 목록과 상세 |
| `migrate watch RUNID` | 단계, 다운타임, 복제 지연, 실패 요청 수, 누락 수를 실시간으로 표시. `--observe`로 시작한 run은 전환 뒤 observe 단계마다 게이트 판정과, 자동 롤백 run 번호도 표시 |
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
- `--canary 1,10,25,50,75,90,100 --canary-step 10s`: 전환 전에 읽기 요청을 대상으로 이 순서로 보내고 단계마다 게이트를 판정합니다. 콘솔 기본값과 같은 권장 단계입니다. 요청이 `--min-requests`보다 적은 단계는 `[..] inconclusive`로 표시되고 다음 단계로 넘어가며, 마지막 단계는 충분한 요청으로 통과해야 합니다.
- `--observe 2m --observe-step 20s`: 전환이 끝난 뒤에도 정한 시간 동안 대상을 게이트로 계속 판정하고 나서 run을 끝냅니다. 게이트를 어긴 단계가 나오면 관찰을 멈추고, run은 `succeeded`로 끝나되 위반 내용을 남깁니다. 게이트웨이 http 모드 Plan에서만 쓸 수 있습니다.
- `--auto-rollback`: `--observe`와 함께 쓰면, 게이트를 어겼을 때 서버가 바로 롤백 run을 시작합니다(행위자 `slo-guard`). 없으면 운영자가 `charlie migrate rollback`으로 정합니다.
- `--max-error-rate`, `--max-p95`, `--min-requests`: 게이트 기준을 바꿉니다. 카나리와 observe에 같은 기준이 쓰이고, `--canary`나 `--observe`가 있어야 합니다. 지정하지 않으면 서버 기본값을 씁니다.
- `--idempotency-key`: 같은 키로 다시 보내면 새 run을 만들지 않고 처음 run을 돌려받습니다. 지정하지 않으면 자동으로 만들어 출력합니다.

모든 명령에 `--json`을 붙이면 API 응답을 JSON으로 출력합니다. `migrate watch --json`은 메시지마다 `{"event", "id", "data"}` 한 줄을 출력합니다. 색은 `--no-color`나 `NO_COLOR`로 끕니다.

## 이전 한 번 해 보기

```bash
charlie plan submit -f plan.json                  # agent 또는 operator
charlie plan show pln_xxx                         # revision과 첨부 확인
charlie plan approve pln_xxx --revision 2 --comment "확인함"    # operator
charlie migrate start pln_xxx --canary 1,10,25,50,75,90,100 --watch
# 다른 터미널에서, awaiting_switch가 되면
charlie migrate switch <run id>
```

`watch`는 run이 `awaiting_switch`에 머무는 동안 계속 따라가고, 연결이 끊기면 마지막으로 받은 이벤트 번호부터 다시 받습니다.

## GitHub 레포로 앱 등록과 배포

앱 레포에는 서비스마다 Dockerfile을, 루트에 `platform.yaml`을 둡니다(형식은 platform 레포 `docs/adr/0011-github-app-deploy.md`). 플랫폼이 이미지를 빌드해 gitops 레포에 커밋하면 Argo CD가 환경마다 배포합니다.

| 명령 | 설명 |
| --- | --- |
| `app validate [-f platform.yaml]` | `platform.yaml`을 서버와 같은 규칙(`pkg/appspec`)으로 로컬에서 검사한다. 로그인이 필요 없다. `-f -`는 표준 입력 |
| `app validate --remote --repo OWNER/NAME [--branch B]` | 플랫폼이 GitHub App으로 레포 브랜치 head의 `platform.yaml`을 읽어 검사한다. 서비스마다 Dockerfile이 있는지도 본다 |
| `app repos` | 플랫폼의 GitHub App이 볼 수 있는 레포 |
| `app add --repo OWNER/NAME --env ENV [--env ENV...] [--branch main] [--no-auto-deploy] [--watch]` | 레포를 앱으로 등록하고 첫 배포를 큐에 넣는다 (operator). 앱 이름은 `platform.yaml`의 `name`이다. `--branch`는 앱의 기본 브랜치이고, 이 브랜치가 앱 이름의 기본 인스턴스가 된다. `--no-auto-deploy`가 없으면 브랜치에 push 할 때마다 다시 배포한다 |
| `app list` | GitHub에서 등록한 앱의 인스턴스 수, 레포, 기본 브랜치, 환경별 상태, 커밋, URL과, 앱 파일의 앱 |
| `app show NAME` | 앱 상세, `platform.yaml` 요약(서비스, 포트, 리소스), 환경별 Argo CD 상태, 인스턴스 목록, 최근 배포 5개 |
| `app rm NAME [--yes]` | 등록을 지운다 (operator). gitops 레포에서 `apps/NAME/`과 추가한 인스턴스마다 `apps/<instance>/`를 지우고, Argo CD가 모든 환경에서 모든 인스턴스의 워크로드와 DB, 버킷을 지우므로 데이터는 되돌릴 수 없다. `--yes`가 없으면 앱 이름을 입력해야 지운다 |
| `app branch add APP BRANCH [--no-auto-deploy] [--watch]` | 앱 레포의 다른 브랜치를 인스턴스 하나로 더 띄우고 첫 배포를 큐에 넣는다 (operator). 인스턴스 이름과 URL을 출력한다. `--no-auto-deploy`가 없으면 그 브랜치에 push 할 때마다 그 인스턴스에 다시 배포한다 |
| `app branch list APP` | 앱의 인스턴스 목록: 인스턴스 이름, 브랜치, 기본 인스턴스인지, 자동 배포, 환경별 상태, 커밋, URL |
| `app branch rm APP INSTANCE\|BRANCH [--yes]` | 추가한 인스턴스를 지운다 (operator). 인스턴스 이름이나 브랜치로 가리킨다. Argo CD가 그 인스턴스의 네임스페이스와 DB, 버킷을 지우므로 데이터는 되돌릴 수 없다. `--yes`가 없으면 인스턴스 이름을 입력해야 지운다. 기본 인스턴스는 `app rm`으로 앱과 함께만 지운다 |
| `deploy APP [--branch B] [--ref REF] [--env ENV...] [--watch]` | 지금 배포한다 (operator). `--branch`는 어느 인스턴스에 배포할지 브랜치로 고르고, 기본은 앱의 기본 브랜치다. `--ref`는 그 인스턴스에 배포할 브랜치, 태그나 커밋이고, 기본은 그 브랜치의 head다. `--env`가 없으면 앱의 모든 환경 |
| `deploy list APP [--branch B] [--limit N]` | 배포 목록 (최신순, 기본 20개). 모든 인스턴스의 배포를 브랜치와 함께 보여 주고, `--branch`면 그 브랜치의 인스턴스 배포만 |
| `deploy show ID` | 배포 상세: 상태, 브랜치와 인스턴스, 커밋, 트리거, 환경별 상태, 빌드한 이미지 |
| `deploy watch ID` | 단계, 환경별 상태, 빌드와 배포 로그, 경과 시간을 실시간으로 표시하고, 끝나면 배포한 인스턴스의 URL을 보여 준다 |
| `deploy logs ID [--follow]` | 빌드와 배포 로그. `--follow`면 배포가 끝날 때까지 계속 출력한다 |

- 앱 하나에 기본 브랜치 말고 다른 브랜치를 인스턴스로 더 띄울 수 있습니다(platform 레포 `docs/adr/0015-branch-instances.md`). 기본 인스턴스 이름은 앱 이름이고, 추가한 브랜치의 인스턴스는 `<app>--<브랜치 슬러그>`이며 URL은 `https://<instance>.hopcharlie.dev`입니다. 슬러그는 브랜치 이름을 소문자로 바꾸고, `a-z`, `0-9`가 아닌 글자가 이어진 곳을 `-` 하나로 바꾸고, 앞뒤의 `-`를 뗀 것입니다. 인스턴스 이름이 53자를 넘으면 슬러그를 잘라 끝에 `-<16진수 6자>`(브랜치 이름의 SHA-256 앞 6자)를 붙입니다. 인스턴스마다 네임스페이스, DB, 버킷이 따로 생기고, gitops 경로는 `apps/<instance>/<env>.yaml`입니다. 앱 하나에 인스턴스는 기본 인스턴스를 포함해 3개까지입니다. 이미 추적하는 브랜치는 409 `branch_exists`, 슬러그가 같은 브랜치는 409 `instance_exists`, 3개를 넘기면 409 `instance_limit`로 끝납니다.
- 추가한 인스턴스는 지금 eks 환경에서만 돌 수 있습니다. GKE의 Argo CD가 `charlie-apps` 네임스페이스만 관리하기 때문입니다. 브랜치를 추가할 앱은 eks 환경으로 등록합니다(`app add --env eks`).
- `app branch rm`의 인자가 한 인스턴스의 이름이면서 다른 인스턴스의 브랜치이면 아무것도 지우지 않고, 각 인스턴스를 헷갈리지 않게 가리키는 방법을 알려 줍니다. `--ref`에 추가한 인스턴스의 브랜치를 주고 `--branch`를 주지 않으면 기본 인스턴스에 배포하므로, `deploy`가 표준 오류에 그 사실을 알려 줍니다.
- `platform.yaml`이 규칙을 어기면 `app add`, `app branch add`, `deploy`는 422로 끝나고, 어긴 규칙을 경로와 함께 한 줄씩 표준 오류로 출력합니다. `app validate`는 같은 형식으로 표준 출력에 쓰고, 유효하면 0, 아니면 1로 끝납니다.

  ```
  error: invalid_app_spec: platform.yaml breaks 2 rules (HTTP 422)
    services[0].port: must be between 1 and 65535
    services[1].public: only one service may be public; services[0] is
  ```

- `--watch`와 `deploy watch`는 배포가 `healthy`로 끝나면 0, `failed`나 `superseded`(같은 인스턴스의 같은 환경에 더 새 배포가 생김)로 끝나면 3, Ctrl-C로 멈추면 130으로 끝납니다. 연결이 끊기면 마지막으로 받은 이벤트 번호부터 다시 받습니다. 터미널이 아니면 이벤트마다 한 줄을 쓰고 끝에 전체 화면을 한 번 출력합니다.
- `environment` 이벤트에는 환경의 새 상태가 없어서, 터미널 화면은 그 이벤트를 받을 때마다 배포를 다시 읽어 환경별 상태를 고칩니다.
- `deploy logs`는 끝나지 않은 배포에서는 지금까지 저장된 로그만 출력하고 끝납니다. 첫 snapshot 뒤 0.5초 동안 새 이벤트가 없거나 5초가 지나면 멈추고, `--follow`로 이어 보라고 표준 오류에 알려 줍니다. 로그만 읽을 때는 배포가 실패했어도 0으로 끝나고, `--follow`는 `deploy watch`와 같은 종료 코드를 씁니다.
- `--json`이면 `app add`, `app branch add`, `deploy`는 API 응답을, `deploy watch`와 `--watch`는 메시지마다 `{"event", "id", "data"}` 한 줄을, `deploy logs`는 `log` 이벤트만 같은 형식으로 출력합니다. `app show --json`은 `{"app", "deployments"}`, `app branch list --json`은 인스턴스 배열(`deploy.instances`), `app validate --json`은 `POST /v1/apps/validate`의 응답 형식(`valid`, `spec`, `errors`)입니다. `app rm`, `app branch rm`은 응답 본문이 없어 아무것도 출력하지 않습니다.

```bash
charlie app validate                                   # 레포 루트에서 platform.yaml 검사
charlie app repos                                      # GitHub App이 보는 레포 확인
charlie app add --repo 2026-Softbank-Hackathon-Charlie/moimroom --env gke --env eks --watch
charlie app show moimroom
git push                                               # 이후 push마다 자동 배포
charlie deploy moimroom --ref 9fceb02 --env eks --watch    # 특정 커밋을 한 환경에만
charlie deploy logs dep_xxx --follow
charlie app branch add moimroom dev --watch            # dev 브랜치를 moimroom--dev 인스턴스로 띄운다
charlie deploy moimroom --branch dev --ref 9fceb02     # moimroom--dev에 특정 커밋을 배포
charlie app branch rm moimroom dev
```

## 종료 코드

| 코드 | 뜻 |
| --- | --- |
| 0 | 성공 |
| 1 | 오류 (API 오류 코드와 메시지를 출력), `app validate`에서 `platform.yaml`이 규칙을 어김 |
| 2 | 사용법 오류 |
| 3 | 지켜보던 run이 `succeeded`가 아닌 상태로, 지켜보던 배포가 `healthy`가 아닌 상태로 끝남 |
| 130 | Ctrl-C로 중단 |

스크립트에서는 `charlie migrate start ... --watch --auto-switch; echo $?`로 이전 성공 여부를 판단할 수 있습니다. observe에서 게이트를 어겨도 데이터는 옮겨졌으므로 run은 `succeeded`이고 종료 코드는 0입니다. 위반 여부는 화면이나 `--json`의 `observe.passed`로 확인합니다.

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
