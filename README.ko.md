# SvnGo

[English](README.md) | **한국어**

Go로 작성한 경량 Subversion(SVN) 서버 및 웹 관리 시스템입니다.
**Apache httpd + mod_dav_svn**이 SVN HTTP(S) 프로토콜을 처리하고, **Go 관리 서버**가 저장소·사용자·그룹·권한을 관리합니다. 모든 구성 요소가 Docker 컨테이너 하나에 들어 있습니다.

- PHP / Node / Java / Redis / ORM / 프론트엔드 프레임워크를 사용하지 않습니다.
- 이미지 약 36MB, 유휴 메모리는 컨테이너 전체 약 12MB 수준입니다(Apache 포함).
- amd64 / arm64 멀티 아키텍처 빌드를 지원합니다.
- 빌드된 이미지는 Docker Hub [isul/svngo](https://hub.docker.com/r/isul/svngo)에서 받을 수 있습니다.

## 구조

```
                       ┌──────────────────── SvnGo 컨테이너 (non-root, uid 1000) ─────────────────────┐
 svn 클라이언트 ─HTTP(S)─▶ Apache httpd :8080 / :8443                                                  │
 웹 브라우저            │   ├─ /svn/<저장소>  mod_dav_svn + mod_authz_svn + mod_auth_basic/authn_file   │
                       │   ├─ /<위치>/<저장소>  동일, SVN 위치마다 <Location> 자동 생성                 │
                       │   │                  (htpasswd: bcrypt, authz: 경로별 권한)                     │
                       │   └─ /admin         reverse proxy ──▶ Go 관리 서버 127.0.0.1:9000           │
                       │                                         ├─ SQLite (WAL) : 단일 진실 원천     │
                       │                                         ├─ htpasswd / authz 생성·검증·원자적 교체│
                       │                                         ├─ svnadmin / svnlook / svn (file://) │
                       │                                         └─ Apache 프로세스 감독(재시작/종료)  │
                       └──────────────────────────────────────────────────────────────────────────────┘
 볼륨: /data/repos  /data/config(htpasswd, authz, tls/, locales/)  /data/db  /data/backups
```

- **SQLite가 단일 진실 원천**입니다. 사용자/그룹/권한이 바뀔 때마다 DB 트랜잭션 안에서 `htpasswd`와 `authz`를 다시 생성합니다. 생성한 파일은 libsvn(`svngo-authz`)으로 검증한 뒤 임시 파일 → fsync → rename으로 원자적으로 교체하고, 다시 읽어서 비교합니다. 이 과정이 모두 성공해야 DB를 커밋하며, 실패하면 이전 파일로 되돌립니다.
- Apache는 요청마다 `htpasswd`/`authz` 파일을 읽으므로 저장소·사용자·권한 변경에 **Apache 재시작이 필요 없습니다**.
- 웹 관리자 계정(DB)과 SVN 사용자 계정(htpasswd)은 분리되어 있습니다. SVN 사용자도 웹에 로그인할 수 있지만, **자신에게 SVN 권한이 있는 저장소와 경로만** 조회할 수 있습니다.

## 주요 기능

| 영역 | 기능 |
|---|---|
| 대시보드 | 저장소/사용자/그룹/권한 수, Apache 상태, 디스크 사용량, 최근 작업과 변경 이력 |
| 저장소 | 생성(trunk/branches/tags 선택), 목록·검색·페이징, 이름 변경, SVN 위치 간 이동, 삭제(이름 재입력 확인), 활성/비활성, UUID·리비전·크기(캐시), 검증(`svnadmin verify`), 백업(dump.gz)·복원 |
| 웹 브라우징 | 디렉터리/파일 보기, 리비전 지정 보기, 원본 다운로드(스트리밍), 커밋 로그(페이징), 변경 파일, diff, 작성자·메시지. 모든 화면에 SVN 권한이 적용됩니다. |
| 사용자 | 생성, 수정, 삭제, 비밀번호 변경, 임시 비밀번호 발급, 활성/비활성, 검색, htpasswd 가져오기(bcrypt/APR1) |
| 그룹 | 생성, 수정, 삭제, 멤버 추가·제거 |
| 권한 | 저장소·디렉터리 단위 읽기/읽기쓰기/접근 금지. 대상은 사용자, 그룹, 인증된 모든 사용자(`$authenticated`), 모든 사용자(`*`). 저장 후 libsvn으로 실제 적용 결과를 검증하며, 권한 확인 도구로 즉시 확인할 수 있습니다. |
| 시스템 설정 | SVN 기본 URL, SVN 위치(`/android/<저장소>` 같은 추가 URL 경로), 기본 레이아웃, 페이지 크기, 로그인 시도 제한, 관리 UI·SVN 접근 허용 IP, 관리자 계정 관리, 인증 파일 동기화 검증·재생성, 저장소 디렉터리 스캔(등록) |
| 백업 | 백업 목록·다운로드·삭제·업로드, 새 저장소로 복원 또는 덮어쓰기 복원 |
| 로그 | 감사 로그(누가 무엇을 바꿨는지), 애플리케이션 로그, Apache 로그 |
| 다국어 | 영어·한국어 기본 제공, 브라우저 언어 자동 선택, 내 계정에서 언어 고정, 시스템 기본 언어, 언어 파일(JSON) 추가·업로드·다운로드 |

## 화면 미리보기

샘플 데이터(저장소 5개, 사용자 7명, 그룹 4개)로 촬영한 웹 관리 화면입니다.

**대시보드**: 저장소·사용자·그룹·권한 수, Apache 상태, 디스크 사용량, 최근 작업과 변경 이력

![대시보드](docs/images/02-dashboard.png)

**저장소 개요**: 체크아웃 URL, UUID·리비전·크기, 최근 커밋

![저장소 개요](docs/images/04-repo-overview.png)

**커밋 상세와 diff**: 작성자·메시지, 변경 파일, 파일별 diff

![커밋 상세와 diff](docs/images/08-repo-commit-diff.png)

**저장소 권한**: 경로별 권한 규칙과 권한 확인 도구(SvnGo 평가와 libsvn 실제 결과를 함께 표시)

![저장소 권한](docs/images/09-repo-perms.png)

<details>
<summary>화면 더 보기 (로그인, 저장소 목록, 파일 브라우저, 사용자·그룹, 시스템 설정, 백업, 로그, 다크 모드)</summary>

**로그인**

![로그인](docs/images/01-login.png)

**저장소 목록**

![저장소 목록](docs/images/03-repos.png)

**파일 브라우저**

![파일 브라우저](docs/images/05-repo-files.png)

**파일 보기**

![파일 보기](docs/images/06-repo-file.png)

**커밋 로그**

![커밋 로그](docs/images/07-repo-commits.png)

**전체 권한 규칙**

![전체 권한 규칙](docs/images/10-perms.png)

**사용자 목록**

![사용자 목록](docs/images/11-users.png)

**사용자 수정**: 정보 수정, 비밀번호 변경·임시 비밀번호 발급, 그룹, 유효 권한

![사용자 수정](docs/images/12-user-edit.png)

**그룹 목록**

![그룹 목록](docs/images/13-groups.png)

**그룹 수정**: 멤버 추가·제외, 그룹 권한

![그룹 수정](docs/images/14-group-edit.png)

**시스템 설정**: 서버 상태, 인증 파일 동기화 검증, SVN 접속 차단 목록, 서버 설정, 웹 관리자 계정

![시스템 설정](docs/images/15-system.png)

**백업 / 복원**

![백업 / 복원](docs/images/16-backups.png)

**감사 로그**

![감사 로그](docs/images/17-logs.png)

**다크 모드**: 운영체제 설정(`prefers-color-scheme`)을 따릅니다.

![다크 모드 대시보드](docs/images/18-dashboard-dark.png)

**SVN 사용자로 로그인한 화면**: 권한이 있는 저장소만 보이고 관리 메뉴는 표시되지 않습니다(QA 그룹 사용자 `jung.hoon`).

![SVN 사용자 화면](docs/images/19-svn-user-repos.png)

</details>

## Docker Hub 이미지

소스를 빌드하지 않고 Docker Hub에 올라간 이미지를 바로 쓸 수 있습니다.

- 저장소: [hub.docker.com/r/isul/svngo](https://hub.docker.com/r/isul/svngo)
- 이미지 이름: `isul/svngo`

| 태그 | 플랫폼 | 설명 |
|---|---|---|
| `1.1.1` | linux/amd64 | 버전 고정. 운영 환경에 권장 |
| `latest` | linux/amd64 | 가장 최근 릴리스 |

- 구성: Alpine 3.22, Apache 2.4, Subversion 1.14, Go 1.26
- ARM CPU 호스트(일부 시놀로지 모델, Raspberry Pi 등)에서는 [멀티 아키텍처 빌드](#멀티-아키텍처-빌드)로 arm64 이미지를 직접 만들어야 합니다.

### docker run으로 실행 (Windows 11 PowerShell)

```powershell
docker run -d --name svngo --restart unless-stopped `
  -p 80:8080 -p 443:8443 `
  -e ADMIN_PASSWORD="Change-Me-123" `
  -v svn-repos:/data/repos -v svn-config:/data/config `
  -v svn-db:/data/db -v svn-backups:/data/backups `
  --read-only `
  --tmpfs "/run/svngo:size=16m,mode=0750,uid=1000,gid=1000" `
  --tmpfs "/tmp:size=64m,mode=1777" `
  --cap-drop ALL `
  --security-opt "no-new-privileges:true" `
  isul/svngo:1.1.1
```

PowerShell은 따옴표 없는 쉼표를 배열 구분자로 해석하므로 `--tmpfs` 값은 따옴표로 감쌉니다. 각 옵션의 의미는 [컨테이너 보안 옵션](#컨테이너-보안-옵션)을 참고하세요.

### docker compose로 실행

이 저장소의 `docker-compose.yml`에서 `build:` 블록과 `image:` 줄을 다음처럼 바꾸면, 빌드하지 않고 Docker Hub 이미지를 사용합니다. 나머지 설정과 `.env`는 그대로 씁니다.

```yaml
services:
  svngo:
    image: isul/svngo:1.1.1
```

```powershell
docker compose pull
docker compose up -d
docker compose ps        # STATUS 가 healthy 이면 준비 완료
```

### 이미지 업데이트

데이터는 네 볼륨에 있으므로 컨테이너를 새 이미지로 다시 만들어도 유지됩니다. 업데이트 전에 볼륨을 백업해 두는 것을 권장합니다.

```powershell
# compose: 태그를 새 버전으로 바꾼 뒤
docker compose pull
docker compose up -d

# docker run: 새 이미지를 받고 기존 컨테이너를 지운 뒤
docker pull isul/svngo:<새버전>
docker stop svngo; docker rm svngo
# 위 docker run 명령을 새 태그로 다시 실행합니다(볼륨 이름은 그대로)
```

- 새 버전을 처음 시작할 때 DB 스키마가 자동으로 변환됩니다(예: 1.0.0 → 1.1.0). 따로 할 일은 없습니다.
- 변환 후 이전 이미지로 되돌리는 것은 지원하지 않습니다. 이전 버전은 새 데이터를 무시하므로(예: SVN 위치를 제공하지 않음), 꼭 되돌려야 하면 업데이트 전에 백업한 `svn-db`를 복원하세요.
- 버전별 변경 내용은 [버전 이력](#버전-이력)을 참고하세요.

## 빠른 시작 (Windows 11 PowerShell)

소스에서 이미지를 직접 빌드해 실행하는 방법입니다. 빌드 없이 쓰려면 [Docker Hub 이미지](#docker-hub-이미지)를 참고하세요.

사전 준비: [Docker Desktop](https://www.docker.com/products/docker-desktop/)을 설치하고 실행합니다.

```powershell
cd C:\work\svngo

# 1) 환경 파일 만들기 (관리자 비밀번호를 반드시 변경하세요)
Copy-Item .env.example .env
notepad .env

# 2) 빌드 및 실행
docker compose up -d --build

# 3) 상태 확인 (STATUS 가 healthy 가 되면 준비 완료)
docker compose ps
docker compose logs -f svngo
```

80번 포트를 이미 쓰고 있으면 `.env`에서 `SVNGO_HTTP_PUBLISH=8081`처럼 바꿉니다.

### 접속 URL

| 용도 | URL |
|---|---|
| 웹 관리 UI | `http://localhost/admin/` (`http://localhost/` 로 접속하면 자동 이동) |
| SVN 저장소 | `http://localhost/svn/<저장소명>` |
| [SVN 위치](#svn-위치-여러-url-경로)의 저장소 | `http://localhost/<위치>/<저장소명>` (예: `/android/app`) |
| 헬스체크 | `http://localhost/admin/healthz` |
| HTTPS (인증서 설정 시) | `https://<서버>/admin/`, `https://<서버>/svn/<저장소명>` |

### 초기 관리자 계정

- 컨테이너를 처음 시작할 때 관리자 계정이 없으면 `ADMIN_USERNAME` / `ADMIN_PASSWORD`로 생성합니다(기본 ID `admin`). 비밀번호는 8자 이상이어야 합니다.
- `ADMIN_PASSWORD`를 비워 두면 임의 비밀번호를 생성해 config 볼륨의 `initial-admin-password` 파일(권한 0600)에 저장합니다. 비밀번호는 로그에 출력하지 않습니다.

  ```powershell
  docker compose exec svngo cat /data/config/initial-admin-password
  # 로그인하고 비밀번호를 변경한 뒤 파일 삭제
  docker compose exec svngo rm /data/config/initial-admin-password
  ```

- 관리자 계정을 만든 뒤에는 `ADMIN_PASSWORD`를 바꿔도 적용되지 않습니다. 비밀번호를 잊었다면 다음 명령으로 재설정합니다(없는 ID면 새로 만듭니다).

  ```powershell
  "New-Admin-Pass-123" | docker compose exec -T svngo svngo admin-passwd admin
  ```

### SVN 사용 예

웹 UI에서 저장소와 사용자를 만들고 권한을 부여한 다음 사용합니다.

```powershell
svn checkout http://localhost/svn/myrepo/trunk myrepo --username kim
cd myrepo
"hello" | Out-File -Encoding utf8 readme.txt
svn add readme.txt
svn commit -m "첫 커밋"
svn update
```

TortoiseSVN에서는 저장소 URL에 `http://localhost/svn/myrepo`를 입력합니다. SVN 위치에 있는 저장소는 `http://localhost/android/app`처럼 위치 경로를 쓰며, 정확한 URL은 저장소 목록과 개요 화면에 표시됩니다.

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `ADMIN_USERNAME` | `admin` | 초기 관리자 ID |
| `ADMIN_PASSWORD` / `ADMIN_PASSWORD_FILE` | (없음) | 초기 관리자 비밀번호(8자 이상, 파일 지정 가능, Docker secret 용) |
| `SVNGO_HTTP_PUBLISH` / `SVNGO_HTTPS_PUBLISH` | `80` / `443` | 호스트에 공개할 포트(compose) |
| `SVNGO_SERVER_NAME` | `localhost` | Apache ServerName |
| `SVNGO_COOKIE_SECURE` | `auto` | 세션 쿠키 Secure 속성: `auto`(HTTPS 또는 `X-Forwarded-Proto: https`), `always`, `never` |
| `SVNGO_BCRYPT_COST` | `8` | SVN 사용자 bcrypt cost(5–14). Apache가 SVN 요청마다 검증하므로 너무 높이면 느려집니다. 웹 관리자는 12로 고정입니다. |
| `SVNGO_SESSION_HOURS` | `12` | 웹 세션 유효 시간 |
| `SVNGO_SVN_MAX_BODY_MB` | `1024` | SVN 요청 하나의 최대 본문 크기(MB, 0=무제한, 최대 2047). 커밋은 파일마다 PUT 요청을 하나씩 보내므로 사실상 파일 하나의 최대 크기입니다. 넘으면 413으로 거부합니다. |
| `SVNGO_SVN_MAX_XML_MB` | `64` | SVN XML 요청(REPORT/PROPFIND/MERGE)의 최대 크기(MB, 0=무제한) |
| `SVNGO_ADMIN_ALLOW_IPS` | (없음) | 관리 UI(`/admin`)에 항상 접근을 허용할 IP/CIDR(쉼표·공백 구분). 웹 UI에서 설정한 목록과 합쳐 적용합니다. [접근 IP 제한](#접근-ip-제한) 참고 |
| `SVNGO_SVN_ALLOW_IPS` | (없음) | SVN(`/svn`)에 항상 접근을 허용할 IP/CIDR. 형식이 틀리면 시작하지 않습니다. |
| `SVNGO_TRUSTED_PROXIES` | (없음) | `X-Forwarded-For`를 신뢰할 리버스 프록시의 IP/CIDR. 이 주소에서 온 요청은 헤더에 담긴 클라이언트 IP로 접근 제한, 로그인 차단, 로그를 처리합니다. [리버스 프록시 뒤의 클라이언트 IP](#리버스-프록시-뒤의-클라이언트-ip) 참고 |
| `SVNGO_SVNAUTHZ` | 자동 탐색 | authz 검증 도구 경로(`svngo-authz` 또는 `svnauthz`). 없거나 자체 점검에 실패하면 시작하지 않습니다. |
| `SVNGO_TLS_CERT` / `SVNGO_TLS_KEY` | `/data/config/tls/server.crt` / `.key` | 이 파일들이 있으면 8443 포트에서 HTTPS를 켭니다. |
| `TZ` | `Asia/Seoul` | 표시 시간대 |

## 컨테이너 보안 옵션

`docker-compose.yml`에는 다음 보안 옵션이 기본으로 들어 있습니다. 모두 빼도 기능은 똑같이 동작하지만, 그대로 두는 것을 권장합니다.

```yaml
    read_only: true
    tmpfs:
      - /run/svngo:size=16m,mode=0750,uid=1000,gid=1000
      - /tmp:size=64m,mode=1777
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
```

### `read_only: true` (읽기 전용 루트 파일시스템)

컨테이너의 루트 파일시스템(이미지 내용)을 읽기 전용으로 마운트합니다. `docker run`에서는 `--read-only`입니다.

- 쓸 수 있는 곳은 명시적으로 마운트한 볼륨과 tmpfs뿐이고, `/usr/local/bin`이나 `/etc` 같은 나머지 경로는 쓰기가 막힙니다.
- 취약점을 통해 컨테이너 안에서 코드가 실행되더라도 실행 파일이나 Apache 설정을 바꾸거나 파일을 심을 수 없습니다.
- 데이터가 이미지 계층이 아니라 볼륨에만 쌓이므로, 컨테이너를 다시 만들어도 상태가 섞이지 않습니다.

SvnGo가 쓰기를 하는 경로는 다음과 같습니다.

| 경로 | 종류 | 용도 |
|---|---|---|
| `/data/repos`, `/data/config`, `/data/db`, `/data/backups` | 볼륨 또는 바인드 마운트 | 저장소, htpasswd·authz, SQLite DB, 백업 (영구 보관) |
| `/run/svngo` | tmpfs (메모리) | Apache PID 파일과 SSL 세션 캐시, SVN 로그인 차단 목록, svn 클라이언트 설정, authz 자체 점검용 임시 파일 |
| `/tmp` | tmpfs (메모리) | SQLite와 라이브러리가 쓰는 임시 파일 |

- `read_only: true`를 쓸 때는 `tmpfs:` 두 항목이 반드시 함께 있어야 합니다. 없으면 Apache가 PID 파일을 만들지 못해 시작에 실패합니다.
- `/run/svngo`는 컨테이너 사용자(uid/gid 1000)가 쓸 수 있도록 `uid=1000,gid=1000`을 지정합니다. 이미지를 다른 UID로 빌드했다면 이 값도 맞춰 바꾸세요.
- tmpfs는 메모리에 있어 재시작하면 지워지지만, 여기에는 시작할 때 다시 만드는 데이터만 들어갑니다. SVN 로그인 차단 목록이 재시작 후 초기화되는 이유도 이 때문입니다.
- `read_only: false`로 바꿔도 기능은 같고, 보호 장치 하나가 빠질 뿐입니다. `directory ... is not writable` 오류는 이 옵션과 관계없는 볼륨 소유권 문제입니다([시놀로지 안내](#1-데이터-폴더-준비-필수) 참고).

### 그 밖의 옵션

| 옵션 | 설명 |
|---|---|
| `cap_drop: ALL` | 리눅스 커널 권한(capability)을 모두 제거합니다. 1024 미만 포트를 열지 않고 non-root로 실행하므로 필요한 권한이 없습니다. 대신 컨테이너 안에서 파일 소유자를 바꿀 수 없어, 바인드 마운트 폴더는 호스트에서 소유권을 맞춰야 합니다. |
| `no-new-privileges:true` | setuid 실행 파일 등으로 권한이 올라가는 것을 막습니다. 일부 Linux 호스트에서 문제가 생기면 [제한 사항](#제한-사항)을 참고하세요. |

## HTTPS 설정

**방법 1: 컨테이너에서 직접 처리**

PEM 형식의 인증서와 키를 config 볼륨의 `tls/` 아래에 컨테이너 사용자 소유로 저장한 뒤 재시작합니다.

```powershell
docker compose exec svngo mkdir -p /data/config/tls
Get-Content .\server.crt -Raw | docker compose exec -T svngo sh -c "cat > /data/config/tls/server.crt"
Get-Content .\server.key -Raw | docker compose exec -T svngo sh -c "umask 077; cat > /data/config/tls/server.key"
docker compose restart svngo
docker compose logs svngo | Select-String "tls=true"
```

또는 compose 파일에 바인드 마운트 `- ./tls:/data/config/tls:ro`를 추가해도 됩니다. 이때 키 파일은 컨테이너 사용자(uid 1000)가 읽을 수 있어야 합니다.

HTTPS가 켜지면 HSTS 헤더와 Secure 쿠키가 자동으로 적용됩니다.

**방법 2: 외부 리버스 프록시**

Nginx, Traefik, Caddy 같은 프록시에서 TLS를 종료하고 `http://<컨테이너>:8080`으로 전달합니다. 프록시가 `X-Forwarded-Proto: https`를 보내면 Secure 쿠키가 적용됩니다. 쿠키를 항상 Secure로 하려면 `SVNGO_COOKIE_SECURE=always`를 지정하세요. 실제 클라이언트 IP로 접근 제한과 로그인 차단을 하려면 [리버스 프록시 뒤의 클라이언트 IP](#리버스-프록시-뒤의-클라이언트-ip)도 설정하세요.

## 시놀로지(Synology) NAS에서 실행하기

DSM 7.2 이상의 **Container Manager** 기준입니다. 배포 이미지(`isul/svngo`)는 linux/amd64 전용이라 Intel/AMD CPU 모델에서 실행됩니다. ARM CPU 모델은 [멀티 아키텍처 빌드](#멀티-아키텍처-빌드)로 arm64 이미지를 직접 만들어 사용하세요.

### 1. 데이터 폴더 준비 (필수)

컨테이너는 uid/gid 1000으로 실행되고 `cap_drop: ALL`이라 컨테이너 안에서는 폴더 권한을 바꿀 수 없습니다. DSM 공유 폴더는 소유자가 DSM 사용자(uid 1026 이상)이고 ACL이 걸려 있으며, 없는 폴더를 마운트하면 Docker가 root 소유로 만듭니다. 그대로 마운트하면 다음 오류로 시작하지 않습니다.

```
error: directory /data/repos is not writable (check the volume ownership): open /data/repos/.svngo-write-test: permission denied
```

**제어판 → 터미널 및 SNMP**에서 SSH를 켜고 Windows PowerShell에서 NAS에 접속합니다.

```powershell
ssh <DSM관리자계정>@<NAS주소>
```

NAS 셸에서 네 폴더를 만들고 ACL을 제거한 뒤 소유자를 uid/gid 1000으로 바꿉니다. `BASE`는 환경에 맞게 바꾸세요. 이미 데이터가 있는 폴더에도 그대로 실행할 수 있습니다.

```bash
BASE=/volume1/docker/svngo
for d in repos config db backups; do
  sudo mkdir -p "$BASE/$d"
  sudo find "$BASE/$d" -exec /usr/syno/bin/synoacltool -del {} \; >/dev/null 2>&1
  sudo chown -R 1000:1000 "$BASE/$d"
  sudo chmod -R u+rwX,g+rX,o-rwx "$BASE/$d"
done
ls -ln "$BASE"   # 네 폴더 모두 소유자가 1000 1000 이면 정상
```

저장소를 다른 공유 폴더(예: `/volume2/DEV/svn`)에 두려면 그 폴더에도 같은 명령을 실행합니다. 공유 폴더 전체가 아니라 SvnGo 전용 하위 폴더에만 적용하세요.

### 2. 프로젝트 만들기

**Container Manager → 프로젝트 → 생성**에서 프로젝트 이름(`svngo`)과 경로(`/volume1/docker/svngo`)를 지정하고, 원본으로 **docker-compose.yml 만들기**를 선택해 다음 내용을 붙여 넣습니다. DSM이 80/443 포트를 사용하므로 다른 호스트 포트를 씁니다.

```yaml
services:
  svngo:
    image: isul/svngo:latest
    container_name: svngo
    restart: unless-stopped
    ports:
      - "36900:8080"
      - "36901:8443"
    environment:
      ADMIN_USERNAME: admin
      ADMIN_PASSWORD: Change-Me-123   # 8자 이상, 최초 실행 시에만 사용됩니다
      SVNGO_SERVER_NAME: nas.example.com
      TZ: Asia/Seoul
    volumes:
      - /volume1/docker/svngo/repos:/data/repos
      - /volume1/docker/svngo/config:/data/config
      - /volume1/docker/svngo/db:/data/db
      - /volume1/docker/svngo/backups:/data/backups
    read_only: true
    tmpfs:
      - /run/svngo:size=16m,mode=0750,uid=1000,gid=1000
      - /tmp:size=64m,mode=1777
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    stop_grace_period: 30s
```

DSM 방화벽을 쓰고 있다면 **제어판 → 보안 → 방화벽**에서 36900, 36901 포트를 허용합니다. 시작 후 다음 주소로 접속합니다.

| 용도 | URL |
|---|---|
| 웹 관리 UI | `http://<NAS주소>:36900/admin/` |
| SVN 저장소 | `http://<NAS주소>:36900/svn/<저장소명>` |
| SVN 위치의 저장소 | `http://<NAS주소>:36900/<위치>/<저장소명>` |

### 3. HTTPS (DSM 인증서 사용)

DSM에 등록된 인증서(Let's Encrypt 등)를 config 폴더의 `tls/`로 복사하면 컨테이너가 36901 포트에서 HTTPS를 켭니다. 아래는 DSM 기본 인증서를 복사하는 예입니다.

```bash
ARCHIVE=/usr/syno/etc/certificate/_archive
CERT="$ARCHIVE/$(sudo cat $ARCHIVE/DEFAULT)"
TLS=/volume1/docker/svngo/config/tls
sudo mkdir -p "$TLS"
sudo cp "$CERT/fullchain.pem" "$TLS/server.crt"
sudo cp "$CERT/privkey.pem" "$TLS/server.key"
sudo chown -R 1000:1000 "$TLS"
sudo chmod 700 "$TLS" && sudo chmod 644 "$TLS/server.crt" && sudo chmod 600 "$TLS/server.key"
sudo docker restart svngo
```

인증서가 갱신되면 다시 복사해야 하므로, 위 명령을 **제어판 → 작업 스케줄러 → 사용자 정의 스크립트**(사용자 `root`, 매주 실행)로 등록해 두면 편합니다. 작업 스케줄러에서는 `sudo` 없이 작성해도 됩니다.

SVN은 `https://<NAS주소>:36901/svn/<저장소명>`처럼 컨테이너 HTTPS 포트로 직접 접속하는 것을 권장합니다. DSM **역방향 프록시**로 HTTPS를 종료하고 `http://localhost:36900`으로 넘기면 웹 UI, checkout, commit은 동작합니다. 하지만 브랜치·태그 생성(`svn copy`)과 `svn move`는 요청 주소의 scheme(https)과 서버 scheme(http)이 달라 `502 Bad Gateway`(`Destination URI refers to different scheme or port`)로 실패할 수 있습니다.

### 4. 운영 팁

- Container Manager에서 만든 컨테이너도 SSH에서 `sudo docker` 명령으로 다룰 수 있습니다.

  ```bash
  sudo docker logs -f svngo
  echo 'New-Admin-Pass-123' | sudo docker exec -i svngo svngo admin-passwd admin
  ```

- 저장소나 [SVN 위치](#svn-위치-여러-url-경로) 폴더(예: `repos/android/`)를 `repos`에 복사한 뒤에는 1단계의 `chown -R 1000:1000` / `chmod` 명령을 다시 실행하고 **시스템 설정**에서 등록하세요.
- 저장소 폴더에 생기는 `#recycle`, `@eaDir` 같은 DSM 시스템 폴더는 저장소 스캔에서 자동으로 제외됩니다.
- Hyper Backup이나 스냅샷으로 백업할 때는 네 폴더(`repos`, `config`, `db`, `backups`)를 함께 포함하세요.
- 컨테이너 안의 모든 실행 파일이 `operation not permitted`로 실패하면 `security_opt` 항목을 제거하세요.

## 다국어

웹 관리 화면은 영어(`en`)와 한국어(`ko`)를 기본으로 제공합니다. 언어 파일(JSON)을 등록하면 재시작 없이 다른 언어를 추가하거나 기본 번역을 고칠 수 있습니다. 서버 로그, CLI 출력, 시작 오류는 언어 설정과 관계없이 영어로 출력합니다.

### 표시 언어 결정 순서

1. 로그인한 사용자가 **내 계정 → 언어**에서 고른 언어 (브라우저 언어와 관계없이 적용)
2. 로그아웃 상태에서는 마지막으로 고른 언어(쿠키 `svngo_lang`, 1년 유지)
3. 브라우저 언어 설정(`Accept-Language`)과 일치하는 언어. 우선순위(`q`) 순으로 찾고, `ko-KR`처럼 지역이 붙은 경우 `ko`로도 찾습니다. 반대로 브라우저가 `pt`이고 등록된 언어가 `pt-br`뿐이면 `pt-br`을 씁니다.
4. **시스템 설정 → 서버 설정 → 기본 언어** (초기값 영어)
5. 위에서 정하지 못하면 영어

웹 관리자와 SVN 사용자 모두 자신의 언어를 고를 수 있습니다. **자동 (브라우저 언어)**를 고르면 다시 브라우저 설정을 따릅니다. 사용자가 고른 언어나 기본 언어로 지정한 언어의 파일을 삭제하면, 그 설정은 없는 것으로 보고 다음 순서의 언어로 표시합니다.

### 언어 파일 형식

UTF-8로 저장한 평면 JSON입니다. `_meta`에 언어 코드와 이름을 적고, 나머지는 `"키": "번역문"` 쌍입니다.

```json
{
  "_meta": { "code": "ja", "name": "日本語" },
  "nav.repos": "リポジトリ",
  "pager.total": "合計 {0} 件"
}
```

| 항목 | 규칙 |
|---|---|
| `_meta.code` | 언어 코드. 소문자 2~3자, 필요하면 `-`와 지역 코드 2~8자를 붙입니다(예: `ja`, `fr`, `pt-br`, `zh-tw`). 대문자와 `_`는 자동으로 소문자와 `-`로 바뀝니다(`pt_BR` → `pt-br`). |
| `_meta.name` | 언어 선택 목록에 표시할 이름. 필수, 64자 이하. 그 언어로 적는 것을 권장합니다(`日本語`, `Français`). |
| 키 | 화면 영역별 접두어가 붙은 이름(`nav.*` 메뉴, `repo.*` 저장소, `user.*` 사용자, `system.*` 시스템 설정 등). 현재 578개이며, 기준 언어인 영어 파일에 있는 키만 화면에 쓰입니다. 모르는 키는 무시됩니다. |
| 값 | 문자열만 허용합니다(숫자, 배열, 객체 불가). 키는 200바이트, 값은 4,000바이트 이하입니다. |
| `{0}`, `{1}` … | 실행 중에 저장소 이름, 개수 등으로 바뀌는 자리입니다. 번호는 그대로 두고 어순에 맞게 위치만 옮기세요. 일부 자리에는 코드 강조나 링크가 들어갑니다. |

- 번역문은 HTML로 해석하지 않고 항상 이스케이프해서 표시합니다. `<b>` 같은 태그를 넣으면 글자 그대로 보입니다.
- 일부만 번역해도 됩니다. 번역하지 않은 키는 영어로 표시됩니다.
- **파일 이름**: 폴더에 직접 넣는 파일은 `<코드>.json` 이름이어야 하며, `_meta.code`가 있으면 파일 이름과 같아야 합니다. 업로드할 때는 `_meta.code`를 우선 쓰고, 없으면 파일 이름(`ja.json` → `ja`)을 코드로 씁니다. 업로드한 파일은 키를 정렬해 `<코드>.json`으로 저장합니다.

### 새 언어 파일 만들기

모든 키가 들어 있는 영어 언어 파일에서 시작합니다. 영어 파일은 두 가지 방법으로 받을 수 있습니다.

- **실행 중인 서버에서** (웹 관리자): **시스템 설정 → 언어** 목록에서 영어(`en`)의 **다운로드**를 누릅니다. 그 서버 버전과 키가 정확히 일치합니다.
- **관리자 권한이나 서버가 없을 때**: 이 저장소의 [`internal/i18n/locales/en.json`](internal/i18n/locales/en.json)을 받습니다. 같은 폴더의 한국어 파일 [`ko.json`](internal/i18n/locales/ko.json)도 참고용으로 쓸 수 있습니다.

  ```powershell
  Invoke-WebRequest https://raw.githubusercontent.com/isul/svngo/main/internal/i18n/locales/en.json -OutFile ja.json
  ```

  최신 소스 기준 파일입니다. 서버가 이보다 오래된 버전이면 서버가 모르는 키는 무시되고, 서버가 더 새 버전이면 파일에 없는 키는 영어로 표시됩니다. 어느 경우든 파일은 그대로 쓸 수 있으며, 정확히 맞추려면 서버에서 받은 파일을 쓰세요.

그다음 순서는 다음과 같습니다.

1. 위 방법으로 영어 파일을 받습니다.
2. 파일 이름을 새 언어 코드로 바꾸고(예: `ja.json`), `_meta`의 `code`와 `name`을 고칩니다. `_meta.code`를 `en`으로 둔 채 업로드하면 새 언어가 추가되지 않고 영어 문구를 덮어씁니다.
3. 값만 번역합니다. 키와 `{0}` 같은 자리 표시는 바꾸지 마세요.
4. **시스템 설정 → 언어**에서 파일을 업로드합니다(웹 관리자 권한 필요). 목록의 **번역률**과 **누락** 개수로 빠진 키를 확인할 수 있습니다.
5. **내 계정 → 언어**에서 새 언어를 고르거나, 브라우저 언어를 바꿔 확인합니다. 모든 사용자의 기본값으로 쓰려면 **서버 설정 → 기본 언어**에서 고릅니다.

기본 번역 일부만 바꾸고 싶다면 `ko.json`처럼 기본 언어와 같은 코드로, 바꿀 키만 넣은 파일을 올리면 됩니다. 파일에 없는 키는 기본 번역을 그대로 씁니다.

### 등록 방법

- **업로드**: **시스템 설정 → 언어**에서 `.json` 파일을 올리면 검증 후 바로 적용됩니다. 같은 코드의 파일이 있으면 교체합니다.
- **폴더**: config 볼륨의 `locales/` 폴더(`/data/config/locales/<코드>.json`)에 파일을 넣고 **다시 읽기**를 누릅니다. 서버를 시작할 때도 이 폴더를 읽습니다. 폴더는 서버가 시작할 때 자동으로 만듭니다.

Docker 볼륨에 PowerShell로 넣는 예:

```powershell
docker compose cp .\ja.json svngo:/data/config/locales/ja.json
```

시놀로지처럼 바인드 마운트를 쓰면 호스트의 `config/locales/` 폴더(예: `/volume1/docker/svngo/config/locales/`)에 파일을 넣습니다. 파일은 컨테이너 사용자(uid 1000)가 읽을 수 있어야 합니다.

### 언어 관리

**시스템 설정 → 언어** 목록에는 등록된 모든 언어가 나옵니다.

| 열 | 의미 |
|---|---|
| 코드 | 언어 코드. 기본 언어로 지정된 언어에는 **기본** 표시가 붙습니다. |
| 출처 | **기본 제공**(내장), **파일**(추가한 언어), **기본 제공 + 파일**(기본 번역을 파일로 덮어씀) |
| 번역률 | 영어 키 중 번역된 비율과 누락 개수 |

- **다운로드**: 현재 번역에 누락된 키를 영어로 채운 완전한 파일을 받습니다. 번역을 이어서 하거나 백업할 때 씁니다.
- **삭제**: 파일로 등록한 언어에만 있습니다. 추가한 언어는 목록에서 사라지고, 덮어쓰기 파일을 지우면 기본 번역으로 돌아갑니다. 기본 제공 언어 자체는 지울 수 없습니다.
- 업로드와 삭제는 감사 로그에 남습니다.

### 오류 확인

업로드한 파일에 문제가 있으면 등록하지 않고 화면에 이유를 표시합니다. 폴더에 넣은 파일에 문제가 있으면 그 파일만 건너뛰고, **시스템 설정 → 언어**에 경고로 표시하며 서버 로그에 `language file skipped`를 남깁니다. 나머지 언어는 정상적으로 쓸 수 있습니다.

| 오류 | 원인과 조치 |
|---|---|
| JSON 형식이 올바르지 않습니다 | 쉼표, 따옴표, 중괄호를 확인합니다. 파일은 UTF-8이어야 합니다. |
| _meta.code(언어 코드)가 필요합니다 | `_meta.code`도 없고 파일 이름에서 코드를 알 수 없습니다. `_meta.code`를 넣습니다. |
| 언어 코드 "…" 가 올바르지 않습니다 | 코드 규칙(`ja`, `pt-br` 형식)에 맞게 고칩니다. |
| _meta.code (…) 가 파일 이름 (…) 과 다릅니다 | 폴더에 넣은 파일의 이름과 `_meta.code`를 맞춥니다. |
| 파일 이름의 언어 코드가 올바르지 않습니다 | 폴더의 파일 이름을 `<코드>.json`으로 바꿉니다. |
| _meta.name(언어 이름)이 필요하며 64자 이하여야 합니다 | `_meta.name`을 넣습니다. |
| "…" 의 값은 문자열이어야 합니다 | 해당 키의 값을 따옴표로 감싼 문자열로 바꿉니다. |
| 언어 파일이 너무 큽니다 / 키가 너무 많습니다 / 언어가 너무 많습니다 | 아래 제한을 넘었습니다. |

### 제한과 업그레이드

- 파일 512KB, 키 5,000개, 언어 50개(기본 제공 언어 포함)까지 등록할 수 있습니다.
- 새 버전에서 화면 문구가 추가되면, 기존 언어 파일에 없는 키는 영어로 표시됩니다. 업그레이드 후 언어 목록의 번역률을 확인하고, 해당 언어를 다시 다운로드하면 새 키가 영어로 채워져 있으므로 그 부분만 번역해 다시 올리면 됩니다.
- 기본 제공 언어 파일은 소스의 `internal/i18n/locales/`에 있습니다. 키를 추가하면 `en.json`과 `ko.json`에 모두 넣어야 하며, 테스트가 두 파일의 키와 자리 표시 일치를 검사합니다.

## 접근 IP 제한

관리 UI와 SVN에 접근할 수 있는 클라이언트 IP를 따로 정할 수 있습니다.

| 목록 | 적용 범위 | 차단 위치 | 반영 시점 |
|---|---|---|---|
| 관리 UI | `/admin` 전체(웹 관리자와 SVN 사용자의 웹 로그인 모두) | Go 관리 서버, 403 안내 화면 | 저장 즉시 |
| SVN | `/svn`과 모든 [SVN 위치](#svn-위치-여러-url-경로)(`/admin`을 제외한 모든 경로, checkout, commit 등 svn 클라이언트) | Apache, **인증 전에** 403 | 저장 즉시(Apache graceful 재시작) |

**설정 방법**

- **웹 UI**: **시스템 설정 → 접근 제한 (허용 IP)**에서 목록별로 입력하고 저장합니다. 재시작이 필요 없습니다.
- **환경 변수**: `SVNGO_ADMIN_ALLOW_IPS`, `SVNGO_SVN_ALLOW_IPS`에 넣은 항목은 웹 UI 목록과 **합쳐서** 항상 허용됩니다. 웹 UI에서는 바꿀 수 없고 화면에 읽기 전용으로 표시됩니다. 실수로 잠기지 않도록 관리자 PC나 사내 대역을 넣어 두는 용도로 권장합니다. 바꾸려면 컨테이너를 다시 만들어야 합니다.

```powershell
# .env (docker compose)
SVNGO_ADMIN_ALLOW_IPS=192.168.10.0/24
SVNGO_SVN_ALLOW_IPS=192.168.0.0/16,10.8.0.0/24
```

**규칙**

- 항목은 IPv4/IPv6 주소 또는 CIDR 대역입니다(`192.168.0.10`, `192.168.0.0/24`, `2001:db8::/48`). 줄바꿈·쉼표·공백으로 구분하고 `#` 뒤는 주석입니다. 목록 하나에 최대 200개입니다.
- 환경 변수와 웹 UI 목록이 **모두 비어 있으면 제한하지 않습니다**(기본값).
- 루프백(`127.0.0.1`, `::1`)은 항상 허용합니다. 컨테이너 헬스체크와 `docker compose exec`로 실행하는 점검이 막히지 않게 하기 위해서입니다.
- 관리 UI 목록을 저장할 때 현재 접속한 IP가 목록에 없으면 저장을 거부합니다(스스로 잠기는 것 방지). 화면에 **현재 접속 IP**가 표시됩니다.
- SVN 목록은 Apache 설정 검사(`httpd -t`)를 통과해야 적용됩니다. 실패하면 이전 목록과 Apache 상태를 그대로 유지합니다. graceful 재시작이라 진행 중인 checkout/commit은 끊기지 않고 Apache PID도 바뀌지 않습니다.
- 변경 내역은 감사 로그(`접근 제한 변경`)에 남습니다.

**잠겼을 때 복구**

웹 UI에서 설정한 목록을 지웁니다(환경 변수 목록은 그대로 유지). 관리 UI는 즉시, SVN은 20초 안에 반영됩니다.

```powershell
docker compose exec svngo svngo access-reset admin   # 관리 UI 목록만
docker compose exec svngo svngo access-reset svn     # SVN 목록만
docker compose exec svngo svngo access-reset all     # 둘 다
```

**주의**: 클라이언트 IP는 Apache가 본 접속 주소입니다. Docker Desktop(Windows/macOS)이나 NAT, 리버스 프록시 뒤에서는 모든 클라이언트가 같은 주소(예: `172.17.0.1`, 프록시 IP)로 보일 수 있습니다. 화면의 **현재 접속 IP**가 실제 PC 주소와 다르면 IP 제한으로 클라이언트를 구분할 수 없습니다. 리버스 프록시 때문이라면 아래와 같이 `SVNGO_TRUSTED_PROXIES`를 설정하고, 그 밖의 NAT 때문이라면 호스트 방화벽이나 프록시에서 제한하세요.

### 리버스 프록시 뒤의 클라이언트 IP

Caddy, Nginx 같은 리버스 프록시 뒤에서는 Apache가 프록시의 주소만 봅니다. 프록시가 같은 호스트에서 공개 포트로 접속하면 Docker 브리지 게이트웨이 주소(예: `172.17.0.1`, `172.22.0.1`)로 보입니다. 이 상태에서는 모든 사용자가 같은 IP라서 접근 IP 제한이 의미가 없고, 한 사람이 SVN 로그인에 반복해서 실패하면 모두가 차단됩니다.

`SVNGO_TRUSTED_PROXIES`에 프록시 주소를 넣으면, 그 주소에서 온 요청은 `X-Forwarded-For`에 담긴 클라이언트 IP를 씁니다(Apache `mod_remoteip`). 접근 IP 제한(관리 UI, SVN), SVN 로그인 차단, 접근 로그, 화면의 **현재 접속 IP**가 모두 이 주소를 기준으로 합니다.

1. 설정하기 전에 **시스템 설정 → 접근 제한**의 **현재 접속 IP**를 확인합니다. 여기 표시된 주소가 프록시 주소입니다.
2. `.env`에 그 주소를 넣고 컨테이너를 다시 만듭니다.

   ```powershell
   # .env
   SVNGO_TRUSTED_PROXIES=172.22.0.1
   ```

   ```powershell
   docker compose up -d
   ```

3. **현재 접속 IP**가 PC 주소로 바뀌었는지 확인합니다. 같은 화면의 **신뢰하는 프록시**에 설정한 목록이 표시됩니다.

프록시는 `X-Forwarded-For`를 보내기만 하면 됩니다. Caddy의 `reverse_proxy`는 기본으로 보내므로 따로 설정할 것이 없습니다.

```
svn.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Nginx는 `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`와 `proxy_set_header X-Forwarded-Proto $scheme;`를 지정합니다.

**규칙**

- 헤더를 오른쪽부터 읽어 신뢰하는 프록시가 아닌 첫 주소를 클라이언트 IP로 씁니다. 클라이언트가 직접 넣은 값은 프록시가 덧붙인 실제 주소보다 앞에 있으므로 IP를 위조할 수 없습니다.
- 목록에 없는 주소에서 온 요청은 헤더를 무시하고 접속 주소를 그대로 씁니다. 기본값(빈 목록)은 이전 버전과 같습니다.
- `192.168.0.10` 같은 사설 주소도 클라이언트 IP로 인정합니다.
- `0.0.0.0/0`, `::/0`처럼 모든 주소를 신뢰하는 값은 누구나 IP를 위조할 수 있게 되므로 거부합니다(시작하지 않음).
- SvnGo가 관리하는 Apache에만 적용됩니다. 외부 Apache(`SVNGO_MANAGE_APACHE` 끔)를 쓰면 그 Apache에서 `mod_remoteip`를 직접 설정하세요.

**보안 주의**: 신뢰하는 주소에서 온 요청은 `X-Forwarded-For`로 클라이언트 IP를 정할 수 있습니다. 프록시가 아닌 곳에서 그 주소로 접속할 수 없게 하세요.

- 프록시가 같은 호스트에 있으면 포트를 루프백에만 공개하고(`SVNGO_HTTP_PUBLISH=127.0.0.1:8080`) 프록시는 `127.0.0.1:8080`으로 전달하는 것을 권장합니다. LAN에서 SvnGo 포트로 직접 접속할 수 없게 됩니다.
- 프록시를 SvnGo와 같은 Docker 네트워크에 두고 `http://svngo:8080`으로 전달하는 방법도 있습니다. 이때는 프록시 컨테이너의 고정 IP를 넣습니다.
- Docker 네트워크를 다시 만들면 게이트웨이 주소가 바뀔 수 있습니다. **현재 접속 IP**가 다시 게이트웨이 주소로 보이면 값을 고치세요.

## SVN 위치 (여러 URL 경로)

저장소는 기본적으로 `/svn/<저장소>`로 제공됩니다. **SVN 위치**를 추가하면 repos 볼륨 안의 같은 이름 폴더에 있는 저장소를 다른 URL 경로로 제공합니다. 여러 `<Location>` 블록을 쓰던 기존 서버의 URL을 그대로 유지할 수 있습니다.

| URL | 폴더 |
|---|---|
| `/svn/<저장소>` (기본) | `/data/repos/<저장소>` |
| `/android/<저장소>` | `/data/repos/android/<저장소>` |
| `/hw/<저장소>` | `/data/repos/hw/<저장소>` |

```
/data/repos/            ← 기본 위치 (/svn)
├── myrepo/             → /svn/myrepo
├── android/            ← 위치 "android"
│   ├── app/            → /android/app
│   └── launcher/       → /android/launcher
└── hw/                 ← 위치 "hw"
    └── board/          → /hw/board
```

**위치 관리** (**시스템 설정 → SVN 위치**)

- **추가**: 이름을 입력합니다. 폴더가 없으면 만들고, 이미 들어 있는 저장소는 함께 등록합니다. Apache 설정을 검사(`httpd -t`)한 뒤 graceful 재시작으로 적용하므로 진행 중인 checkout·commit이 끊기지 않고 Apache PID도 바뀌지 않습니다. 검사에 실패하면 아무것도 바뀌지 않습니다.
- repos 볼륨에 저장소가 들어 있지만 위치로 등록되지 않은 폴더가 있으면 **/<이름> 등록** 버튼이 표시됩니다.
- **삭제**: 위치에 저장소가 하나도 없을 때만 가능합니다. 폴더는 비어 있을 때만 함께 지웁니다.
- 위치 이름은 바꿀 수 없습니다. 이름 규칙은 저장소 이름과 같고 `admin`, `svn`은 사용할 수 없습니다.

**규칙**

- **저장소 이름은 모든 위치에서 고유합니다.** 권한(`authz`), 웹 UI(`/admin/repos/<이름>`), 백업은 저장소를 이름만으로 구분합니다. mod_authz_svn은 `[repo:/path]`를 폴더 이름으로 찾으므로 `authz`와 `htpasswd` 하나로 모든 위치를 처리합니다.
- `/svn` 저장소는 위치 이름과 같은 이름을 쓸 수 없고, 같은 이름의 `/svn` 저장소가 있으면 그 이름으로 위치를 추가할 수 없습니다.
- 다른 위치에 이미 등록된 저장소와 이름이 같은 폴더는 등록하지 않고, 시스템 설정과 동기화 검증에서 **이름 충돌**로 알려 줍니다.
- 저장소 **생성** 화면에서 위치를 선택할 수 있습니다. **이동**은 저장소 **설정** 화면의 **이름 변경 / 이동**에서 합니다. 기존 작업 복사본은 `svn relocate`가 필요합니다.
- 새 저장소로 **복원**할 때 위치를 선택합니다. 기존 저장소를 덮어쓰면 그 저장소의 위치를 유지합니다.
- SVN 접근 IP 제한과 SVN 무차별 대입 차단은 모든 SVN 경로(`/admin`을 제외한 전부)에 적용됩니다.
- **SVN 기본 URL**을 설정한 경우(예: `https://svn.example.com/svn`) 다른 위치의 URL은 끝의 `/svn`을 위치 이름으로 바꿔 표시합니다(`https://svn.example.com/android/...`).
- 위치 폴더는 `/data/repos`와 같은 볼륨에 두세요. 위치마다 작업 폴더(`.svngo-tmp`)를 따로 쓰므로 생성·복원은 어느 마운트에서도 되지만, 위치 간 이동은 이름 변경(rename)이라 마운트가 다르면 실패합니다.
- 외부 Apache를 쓰는 경우(`SVNGO_MANAGE_APACHE` 끔) 위치마다 `/svn`과 같은 지시어로 `<Location /<이름>>` 블록(`SVNParentPath /data/repos/<이름>`)을 직접 추가해야 합니다.

## 기존 SVN 서버에서 이전하기

1. **저장소**: 기존 저장소 디렉터리를 repos 볼륨에 복사합니다. 웹 UI의 **시스템 설정**에 등록되지 않은 저장소가 있다는 경고가 나오면 **등록**을 누릅니다. 서버를 시작할 때도 자동으로 스캔해 등록합니다.
   ```powershell
   docker compose cp .\old-repos\myrepo svngo:/data/repos/myrepo
   ```
   또는 `svnadmin dump` 파일을 **백업** 화면에서 업로드한 뒤 새 저장소로 복원합니다(.svndump / .svndump.gz).

   기존 서버가 여러 `<Location>` 블록을 쓰는 경우(예: `/android` → `SVNParentPath /var/svn/android`) 각 상위 폴더를 같은 이름의 폴더(`/data/repos/android/`)로 복사하고 **시스템 설정 → SVN 위치**에서 등록합니다(**/android 등록** 버튼도 가능). URL이 그대로이므로 작업 복사본은 호스트나 포트가 바뀔 때만 `svn relocate`가 필요합니다. 폴더 이름이 URL 경로와 다르면(예: `/delphi` → `SVNParentPath /var/svn/delphi/src`) 폴더 이름을 경로에 맞춰(`/data/repos/delphi/`) 복사하고, 저장소 이름이 모든 위치에서 고유한지 먼저 확인하세요.
2. **사용자**: **사용자 → htpasswd 가져오기**에 기존 htpasswd 내용을 붙여 넣거나 파일을 업로드합니다. bcrypt와 APR1-MD5(`$apr1$`) 해시를 지원합니다. APR1 사용자는 SVN에 그대로 접속할 수 있고, 웹에 한 번 로그인하면 bcrypt로 자동 전환됩니다. SHA1/crypt/평문 항목은 보안상 가져오지 않습니다.
3. **권한**: 웹 UI에서 그룹과 권한 규칙을 다시 설정합니다. `authz` 파일은 DB에서 자동 생성하므로 직접 수정하지 마세요. 직접 수정한 내용은 다음 변경 때 덮어씁니다.

## 백업

- 저장소 화면의 **백업**은 `svnadmin dump` 결과를 gzip으로 압축해 backups 볼륨에 저장합니다(`<저장소>-YYYYmmdd-HHMMSS-r<리비전>.svndump.gz`).
- 복원은 임시 저장소에 load한 뒤 원자적으로 교체하므로, 실패해도 기존 저장소가 손상되지 않습니다.
- 전체 백업은 네 개의 볼륨(`svn-repos`, `svn-config`, `svn-db`, `svn-backups`)을 함께 보관하면 됩니다.

## 보안 설계 요약

- **명령 실행**: 셸 없이 `exec.CommandContext`에 고정 인자로 실행합니다. 경로와 URL은 검증과 이스케이프를 거친 뒤 전달하며, 타임아웃과 출력 크기 제한을 둡니다.
- **입력 검증**: 저장소/사용자/그룹명은 화이트리스트 정규식으로 검사하고, 저장소 내 경로는 정규화하면서 `..`과 제어 문자를 거부합니다. 파일 접근은 저장소 루트 밖으로 벗어날 수 없습니다.
- **SQL**: 모든 쿼리를 파라미터 바인딩으로 작성합니다.
- **XSS**: `html/template` 자동 이스케이프와 엄격한 CSP(`default-src 'self'`, 인라인 스크립트/스타일 금지)를 적용합니다. 외부 CDN은 쓰지 않습니다. 업로드한 언어 파일의 번역문도 HTML로 해석하지 않습니다.
- **CSRF**: 여러 겹으로 막습니다.
  - 로그인 후 모든 POST: 세션별 CSRF 토큰(폼 필드 또는 HTMX 헤더)을 검사합니다.
  - 로그인 폼: double-submit 토큰을 씁니다. HttpOnly·SameSite=Strict 쿠키와 폼 값이 일치해야 하며, HTTPS에서는 `__Host-` 접두사로 하위 도메인의 쿠키 주입을 막습니다.
  - 전역: Go 표준 `http.CrossOriginProtection`(Sec-Fetch-Site/Origin 기반)이 다른 사이트나 다른 하위 도메인에서 온 POST를 핸들러 실행 전에 차단합니다.
  - 그 밖에 Origin/Referer 검사와 SameSite=Lax 세션 쿠키를 적용합니다.
  - 로그아웃하면 페이지를 완전히 새로 고쳐 이전 세션의 토큰이 남지 않게 합니다.
- **세션**: 쿠키는 HttpOnly와 SameSite로 설정하고 HTTPS에서는 Secure를 붙입니다. DB에는 토큰의 SHA-256 해시만 저장합니다. 비밀번호를 변경하면 해당 사용자의 세션을 폐기합니다.
- **비밀번호**: bcrypt로만 저장하고 평문은 저장하지 않습니다. 비밀번호와 인증 정보는 로그에 출력하지 않습니다.
- **로그인 제한**: 웹 로그인은 IP와 계정별로 15분 동안 실패 횟수를 제한합니다(기본 10회, 설정 가능).
- **SVN 무차별 대입 차단**: SVN(HTTP Basic) 로그인에 15분 안에 20회 실패한 IP는 15분 동안 차단됩니다(횟수와 시간은 시스템 설정에서 변경, 0이면 사용 안 함).
  - 차단은 Apache가 **인증보다 먼저** 429로 응답하는 방식이라, 차단된 동안에는 비밀번호를 더 시도할 수 없습니다.
  - Apache를 재시작하지 않고 바로 적용되며, 시스템 설정 화면에서 차단 목록을 보고 해제할 수 있습니다.
  - 차단과 해제는 감사 로그에 기록됩니다.
- **접근 IP 제한**: 관리 UI와 SVN에 각각 허용 IP/CIDR 목록을 둘 수 있습니다. SVN은 Apache가 인증 전에 403으로 거부하고, 관리 UI는 모든 화면과 로그인 요청을 Go 서버가 거부합니다. 저장된 목록이 손상되면 루프백만 허용하는 쪽으로 동작합니다(fail-closed). 자세한 내용은 [접근 IP 제한](#접근-ip-제한)을 참고하세요.
- **프록시 헤더 신뢰 범위**: `X-Forwarded-For`는 `SVNGO_TRUSTED_PROXIES`에 넣은 프록시에서 온 요청에서만 읽고, 그 밖의 요청은 접속 주소를 씁니다. 모든 주소를 신뢰하는 값(`0.0.0.0/0`, `::/0`)은 거부합니다.
- **요청 크기 제한**: SVN 요청(`/svn`과 SVN 위치)의 본문과 XML 요청 크기를 제한합니다. 그 밖의 경로는 1MB까지만 받고, 관리 화면은 Go 서버가 기능별로 제한합니다.
- **authz 검증 강제**: 시작할 때 검증 도구를 자체 점검합니다. 정상 파일은 통과시키고, 잘못된 파일은 거부하고, 권한을 올바르게 평가하는지 확인합니다. 하나라도 실패하면 서버가 시작되지 않으며, 검증 없이 인증 파일을 적용하는 경로는 없습니다.
- **보안 헤더**: CSP, X-Frame-Options DENY, nosniff, Referrer-Policy, Permissions-Policy, COOP, HSTS(HTTPS)를 보냅니다.
- **컨테이너**: non-root(uid 1000)로 실행하며 읽기 전용 루트 파일시스템, `cap_drop: ALL`, `no-new-privileges`를 적용합니다. PID 1은 tini입니다.
- **인증 파일**: 임시 파일에 쓰고 libsvn으로 검증한 뒤 원자적으로 교체하고, 다시 읽어 비교합니다. 실패하면 롤백합니다. **시스템 설정 → 동기화 검증**에서 DB와 파일 내용, libsvn 평가 결과의 일치 여부를 확인할 수 있습니다.

## 운영

```powershell
docker compose logs -f svngo        # 로그 (Apache 로그는 [apache] 접두사)
docker compose restart svngo        # 재시작 (데이터는 볼륨에 유지)
docker compose down                 # 중지 (볼륨 유지)
docker compose down -v              # 중지 + 데이터 삭제 (주의!)
docker compose exec svngo svngo version
```

- Go 관리 서버가 Apache를 감독합니다. Apache가 비정상 종료되면 백오프 후 다시 시작하고, 컨테이너 종료 신호(SIGTERM)를 받으면 Apache를 graceful 종료한 뒤 끝냅니다.
- 헬스체크(`svngo healthcheck`)는 Apache를 거쳐 `/admin/healthz`를 확인하므로 Apache와 Go 서버를 함께 검사합니다.

### 멀티 아키텍처 빌드

```powershell
docker buildx build --platform linux/amd64,linux/arm64 -t <레지스트리>/svngo:1.1.1 --push .
```

## 테스트

**단위 및 통합 테스트**

호스트에 `svn`, `svnadmin`, `svnlook`, `svnauthz`가 있으면 실제 SVN 도구로 실행됩니다.

```powershell
go test -count=1 ./...
```

**Docker E2E 테스트**

실제 컨테이너에 svn 클라이언트로 HTTP 요청을 보냅니다. 테스트 포트는 18080입니다.

```powershell
docker compose -p svngo-e2e -f docker-compose.yml -f e2e/compose.e2e.yml up -d --build
go test -tags e2e -count=1 -v ./e2e
docker compose -p svngo-e2e -f docker-compose.yml -f e2e/compose.e2e.yml down -v
```

E2E는 다음 항목을 실제 `svn` 명령으로 검증합니다.

1. 저장소 생성
2. 사용자 생성
3. HTTP checkout
4. HTTP commit
5. HTTP update
6. 비밀번호 변경
7. 기존 비밀번호 거부
8. 권한 변경(rw→r 후 커밋 거부, 디렉터리 거부, 규칙 삭제, 그룹 권한)
9. 권한 없는 사용자 차단
10. Apache 재시작 없이 신규 저장소 접근(PID와 재시작 횟수 비교)
11. 웹 UI 커밋 이력과 diff
12. `docker compose restart` 후 데이터 유지
13. SVN 위치: 위치 추가, `/<위치>/<저장소>` checkout·commit·서버 측 copy, 위치 경로의 SVN 접근 IP 제한, 위치 간 이동, 위치 삭제(Apache PID 유지)

## 디렉터리 구조

```
cmd/svngo/            진입점 (serve, healthcheck, admin-passwd, access-reset, version 명령)
internal/
  server/             관리 웹 앱: 핸들러, 세션·CSRF, 작업(백업·복원), DB→인증 파일 동기화
    templates/        html/template 화면 (번역 키 사용, 바이너리에 내장)
    static/           CSS, htmx(로컬 번들), 소형 JS (바이너리에 내장)
  i18n/               다국어: 언어 파일 로드·검증·등록, 브라우저 언어 매칭, 번역 메시지
    locales/          기본 언어 파일 en.json, ko.json (바이너리에 내장)
  apache/             httpd 프로세스 관리, 로그 수집, SVN 무차별 대입 차단, SVN 허용 IP 규칙 생성·적용, SVN 위치 <Location> 생성·적용
  ipacl/              허용 IP/CIDR 목록 파싱·정규화·포함 여부 판정
  svn/                svnadmin·svnlook·svn·authz 검증 도구 실행
  authz/              권한 정책 평가, htpasswd·authz 파일 생성
  store/              SQLite 스키마, 감사 로그, 설정값
  passwd/             bcrypt·APR1 해시, htpasswd 파싱
  config/             환경 변수 설정
  validate/           이름·비밀번호·권한 경로 검증
  repopath/           저장소 내부 경로 처리
  fsutil/             원자적 파일 쓰기
  logring/            최근 로그 보관(메모리)
docker/httpd.conf  Apache 설정 (mod_dav_svn, authz_svn, proxy)
docker/svngo-authz.c  libsvn 기반 authz 검증 도구 (svnauthz 호환)
docker/DOCKERHUB.md   Docker Hub Overview(영어·한국어)
docker/DOCKERHUB.short.txt  Hub 상단 한 줄 설명(영어 | 한국어); docker/update-dockerhub-overview.sh 로 반영
Dockerfile         멀티 스테이지 빌드 (Go → C 헬퍼 → alpine 런타임)
docker-compose.yml 운영용 compose
e2e/               Docker E2E 테스트
docs/images/       README 화면 캡처 (한국어 UI, en/ 은 영어 UI)
README.md          영어 문서
README.ko.md       한국어 문서 (이 문서)
```

## 버전 이력

| 버전 | 변경 내용 |
|---|---|
| 1.1.1 | 리버스 프록시 뒤의 실제 클라이언트 IP: `SVNGO_TRUSTED_PROXIES`에 넣은 프록시의 `X-Forwarded-For`를 신뢰해 접근 IP 제한, SVN 로그인 차단, 로그에 실제 IP를 사용([리버스 프록시 뒤의 클라이언트 IP](#리버스-프록시-뒤의-클라이언트-ip)). DB 스키마 변경 없음. |
| 1.1.0 | [SVN 위치](#svn-위치-여러-url-경로): `/android/<저장소>` 같은 추가 URL 경로를 같은 이름 폴더에서 제공하고, 위치 간 저장소 이동 지원. 다국어(영어·한국어 기본 제공, 언어 파일 추가 가능). 관리 UI·SVN [접근 IP 제한](#접근-ip-제한). DB 스키마는 자동으로 변환됩니다. |
| 1.0.0 | 첫 릴리스: 저장소·사용자·그룹·권한 관리, 웹 탐색, 백업·복원, SVN 무차별 대입 차단 |

## 제한 사항

- `svn://`(svnserve)는 지원하지 않으며 HTTP/HTTPS만 지원합니다.
- 인증 방식은 htpasswd(Basic 인증)뿐입니다. LDAP/AD 연동은 없습니다.
- 웹 파일 보기에는 구문 강조가 없습니다. 512KB를 넘는 파일은 일부만 보여 주고, 다운로드는 전체를 제공합니다. diff는 5,000줄 또는 2MB까지 화면에 표시하며 원본 diff는 전체를 다운로드할 수 있습니다.
- 저장소 크기는 비동기로 계산해 캐시한 값이므로 최신 커밋이 바로 반영되지 않을 수 있습니다.
- 권한 규칙의 경로에 `[ ] % \` 문자와 앞뒤 공백은 쓸 수 없습니다(authz 문법 제약).
- 컨테이너 하나 구성이라 수평 확장(다중 인스턴스)은 지원하지 않습니다.
- SVN 위치는 이름을 바꿀 수 없고(새 위치를 만든 뒤 저장소를 이동), 저장소 이름은 모든 위치에서 고유해야 합니다. 외부 Apache를 쓰는 경우(`SVNGO_MANAGE_APACHE` 끔) SVN 위치의 `<Location>` 블록을 직접 추가해야 합니다.
- SVN 로그인 차단은 클라이언트 IP 기준이며, 차단 목록은 메모리에만 있어 컨테이너를 재시작하면 초기화됩니다.
  - 일부 Docker Desktop 환경처럼 모든 클라이언트가 같은 IP로 보이면, 한 사람의 반복 실패로 모두가 차단될 수 있습니다. 이 경우 시스템 설정에서 허용 횟수를 높이거나 0으로 끄고, 프록시에서 따로 제한하세요. 리버스 프록시 때문이라면 `SVNGO_TRUSTED_PROXIES`로 해결됩니다.
- 접근 IP 제한도 Apache가 본 접속 주소 기준입니다. `X-Forwarded-For`는 `SVNGO_TRUSTED_PROXIES`에 넣은 프록시에서 온 요청에서만 사용합니다([리버스 프록시 뒤의 클라이언트 IP](#리버스-프록시-뒤의-클라이언트-ip)). `SVNGO_MANAGE_APACHE`를 끄고 외부 Apache를 쓰는 경우 SVN 목록과 `SVNGO_TRUSTED_PROXIES`는 적용되지 않습니다.
- 일부 Linux 호스트(AppArmor 또는 snap Docker)에서는 `no-new-privileges` 설정이 있으면 컨테이너 안의 모든 실행 파일이 `operation not permitted`로 실패할 수 있습니다. 이 경우 `docker-compose.yml`의 `security_opt` 항목을 제거하세요. Docker Desktop(Windows/macOS)에서는 문제없습니다.
