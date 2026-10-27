# SvnGo

Go로 작성한 경량 Subversion(SVN) 서버 및 웹 관리 시스템입니다.
**Apache httpd + mod_dav_svn**이 SVN HTTP(S) 프로토콜을 처리하고, **Go 관리 서버**가 저장소·사용자·그룹·권한을 관리합니다. 모든 구성 요소가 Docker 컨테이너 하나에 들어 있습니다.

- PHP / Node / Java / Redis / ORM / 프론트엔드 프레임워크를 사용하지 않습니다.
- 이미지 약 36MB, 유휴 메모리는 컨테이너 전체 약 12MB 수준입니다(Apache 포함).
- amd64 / arm64 멀티 아키텍처 빌드를 지원합니다.

## 구조

```
                       ┌──────────────────── SvnGo 컨테이너 (non-root, uid 1000) ─────────────────────┐
 svn 클라이언트 ─HTTP(S)─▶ Apache httpd :8080 / :8443                                                  │
 웹 브라우저            │   ├─ /svn/<저장소>  mod_dav_svn + mod_authz_svn + mod_auth_basic/authn_file   │
                       │   │                  (htpasswd: bcrypt, authz: 경로별 권한)                     │
                       │   └─ /admin         reverse proxy ──▶ Go 관리 서버 127.0.0.1:9000           │
                       │                                         ├─ SQLite (WAL) : 단일 진실 원천     │
                       │                                         ├─ htpasswd / authz 생성·검증·원자적 교체│
                       │                                         ├─ svnadmin / svnlook / svn (file://) │
                       │                                         └─ Apache 프로세스 감독(재시작/종료)  │
                       └──────────────────────────────────────────────────────────────────────────────┘
 볼륨: /data/repos  /data/config(htpasswd, authz, tls/)  /data/db  /data/backups
```

- **SQLite가 단일 진실 원천**입니다. 사용자/그룹/권한이 바뀔 때마다 DB 트랜잭션 안에서 `htpasswd`와 `authz`를 다시 생성합니다. 생성한 파일은 libsvn(`svngo-authz`)으로 검증한 뒤 임시 파일 → fsync → rename으로 원자적으로 교체하고, 다시 읽어서 비교합니다. 이 과정이 모두 성공해야 DB를 커밋하며, 실패하면 이전 파일로 되돌립니다.
- Apache는 요청마다 `htpasswd`/`authz` 파일을 읽으므로 저장소·사용자·권한 변경에 **Apache 재시작이 필요 없습니다**.
- 웹 관리자 계정(DB)과 SVN 사용자 계정(htpasswd)은 분리되어 있습니다. SVN 사용자도 웹에 로그인할 수 있지만, **자신에게 SVN 권한이 있는 저장소와 경로만** 조회할 수 있습니다.

## 주요 기능

| 영역 | 기능 |
|---|---|
| 대시보드 | 저장소/사용자/그룹/권한 수, Apache 상태, 디스크 사용량, 최근 작업과 변경 이력 |
| 저장소 | 생성(trunk/branches/tags 선택), 목록·검색·페이징, 이름 변경, 삭제(이름 재입력 확인), 활성/비활성, UUID·리비전·크기(캐시), 검증(`svnadmin verify`), 백업(dump.gz)·복원 |
| 웹 브라우징 | 디렉터리/파일 보기, 리비전 지정 보기, 원본 다운로드(스트리밍), 커밋 로그(페이징), 변경 파일, diff, 작성자·메시지. 모든 화면에 SVN 권한이 적용됩니다. |
| 사용자 | 생성, 수정, 삭제, 비밀번호 변경, 임시 비밀번호 발급, 활성/비활성, 검색, htpasswd 가져오기(bcrypt/APR1) |
| 그룹 | 생성, 수정, 삭제, 멤버 추가·제거 |
| 권한 | 저장소·디렉터리 단위 읽기/읽기쓰기/접근 금지. 대상은 사용자, 그룹, 인증된 모든 사용자(`$authenticated`), 모든 사용자(`*`). 저장 후 libsvn으로 실제 적용 결과를 검증하며, 권한 확인 도구로 즉시 확인할 수 있습니다. |
| 시스템 설정 | SVN 기본 URL, 기본 레이아웃, 페이지 크기, 로그인 시도 제한, 관리자 계정 관리, 인증 파일 동기화 검증·재생성, 저장소 디렉터리 스캔(등록) |
| 백업 | 백업 목록·다운로드·삭제·업로드, 새 저장소로 복원 또는 덮어쓰기 복원 |
| 로그 | 감사 로그(누가 무엇을 바꿨는지), 애플리케이션 로그, Apache 로그 |

## 빠른 시작 (Windows 11 PowerShell)

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
| 헬스체크 | `http://localhost/admin/healthz` |
| HTTPS (인증서 설정 시) | `https://<서버>/admin/`, `https://<서버>/svn/<저장소명>` |

### 초기 관리자 계정

- 컨테이너를 처음 시작할 때 관리자 계정이 없으면 `ADMIN_USERNAME` / `ADMIN_PASSWORD`로 생성합니다(기본 ID `admin`).
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

TortoiseSVN에서는 저장소 URL에 `http://localhost/svn/myrepo`를 입력합니다.

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `ADMIN_USERNAME` | `admin` | 초기 관리자 ID |
| `ADMIN_PASSWORD` / `ADMIN_PASSWORD_FILE` | (없음) | 초기 관리자 비밀번호(파일 지정 가능, Docker secret 용) |
| `SVNGO_HTTP_PUBLISH` / `SVNGO_HTTPS_PUBLISH` | `80` / `443` | 호스트에 공개할 포트(compose) |
| `SVNGO_SERVER_NAME` | `localhost` | Apache ServerName |
| `SVNGO_COOKIE_SECURE` | `auto` | 세션 쿠키 Secure 속성: `auto`(HTTPS 또는 `X-Forwarded-Proto: https`), `always`, `never` |
| `SVNGO_BCRYPT_COST` | `8` | SVN 사용자 bcrypt cost(5–14). Apache가 SVN 요청마다 검증하므로 너무 높이면 느려집니다. 웹 관리자는 12로 고정입니다. |
| `SVNGO_SESSION_HOURS` | `12` | 웹 세션 유효 시간 |
| `SVNGO_SVN_MAX_BODY_MB` | `1024` | SVN 요청 하나의 최대 본문 크기(MB, 0=무제한, 최대 2047). 커밋은 파일마다 PUT 요청을 하나씩 보내므로 사실상 파일 하나의 최대 크기입니다. 넘으면 413으로 거부합니다. |
| `SVNGO_SVN_MAX_XML_MB` | `64` | SVN XML 요청(REPORT/PROPFIND/MERGE)의 최대 크기(MB, 0=무제한) |
| `SVNGO_SVNAUTHZ` | 자동 탐색 | authz 검증 도구 경로(`svngo-authz` 또는 `svnauthz`). 없거나 자체 점검에 실패하면 시작하지 않습니다. |
| `SVNGO_TLS_CERT` / `SVNGO_TLS_KEY` | `/data/config/tls/server.crt` / `.key` | 이 파일들이 있으면 8443 포트에서 HTTPS를 켭니다. |
| `TZ` | `Asia/Seoul` | 표시 시간대 |

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

Nginx, Traefik, Caddy 같은 프록시에서 TLS를 종료하고 `http://<컨테이너>:8080`으로 전달합니다. 프록시가 `X-Forwarded-Proto: https`를 보내면 Secure 쿠키가 적용됩니다. 쿠키를 항상 Secure로 하려면 `SVNGO_COOKIE_SECURE=always`를 지정하세요.

## 기존 SVN 서버에서 이전하기

1. **저장소**: 기존 저장소 디렉터리를 repos 볼륨에 복사합니다. 웹 UI의 **시스템 설정 → 저장소 스캔**을 누르면 자동으로 등록됩니다. 시작할 때도 자동으로 스캔합니다.
   ```powershell
   docker compose cp .\old-repos\myrepo svngo:/data/repos/myrepo
   ```
   또는 `svnadmin dump` 파일을 **백업** 화면에서 업로드한 뒤 새 저장소로 복원합니다(.svndump / .svndump.gz).
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
- **XSS**: `html/template` 자동 이스케이프와 엄격한 CSP(`default-src 'self'`, 인라인 스크립트/스타일 금지)를 적용합니다. 외부 CDN은 쓰지 않습니다.
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
- **요청 크기 제한**: `/svn` 본문과 XML 요청 크기를 제한합니다. 그 밖의 경로는 1MB까지만 받고, 관리 화면은 Go 서버가 기능별로 제한합니다.
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
docker buildx build --platform linux/amd64,linux/arm64 -t <레지스트리>/svngo:1.0.0 --push .
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

## 디렉터리 구조

```
cmd/svngo/            진입점 (serve, healthcheck, admin-passwd, version 명령)
internal/
  server/             관리 웹 앱: 핸들러, 세션·CSRF, 작업(백업·복원), DB→인증 파일 동기화
    templates/        html/template 화면 (한국어, 바이너리에 내장)
    static/           CSS, htmx(로컬 번들), 소형 JS (바이너리에 내장)
  apache/             httpd 프로세스 관리, 로그 수집, SVN 무차별 대입 차단
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
Dockerfile         멀티 스테이지 빌드 (Go → C 헬퍼 → alpine 런타임)
docker-compose.yml 운영용 compose
e2e/               Docker E2E 테스트
```

## 제한 사항

- `svn://`(svnserve)는 지원하지 않으며 HTTP/HTTPS만 지원합니다.
- 인증 방식은 htpasswd(Basic 인증)뿐입니다. LDAP/AD 연동은 없습니다.
- 웹 파일 보기에는 구문 강조가 없습니다. 512KB를 넘는 파일은 일부만 보여 주고, 다운로드는 전체를 제공합니다. diff는 5,000줄 또는 2MB까지 화면에 표시하며 원본 diff는 전체를 다운로드할 수 있습니다.
- 저장소 크기는 비동기로 계산해 캐시한 값이므로 최신 커밋이 바로 반영되지 않을 수 있습니다.
- 권한 규칙의 경로에 `[ ] % \` 문자와 앞뒤 공백은 쓸 수 없습니다(authz 문법 제약).
- 컨테이너 하나 구성이라 수평 확장(다중 인스턴스)은 지원하지 않습니다.
- SVN 로그인 차단은 클라이언트 IP 기준이며, 차단 목록은 메모리에만 있어 컨테이너를 재시작하면 초기화됩니다.
  - 일부 Docker Desktop 환경이나 리버스 프록시 뒤처럼 모든 클라이언트가 같은 IP로 보이면, 한 사람의 반복 실패로 모두가 차단될 수 있습니다. 이 경우 시스템 설정에서 허용 횟수를 높이거나 0으로 끄고, 프록시에서 따로 제한하세요.
- 일부 Linux 호스트(AppArmor 또는 snap Docker)에서는 `no-new-privileges` 설정이 있으면 컨테이너 안의 모든 실행 파일이 `operation not permitted`로 실패할 수 있습니다. 이 경우 `docker-compose.yml`의 `security_opt` 항목을 제거하세요. Docker Desktop(Windows/macOS)에서는 문제없습니다.
