# SvnGo

**Apache httpd + mod_dav_svn**으로 SVN(HTTP/HTTPS)을 서비스하고, **Go로 만든 다국어(영어·한국어) 웹 관리 UI**로 저장소·사용자·그룹·권한을 관리하는 경량 Subversion 서버입니다. 모든 구성 요소가 컨테이너 하나에 들어 있습니다.

- 소스 코드, 상세 문서, 이슈: [github.com/isul/svngo](https://github.com/isul/svngo)

- 이미지 약 36MB, 유휴 메모리 약 12MB(Apache 포함)
- 저장소·사용자·권한을 바꿔도 **Apache 재시작이 필요 없습니다**
- non-root(uid 1000), 읽기 전용 루트 파일시스템, `cap_drop: ALL`로 실행할 수 있습니다
- 외부 DB 불필요(SQLite 내장)

## 주요 기능

| 영역 | 기능 |
|---|---|
| 저장소 | 생성(trunk/branches/tags), 이름 변경, 삭제, 활성/비활성, 검증(`svnadmin verify`), 백업(dump.gz)·복원 |
| 웹 브라우징 | 디렉터리/파일 보기, 리비전별 보기, 다운로드, 커밋 로그, 변경 파일, diff. 모든 화면에 SVN 권한 적용 |
| 사용자·그룹 | 생성, 수정, 삭제, 비밀번호 변경, 활성/비활성, htpasswd 가져오기(bcrypt / APR1-MD5) |
| 권한 | 저장소·디렉터리 단위 읽기 / 읽기쓰기 / 접근 금지. 저장 후 libsvn으로 실제 적용 결과를 검증 |
| 운영 | 대시보드, 감사 로그, 애플리케이션·Apache 로그, 인증 파일 동기화 검증 |
| 보안 | 관리 UI·SVN 각각의 접근 허용 IP 목록, SVN 무차별 대입 IP 차단(429), 웹 로그인 시도 제한, 다중 CSRF 방어, 엄격한 CSP, 요청 크기 제한 |
| 다국어 | 영어·한국어 기본 제공, 브라우저 언어 자동 선택, 사용자별 언어 고정, 시스템 기본 언어, 언어 파일(JSON) 업로드 또는 `/data/config/locales/` 폴더로 언어 추가 |

## 빠른 시작

### docker run

```bash
docker run -d --name svngo --restart unless-stopped \
  -p 80:8080 -p 443:8443 \
  -e ADMIN_PASSWORD='Change-Me-123' \
  -v svn-repos:/data/repos -v svn-config:/data/config \
  -v svn-db:/data/db -v svn-backups:/data/backups \
  --read-only \
  --tmpfs /run/svngo:size=16m,mode=0750,uid=1000,gid=1000 \
  --tmpfs /tmp:size=64m,mode=1777 \
  --cap-drop ALL \
  isul/svngo:1.0.0
```

Windows PowerShell에서는 줄 끝의 `\` 대신 백틱(`` ` ``)을 쓰거나 한 줄로 입력합니다.

### docker compose

```yaml
services:
  svngo:
    image: isul/svngo:1.0.0
    container_name: svngo
    restart: unless-stopped
    ports:
      - "80:8080"
      - "443:8443"
    environment:
      ADMIN_USERNAME: admin
      ADMIN_PASSWORD: Change-Me-123   # 8자 이상, 최초 실행 시에만 사용됩니다
      SVNGO_SERVER_NAME: svn.example.com
      TZ: Asia/Seoul
    volumes:
      - svn-repos:/data/repos
      - svn-config:/data/config
      - svn-db:/data/db
      - svn-backups:/data/backups
    read_only: true
    tmpfs:
      - /run/svngo:size=16m,mode=0750,uid=1000,gid=1000
      - /tmp:size=64m,mode=1777
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    stop_grace_period: 30s

volumes:
  svn-repos:
  svn-config:
  svn-db:
  svn-backups:
```

```bash
docker compose up -d
docker compose ps        # STATUS 가 healthy 이면 준비 완료
```

## 접속

| 용도 | URL |
|---|---|
| 웹 관리 UI | `http://<서버>/admin/` |
| SVN 저장소 | `http://<서버>/svn/<저장소명>` |
| 헬스체크 | `http://<서버>/admin/healthz` |

```bash
svn checkout http://<서버>/svn/myrepo/trunk myrepo --username kim
```

TortoiseSVN에서는 저장소 URL로 `http://<서버>/svn/myrepo`를 입력합니다.

## 초기 관리자 계정

- 처음 시작할 때 관리자 계정이 없으면 `ADMIN_USERNAME`(기본 `admin`) / `ADMIN_PASSWORD`로 만듭니다. 비밀번호는 8자 이상이어야 합니다.
- `ADMIN_PASSWORD`를 비워 두면 임의 비밀번호를 만들어 `/data/config/initial-admin-password`에 저장합니다(로그에는 출력하지 않음).

  ```bash
  docker exec svngo cat /data/config/initial-admin-password
  ```

- 비밀번호를 잊었을 때:

  ```bash
  echo 'New-Admin-Pass-123' | docker exec -i svngo svngo admin-passwd admin
  ```

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `ADMIN_USERNAME` | `admin` | 초기 관리자 ID |
| `ADMIN_PASSWORD` / `ADMIN_PASSWORD_FILE` | (없음) | 초기 관리자 비밀번호(8자 이상). 파일 경로로도 지정 가능(Docker secret) |
| `SVNGO_SERVER_NAME` | `localhost` | Apache ServerName |
| `SVNGO_COOKIE_SECURE` | `auto` | 세션 쿠키 Secure: `auto`(HTTPS 또는 `X-Forwarded-Proto: https`) / `always` / `never` |
| `SVNGO_BCRYPT_COST` | `8` | SVN 사용자 bcrypt cost(5–14). 높을수록 SVN 요청이 느려집니다 |
| `SVNGO_SESSION_HOURS` | `12` | 웹 세션 유효 시간 |
| `SVNGO_SVN_MAX_BODY_MB` | `1024` | SVN 요청 하나(사실상 파일 하나)의 최대 크기, MB. 0=무제한, 최대 2047 |
| `SVNGO_SVN_MAX_XML_MB` | `64` | SVN XML 요청 최대 크기, MB. 0=무제한 |
| `SVNGO_ADMIN_ALLOW_IPS` | (없음) | 관리 UI(`/admin`)에 항상 허용할 IP/CIDR(쉼표 구분). 웹 UI 목록과 합쳐 적용 |
| `SVNGO_SVN_ALLOW_IPS` | (없음) | SVN(`/svn`)에 항상 허용할 IP/CIDR. Apache가 인증 전에 403으로 거부 |
| `SVNGO_TLS_CERT` / `SVNGO_TLS_KEY` | `/data/config/tls/server.crt` / `.key` | 두 파일이 있으면 8443에서 HTTPS 활성화 |
| `TZ` | `Asia/Seoul` | 표시 시간대 |

## 포트와 볼륨

| 항목 | 설명 |
|---|---|
| `8080` | HTTP (SVN + 웹 관리 UI) |
| `8443` | HTTPS (인증서가 있을 때) |
| `/data/repos` | SVN 저장소 |
| `/data/config` | htpasswd, authz(자동 생성), `tls/` 인증서, `locales/` 추가 언어 파일 |
| `/data/db` | SQLite DB(사용자·그룹·권한·감사 로그) |
| `/data/backups` | 저장소 백업(.svndump.gz) |

네 볼륨을 함께 보관하면 전체 백업이 됩니다. 바인드 마운트를 쓸 때는 디렉터리 소유자를 uid/gid 1000으로 맞춰 주세요. 맞지 않으면 `directory ... is not writable ... permission denied` 오류로 시작하지 않습니다.

## 읽기 전용 루트 파일시스템 (`read_only` / `--read-only`)

예제의 `read_only: true`는 이미지 내용을 읽기 전용으로 마운트하는 보안 옵션입니다. 컨테이너 안에서 코드가 실행되더라도 실행 파일이나 설정을 바꿀 수 없습니다. 쓰기는 다음 경로에서만 일어납니다.

| 경로 | 종류 | 용도 |
|---|---|---|
| `/data/*` 네 볼륨 | 볼륨 / 바인드 마운트 | 영구 데이터 |
| `/run/svngo` | tmpfs | Apache PID 파일, SVN 로그인 차단 목록, svn 클라이언트 설정 등 런타임 파일 |
| `/tmp` | tmpfs | 임시 파일 |

- 읽기 전용으로 실행할 때는 `tmpfs` 두 항목(`/run/svngo`는 `uid=1000,gid=1000`)이 꼭 있어야 합니다. 없으면 Apache가 시작하지 못합니다.
- tmpfs 내용은 재시작하면 지워지지만 시작할 때 다시 만들어집니다.
- `read_only: false`여도 기능은 같고, 보호 장치 하나가 빠질 뿐입니다.

## 시놀로지(Synology) NAS

DSM 7.2 이상의 Container Manager에서 실행할 수 있습니다. 이미지가 linux/amd64 전용이라 Intel/AMD CPU 모델만 지원합니다.

**1. 데이터 폴더 준비 (필수).** DSM 공유 폴더는 소유자가 DSM 사용자이고 ACL이 걸려 있어 uid 1000인 컨테이너가 쓸 수 없습니다. SSH로 접속해 ACL을 제거하고 소유자를 바꿉니다(SSH는 **제어판 → 터미널 및 SNMP**에서 켭니다).

```bash
BASE=/volume1/docker/svngo
for d in repos config db backups; do
  sudo mkdir -p "$BASE/$d"
  sudo find "$BASE/$d" -exec /usr/syno/bin/synoacltool -del {} \; >/dev/null 2>&1
  sudo chown -R 1000:1000 "$BASE/$d"
  sudo chmod -R u+rwX,g+rX,o-rwx "$BASE/$d"
done
ls -ln "$BASE"   # 네 폴더 모두 1000 1000 이면 정상
```

**2. 프로젝트 만들기.** **Container Manager → 프로젝트 → 생성**에서 위 compose 예제를 붙여 넣고 다음 부분만 바꿉니다. DSM이 80/443을 쓰므로 다른 호스트 포트를 사용합니다.

```yaml
    ports:
      - "36900:8080"
      - "36901:8443"
    volumes:
      - /volume1/docker/svngo/repos:/data/repos
      - /volume1/docker/svngo/config:/data/config
      - /volume1/docker/svngo/db:/data/db
      - /volume1/docker/svngo/backups:/data/backups
```

맨 아래의 `volumes:` 블록(named volume 선언)은 지웁니다. 웹 UI는 `http://<NAS주소>:36900/admin/`입니다.

**3. HTTPS.** DSM 인증서(`/usr/syno/etc/certificate/_archive/<ID>/fullchain.pem`, `privkey.pem`)를 `$BASE/config/tls/server.crt`, `server.key`로 복사하고 uid 1000 소유로 바꾼 뒤 재시작하면 36901에서 HTTPS가 켜집니다. SVN은 이 포트로 직접 접속하세요. DSM 역방향 프록시에서 TLS를 종료하고 HTTP로 넘기면 브랜치·태그 생성(`svn copy`)과 `svn move`가 `502 Bad Gateway`로 실패할 수 있습니다.

자세한 절차(인증서 자동 갱신, 운영 팁)는 [GitHub README의 "시놀로지(Synology) NAS에서 실행하기"](https://github.com/isul/svngo/blob/main/README.ko.md#시놀로지synology-nas에서-실행하기)를 참고하세요.

## HTTPS

- **컨테이너에서 직접:** PEM 인증서와 키를 config 볼륨의 `tls/server.crt`, `tls/server.key`로 넣고 재시작하면 8443에서 HTTPS가 켜지고 HSTS와 Secure 쿠키가 적용됩니다.
- **리버스 프록시:** Nginx, Traefik, Caddy 등에서 TLS를 종료하고 `http://<컨테이너>:8080`으로 전달합니다. `X-Forwarded-Proto: https`를 보내거나 `SVNGO_COOKIE_SECURE=always`를 지정하세요.

## 기존 SVN 서버에서 이전

1. 저장소 디렉터리를 `/data/repos`에 복사하면 시작할 때(또는 **시스템 설정 → 저장소 스캔**) 자동 등록됩니다. `svnadmin dump` 파일은 **백업** 화면에서 업로드해 복원할 수 있습니다.
2. 기존 htpasswd는 **사용자 → htpasswd 가져오기**로 옮깁니다(bcrypt, APR1-MD5 지원).
3. 그룹과 권한은 웹 UI에서 다시 설정합니다. `authz` 파일은 DB에서 자동 생성되므로 직접 수정하지 마세요.

## 접근 IP 제한

**시스템 설정 → 접근 제한 (허용 IP)**에서 관리 UI와 SVN에 접근할 수 있는 IP/CIDR을 각각 정합니다. 재시작 없이 바로 적용되고, 위 환경 변수 항목은 항상 함께 허용됩니다. 목록이 모두 비어 있으면 제한하지 않으며, 루프백은 항상 허용합니다. 잠겼을 때는 다음 명령으로 웹 UI 목록을 지웁니다.

```bash
docker exec svngo svngo access-reset admin   # admin | svn | all
```

## 참고

- `svn://`(svnserve)는 지원하지 않고 HTTP/HTTPS만 지원합니다. 인증은 htpasswd(Basic)만 지원합니다.
- SVN 로그인 차단은 IP 기준입니다. 모든 클라이언트가 같은 IP로 보이는 환경(일부 Docker Desktop, 프록시 뒤)에서는 **시스템 설정**에서 허용 횟수를 높이거나 0으로 꺼 주세요.
- 일부 Linux 호스트(AppArmor, snap Docker)에서 `no-new-privileges` 때문에 `operation not permitted`가 나면 해당 `security_opt`를 제거하세요.

## 태그

| 태그 | 설명 |
|---|---|
| `1.0.0`, `latest` | linux/amd64 |

구성: Alpine 3.22, Apache 2.4, Subversion 1.14, Go 1.26
