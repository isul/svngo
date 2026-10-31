# SvnGo

**English** | [한국어](README.ko.md)

A lightweight Subversion (SVN) server and web administration system written in Go.
**Apache httpd + mod_dav_svn** serves the SVN HTTP(S) protocol, and a **Go admin server** manages repositories, users, groups and permissions. Everything runs in a single Docker container.

- No PHP / Node / Java / Redis / ORM / frontend framework.
- The image is about 36 MB, and the whole container idles at about 12 MB of memory (Apache included).
- Multi-architecture builds for amd64 / arm64.
- Prebuilt images are available on Docker Hub: [isul/svngo](https://hub.docker.com/r/isul/svngo).

## Architecture

```
                       ┌─────────────────────── SvnGo container (non-root, uid 1000) ────────────────────────┐
 svn client ───HTTP(S)─▶ Apache httpd :8080 / :8443                                                          │
 web browser           │   ├─ /svn/<repo>    mod_dav_svn + mod_authz_svn + mod_auth_basic/authn_file         │
                       │   ├─ /<location>/<repo>  same, one <Location> per SVN location (generated)          │
                       │   │                 (htpasswd: bcrypt, authz: per-path permissions)                 │
                       │   └─ /admin         reverse proxy ──▶ Go admin server 127.0.0.1:9000                │
                       │                                       ├─ SQLite (WAL): single source of truth       │
                       │                                       ├─ generates, verifies and atomically swaps   │
                       │                                       │  htpasswd / authz                           │
                       │                                       ├─ svnadmin / svnlook / svn (file://)         │
                       │                                       └─ supervises Apache (restart / stop)         │
                       └─────────────────────────────────────────────────────────────────────────────────────┘
 Volumes: /data/repos  /data/config (htpasswd, authz, tls/, locales/)  /data/db  /data/backups
```

- **SQLite is the single source of truth.** Whenever users, groups or permissions change, `htpasswd` and `authz` are regenerated inside the DB transaction. The generated files are verified with libsvn (`svngo-authz`), swapped in atomically (temp file → fsync → rename) and read back for comparison. The DB is committed only if all of this succeeds; otherwise the previous files are restored.
- Apache reads the `htpasswd`/`authz` files on every request, so changes to repositories, users and permissions **never require an Apache restart**.
- Web admin accounts (DB) and SVN user accounts (htpasswd) are separate. SVN users can also sign in to the web UI, but they can only browse **the repositories and paths they have SVN access to**.

## Features

| Area | Features |
|---|---|
| Dashboard | Counts of repositories/users/groups/permissions, Apache status, disk usage, recent jobs and changes |
| Repositories | Create (optional trunk/branches/tags), list, search and paging, rename, move between SVN locations, delete (confirm by retyping the name), enable/disable, UUID, revision and size (cached), verify (`svnadmin verify`), backup (dump.gz) and restore |
| Web browsing | Directory and file views, view at a revision, raw download (streamed), commit log (paged), changed files, diff, author and message. SVN permissions apply to every page. |
| Users | Create, edit, delete, change password, issue a temporary password, enable/disable, search, import htpasswd (bcrypt/APR1) |
| Groups | Create, edit, delete, add and remove members |
| Permissions | Read / read-write / no access per repository and directory. Subjects are users, groups, all authenticated users (`$authenticated`) and everyone (`*`). After saving, the effective result is verified with libsvn, and the access check tool shows it immediately. |
| System settings | SVN base URL, SVN locations (extra URL paths such as `/android/<repo>`), default layout, page size, login attempt limits, allowed IPs for the admin UI and SVN, web admin accounts, auth file sync check and regeneration, repository directory scan (registration) |
| Backups | List, download, delete and upload backups; restore into a new repository or overwrite an existing one |
| Logs | Audit log (who changed what), application log, Apache log |
| Languages | English and Korean built in, automatic selection from the browser language, per-user language in My account, system default language, add/upload/download language files (JSON) |

## Screenshots

Web admin pages captured with sample data (5 repositories, 7 users, 4 groups) in the English UI.

**Dashboard**: counts of repositories, users, groups and permissions, Apache status, disk usage, recent jobs and changes

![Dashboard](docs/images/en/02-dashboard.png)

**Repository overview**: checkout URL, UUID, revision and size, recent commits

![Repository overview](docs/images/en/04-repo-overview.png)

**Commit details and diff**: author and message, changed files, per-file diff

![Commit details and diff](docs/images/en/08-repo-commit-diff.png)

**Repository permissions**: per-path permission rules and the access check tool (SvnGo's evaluation shown next to the actual libsvn result)

![Repository permissions](docs/images/en/09-repo-perms.png)

<details>
<summary>More screenshots (login, repository list, file browser, users and groups, system settings, backups, logs, dark mode)</summary>

**Login**

![Login](docs/images/en/01-login.png)

**Repository list**

![Repository list](docs/images/en/03-repos.png)

**File browser**

![File browser](docs/images/en/05-repo-files.png)

**File view**

![File view](docs/images/en/06-repo-file.png)

**Commit log**

![Commit log](docs/images/en/07-repo-commits.png)

**All permission rules**

![All permission rules](docs/images/en/10-perms.png)

**User list**

![User list](docs/images/en/11-users.png)

**Edit user**: edit details, change password or issue a temporary password, groups, effective permissions

![Edit user](docs/images/en/12-user-edit.png)

**Group list**

![Group list](docs/images/en/13-groups.png)

**Edit group**: add and remove members, group permissions

![Edit group](docs/images/en/14-group-edit.png)

**System settings**: server status, auth file sync check, SVN access block list, server settings, web admin accounts

![System settings](docs/images/en/15-system.png)

**Backup / restore**

![Backup / restore](docs/images/en/16-backups.png)

**Audit log**

![Audit log](docs/images/en/17-logs.png)

**Dark mode**: follows the operating system setting (`prefers-color-scheme`).

![Dark mode dashboard](docs/images/en/18-dashboard-dark.png)

**Signed in as an SVN user**: only repositories the user can access are listed and the admin menus are hidden (user `jung.hoon` of the QA group).

![SVN user view](docs/images/en/19-svn-user-repos.png)

</details>

## Docker Hub image

You can use the image published on Docker Hub instead of building from source.

- Repository: [hub.docker.com/r/isul/svngo](https://hub.docker.com/r/isul/svngo)
- Image name: `isul/svngo`

| Tag | Platform | Description |
|---|---|---|
| `1.1.0` | linux/amd64 | Pinned version. Recommended for production |
| `latest` | linux/amd64 | Latest release |

- Built with Alpine 3.22, Apache 2.4, Subversion 1.14, Go 1.26.
- On ARM hosts (some Synology models, Raspberry Pi, etc.) build an arm64 image yourself with a [multi-architecture build](#multi-architecture-build).

### Run with docker run (Windows 11 PowerShell)

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
  isul/svngo:1.1.0
```

PowerShell treats unquoted commas as array separators, so quote the `--tmpfs` values. See [Container security options](#container-security-options) for what each option does.

### Run with docker compose

Replace the `build:` block and the `image:` line in this repository's `docker-compose.yml` as follows to use the Docker Hub image without building. Keep the rest of the settings and `.env` as they are.

```yaml
services:
  svngo:
    image: isul/svngo:1.1.0
```

```powershell
docker compose pull
docker compose up -d
docker compose ps        # ready when STATUS is healthy
```

### Updating the image

All data lives in the four volumes, so it survives recreating the container from a new image. Back up the volumes before updating.

```powershell
# compose: change the tag to the new version, then
docker compose pull
docker compose up -d

# docker run: pull the new image and remove the old container, then
docker pull isul/svngo:<new-version>
docker stop svngo; docker rm svngo
# run the docker run command above again with the new tag (same volume names)
```

- The DB schema is migrated automatically on the first start of a new version (e.g. 1.0.0 → 1.1.0); nothing has to be done by hand.
- Going back to an older image after the migration is not supported. An older version ignores newer data (for example it does not serve SVN locations), so restore the `svn-db` backup taken before the update if you must downgrade.
- See [Version history](#version-history) for what changed in each version.

## Quick start (Windows 11 PowerShell)

This builds the image from source and runs it. To skip the build, see [Docker Hub image](#docker-hub-image).

Prerequisite: install and start [Docker Desktop](https://www.docker.com/products/docker-desktop/).

```powershell
cd C:\work\svngo

# 1) Create the environment file (be sure to change the admin password)
Copy-Item .env.example .env
notepad .env

# 2) Build and start
docker compose up -d --build

# 3) Check the status (ready when STATUS becomes healthy)
docker compose ps
docker compose logs -f svngo
```

If port 80 is already in use, change it in `.env`, e.g. `SVNGO_HTTP_PUBLISH=8081`.

### URLs

| Purpose | URL |
|---|---|
| Web admin UI | `http://localhost/admin/` (`http://localhost/` redirects there) |
| SVN repository | `http://localhost/svn/<repository>` |
| SVN repository in an [SVN location](#svn-locations-multiple-url-paths) | `http://localhost/<location>/<repository>` (e.g. `/android/app`) |
| Health check | `http://localhost/admin/healthz` |
| HTTPS (with a certificate) | `https://<server>/admin/`, `https://<server>/svn/<repository>` |

### Initial admin account

- On first start, if there is no admin account, one is created from `ADMIN_USERNAME` / `ADMIN_PASSWORD` (default ID `admin`). The password must be at least 8 characters.
- If `ADMIN_PASSWORD` is empty, a random password is generated and saved to the `initial-admin-password` file (mode 0600) in the config volume. The password is never written to the logs.

  ```powershell
  docker compose exec svngo cat /data/config/initial-admin-password
  # after signing in and changing the password, delete the file
  docker compose exec svngo rm /data/config/initial-admin-password
  ```

- Once the admin account exists, changing `ADMIN_PASSWORD` has no effect. If you forget the password, reset it with the following command (the account is created if the ID does not exist).

  ```powershell
  "New-Admin-Pass-123" | docker compose exec -T svngo svngo admin-passwd admin
  ```

### Using SVN

Create a repository and a user in the web UI, grant permissions, then:

```powershell
svn checkout http://localhost/svn/myrepo/trunk myrepo --username kim
cd myrepo
"hello" | Out-File -Encoding utf8 readme.txt
svn add readme.txt
svn commit -m "first commit"
svn update
```

In TortoiseSVN, enter `http://localhost/svn/myrepo` as the repository URL. A repository in an SVN location uses its own path, e.g. `http://localhost/android/app`; the repository list and overview page show the exact URL.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `ADMIN_USERNAME` | `admin` | Initial admin ID |
| `ADMIN_PASSWORD` / `ADMIN_PASSWORD_FILE` | (none) | Initial admin password (8+ characters; can be read from a file, for Docker secrets) |
| `SVNGO_HTTP_PUBLISH` / `SVNGO_HTTPS_PUBLISH` | `80` / `443` | Host ports to publish (compose) |
| `SVNGO_SERVER_NAME` | `localhost` | Apache ServerName |
| `SVNGO_COOKIE_SECURE` | `auto` | Secure attribute of the session cookie: `auto` (HTTPS or `X-Forwarded-Proto: https`), `always`, `never` |
| `SVNGO_BCRYPT_COST` | `8` | bcrypt cost for SVN users (5–14). Apache verifies it on every SVN request, so high values slow SVN down. Web admins always use 12. |
| `SVNGO_SESSION_HOURS` | `12` | Web session lifetime |
| `SVNGO_SVN_MAX_BODY_MB` | `1024` | Maximum body size of one SVN request (MB, 0 = unlimited, max 2047). A commit sends one PUT request per file, so this is effectively the maximum file size. Larger requests are rejected with 413. |
| `SVNGO_SVN_MAX_XML_MB` | `64` | Maximum size of SVN XML requests (REPORT/PROPFIND/MERGE) (MB, 0 = unlimited) |
| `SVNGO_ADMIN_ALLOW_IPS` | (none) | IPs/CIDRs always allowed to reach the admin UI (`/admin`), separated by commas or spaces. Combined with the list set in the web UI. See [Restricting access by IP](#restricting-access-by-ip) |
| `SVNGO_SVN_ALLOW_IPS` | (none) | IPs/CIDRs always allowed to reach SVN (`/svn`). The server does not start if the format is invalid. |
| `SVNGO_TRUSTED_PROXIES` | (none) | IPs/CIDRs of reverse proxies whose `X-Forwarded-For` is trusted. For requests from these addresses, access restriction, login blocking and logs use the client IP from the header. See [Client IP behind a reverse proxy](#client-ip-behind-a-reverse-proxy) |
| `SVNGO_SVNAUTHZ` | auto-detected | Path of the authz validator (`svngo-authz` or `svnauthz`). The server does not start if it is missing or fails its self-test. |
| `SVNGO_TLS_CERT` / `SVNGO_TLS_KEY` | `/data/config/tls/server.crt` / `.key` | HTTPS is enabled on port 8443 when these files exist. |
| `TZ` | `Asia/Seoul` | Display time zone |

## Container security options

`docker-compose.yml` ships with the following security options. Everything works the same without them, but keeping them is recommended.

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

### `read_only: true` (read-only root filesystem)

Mounts the container's root filesystem (the image contents) read-only. With `docker run` it is `--read-only`.

- Only explicitly mounted volumes and tmpfs are writable; everything else, such as `/usr/local/bin` or `/etc`, is not.
- Even if a vulnerability lets code run inside the container, it cannot change executables or the Apache configuration, or plant files.
- Data accumulates only in volumes, never in the image layer, so recreating the container never mixes old state in.

SvnGo writes to these paths:

| Path | Type | Purpose |
|---|---|---|
| `/data/repos`, `/data/config`, `/data/db`, `/data/backups` | Volume or bind mount | Repositories, htpasswd and authz, SQLite DB, backups (persistent) |
| `/run/svngo` | tmpfs (memory) | Apache PID file and SSL session cache, SVN login block list, svn client config, temporary files for the authz self-test |
| `/tmp` | tmpfs (memory) | Temporary files of SQLite and libraries |

- With `read_only: true`, both `tmpfs:` entries are required. Without them Apache cannot create its PID file and fails to start.
- `/run/svngo` gets `uid=1000,gid=1000` so the container user (uid/gid 1000) can write to it. If you build the image with a different UID, change these values too.
- tmpfs lives in memory and is cleared on restart, but it only holds data that is recreated at startup. That is also why the SVN login block list resets after a restart.
- `read_only: false` works the same; it just removes one layer of protection. The `directory ... is not writable` error is a volume ownership problem unrelated to this option (see the [Synology notes](#1-prepare-the-data-folders-required)).

### Other options

| Option | Description |
|---|---|
| `cap_drop: ALL` | Drops all Linux kernel capabilities. SvnGo runs as non-root and does not open ports below 1024, so it needs none. As a consequence, file ownership cannot be changed inside the container, so set the ownership of bind-mounted folders on the host. |
| `no-new-privileges:true` | Prevents privilege escalation through setuid executables and the like. If it causes problems on some Linux hosts, see [Limitations](#limitations). |

## HTTPS

**Option 1: terminate TLS in the container**

Store a PEM certificate and key under `tls/` in the config volume, owned by the container user, then restart.

```powershell
docker compose exec svngo mkdir -p /data/config/tls
Get-Content .\server.crt -Raw | docker compose exec -T svngo sh -c "cat > /data/config/tls/server.crt"
Get-Content .\server.key -Raw | docker compose exec -T svngo sh -c "umask 077; cat > /data/config/tls/server.key"
docker compose restart svngo
docker compose logs svngo | Select-String "tls=true"
```

You can also add the bind mount `- ./tls:/data/config/tls:ro` to the compose file. The key file must then be readable by the container user (uid 1000).

When HTTPS is on, the HSTS header and Secure cookies are applied automatically.

**Option 2: external reverse proxy**

Terminate TLS in a proxy such as Nginx, Traefik or Caddy and forward to `http://<container>:8080`. Secure cookies are used when the proxy sends `X-Forwarded-Proto: https`. To always use Secure cookies, set `SVNGO_COOKIE_SECURE=always`. To restrict access and block logins by the real client IP, also set up [Client IP behind a reverse proxy](#client-ip-behind-a-reverse-proxy).

## Running on a Synology NAS

This targets **Container Manager** on DSM 7.2 or later. The published image (`isul/svngo`) is linux/amd64 only, so it runs on Intel/AMD models. On ARM models, build an arm64 image yourself with a [multi-architecture build](#multi-architecture-build).

### 1. Prepare the data folders (required)

The container runs as uid/gid 1000 with `cap_drop: ALL`, so it cannot change folder permissions itself. DSM shared folders are owned by DSM users (uid 1026 and up) and carry ACLs, and Docker creates missing mount folders as root. Mounted as is, the server refuses to start with:

```
error: directory /data/repos is not writable (check the volume ownership): open /data/repos/.svngo-write-test: permission denied
```

Enable SSH in **Control Panel → Terminal & SNMP** and connect to the NAS from Windows PowerShell.

```powershell
ssh <dsm-admin-account>@<nas-address>
```

In the NAS shell, create the four folders, remove their ACLs and change the owner to uid/gid 1000. Adjust `BASE` to your setup. It is safe to run on folders that already contain data.

```bash
BASE=/volume1/docker/svngo
for d in repos config db backups; do
  sudo mkdir -p "$BASE/$d"
  sudo find "$BASE/$d" -exec /usr/syno/bin/synoacltool -del {} \; >/dev/null 2>&1
  sudo chown -R 1000:1000 "$BASE/$d"
  sudo chmod -R u+rwX,g+rX,o-rwx "$BASE/$d"
done
ls -ln "$BASE"   # OK when all four folders are owned by 1000 1000
```

To keep repositories in another shared folder (e.g. `/volume2/DEV/svn`), run the same commands on that folder. Apply them only to a subfolder dedicated to SvnGo, not to the whole shared folder.

### 2. Create the project

In **Container Manager → Project → Create**, set the project name (`svngo`) and path (`/volume1/docker/svngo`), choose **Create docker-compose.yml** as the source and paste the following. DSM uses ports 80/443, so other host ports are used.

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
      ADMIN_PASSWORD: Change-Me-123   # 8+ characters, used only on the first start
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

If the DSM firewall is on, allow ports 36900 and 36901 in **Control Panel → Security → Firewall**. After it starts, open:

| Purpose | URL |
|---|---|
| Web admin UI | `http://<nas-address>:36900/admin/` |
| SVN repository | `http://<nas-address>:36900/svn/<repository>` |
| SVN location repository | `http://<nas-address>:36900/<location>/<repository>` |

### 3. HTTPS (using the DSM certificate)

Copy a certificate registered in DSM (Let's Encrypt, etc.) to `tls/` in the config folder and the container enables HTTPS on port 36901. This example copies the DSM default certificate.

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

Certificates must be copied again after they renew, so it is convenient to register the commands above as a **Control Panel → Task Scheduler → User-defined script** (user `root`, weekly). In Task Scheduler you can omit `sudo`.

For SVN, connect directly to the container's HTTPS port, e.g. `https://<nas-address>:36901/svn/<repository>`. If a DSM **reverse proxy** terminates HTTPS and forwards to `http://localhost:36900`, the web UI, checkout and commit work. However, creating branches and tags (`svn copy`) and `svn move` can fail with `502 Bad Gateway` (`Destination URI refers to different scheme or port`) because the request scheme (https) differs from the server scheme (http).

### 4. Tips

- Containers created in Container Manager can also be managed with `sudo docker` over SSH.

  ```bash
  sudo docker logs -f svngo
  echo 'New-Admin-Pass-123' | sudo docker exec -i svngo svngo admin-passwd admin
  ```

- After copying repositories or [SVN location](#svn-locations-multiple-url-paths) folders into `repos` (e.g. `repos/android/`), run the `chown -R 1000:1000` / `chmod` commands from step 1 again, then register them in **System settings**.
- DSM system folders such as `#recycle` and `@eaDir` in the repository folder are skipped by the repository scan.
- When backing up with Hyper Backup or snapshots, include all four folders (`repos`, `config`, `db`, `backups`).
- If every executable in the container fails with `operation not permitted`, remove the `security_opt` entry.

## Languages

The web admin UI ships with English (`en`) and Korean (`ko`). Registering language files (JSON) adds other languages or adjusts the built-in translations without a restart. Server logs, CLI output and startup errors are always in English, regardless of the language settings.

### How the display language is chosen

1. The language a signed-in user picked in **My account → Language** (applies regardless of the browser language)
2. When signed out, the language picked last (cookie `svngo_lang`, kept for one year)
3. A language matching the browser setting (`Accept-Language`). Entries are tried in priority (`q`) order, and a regional tag such as `ko-KR` also matches `ko`. Conversely, if the browser asks for `pt` and only `pt-br` is registered, `pt-br` is used.
4. **System settings → Server settings → Default language** (English initially)
5. English if none of the above applies

Both web admins and SVN users can pick their own language. Choosing **Automatic (browser language)** goes back to following the browser. If the file of a language picked by a user or set as the default is deleted, that setting is ignored and the next step in the order applies.

### Language file format

A flat JSON file saved as UTF-8. `_meta` holds the language code and name; everything else is `"key": "translation"` pairs.

```json
{
  "_meta": { "code": "ja", "name": "日本語" },
  "nav.repos": "リポジトリ",
  "pager.total": "合計 {0} 件"
}
```

| Item | Rules |
|---|---|
| `_meta.code` | Language code: 2–3 lowercase letters, optionally followed by `-` and a 2–8 character region (e.g. `ja`, `fr`, `pt-br`, `zh-tw`). Upper case and `_` are converted to lower case and `-` (`pt_BR` → `pt-br`). |
| `_meta.name` | Name shown in the language lists. Required, at most 64 characters. Writing it in the language itself is recommended (`日本語`, `Français`). |
| Keys | Names prefixed by UI area (`nav.*` menus, `repo.*` repositories, `user.*` users, `system.*` system settings, etc.). There are currently 578. Only keys present in the English reference file are used; unknown keys are ignored. |
| Values | Strings only (no numbers, arrays or objects). Keys can be up to 200 bytes and values up to 4,000 bytes. |
| `{0}`, `{1}` … | Placeholders replaced at run time with repository names, counts and so on. Keep the numbers and only move them to fit the word order. Some placeholders receive code formatting or links. |

- Translations are never interpreted as HTML; they are always escaped. Tags such as `<b>` appear literally.
- Partial translations are fine. Untranslated keys are shown in English.
- **File names**: a file placed in the folder must be named `<code>.json`, and if it has `_meta.code`, it must match the file name. On upload, `_meta.code` takes precedence; without it the file name is used (`ja.json` → `ja`). Uploaded files are saved as `<code>.json` with sorted keys.

### Creating a new language file

Start from the English language file, which always contains every key. You can get it in two ways:

- **From a running server** (web admins): in **System settings → Languages**, click **Download** for English (`en`). The file matches that server's version exactly.
- **Without admin access or a server**: download [`internal/i18n/locales/en.json`](internal/i18n/locales/en.json) from this repository. The Korean file [`ko.json`](internal/i18n/locales/ko.json) is in the same folder if a second reference helps.

  ```powershell
  Invoke-WebRequest https://raw.githubusercontent.com/isul/svngo/main/internal/i18n/locales/en.json -OutFile ja.json
  ```

  This is the file of the latest source. If your server is older, keys it does not know are ignored; if it is newer, keys missing from your file are shown in English. Either way the file still works, and the server download is the exact match.

Then:

1. Get the English file as described above.
2. Rename the file to the new language code (e.g. `ja.json`) and change `code` and `name` in `_meta`. If `_meta.code` stays `en`, uploading the file overrides the English text instead of adding a new language.
3. Translate the values only. Do not change the keys or placeholders such as `{0}`.
4. Upload the file in **System settings → Languages** (a web admin does this). The **Coverage** column and the missing count show which keys are left.
5. Pick the new language in **My account → Language**, or change your browser language, to check it. To make it the default for everyone, choose it in **Server settings → Default language**.

To change only some built-in strings, upload a file with the built-in language code (e.g. `ko.json`) that contains just the keys to change. Keys not in the file keep the built-in translation.

### Registering a language

- **Upload**: upload a `.json` file in **System settings → Languages**. It is validated and applied immediately. A file with the same code is replaced.
- **Folder**: put the file in the `locales/` folder of the config volume (`/data/config/locales/<code>.json`) and click **Reload**. The folder is also read at startup, and the server creates it at startup if it is missing.

Copying into the Docker volume from PowerShell:

```powershell
docker compose cp .\ja.json svngo:/data/config/locales/ja.json
```

With bind mounts, as on Synology, put the file in the host's `config/locales/` folder (e.g. `/volume1/docker/svngo/config/locales/`). The file must be readable by the container user (uid 1000).

### Managing languages

**System settings → Languages** lists every registered language.

| Column | Meaning |
|---|---|
| Code | Language code. The default language is marked **default**. |
| Source | **Built-in**, **File** (added language), **Built-in + file** (built-in translation overridden by a file) |
| Coverage | Share of English keys that are translated, and the missing count |

- **Download**: a complete file with the current translation, missing keys filled in with English. Use it to continue translating or as a backup.
- **Delete**: available only for languages registered from a file. An added language disappears from the list; deleting an override file brings back the built-in translation. Built-in languages themselves cannot be deleted.
- Uploads and deletions are recorded in the audit log.

### Troubleshooting

An uploaded file with problems is not registered, and the reason is shown on screen. A problematic file in the folder is skipped on its own: it is shown as a warning in **System settings → Languages** and logged as `language file skipped`. All other languages keep working.

| Error | Cause and fix |
|---|---|
| Invalid JSON | Check commas, quotes and braces. The file must be UTF-8. |
| _meta.code (language code) is required | There is no `_meta.code` and no code could be taken from the file name. Add `_meta.code`. |
| Invalid language code "…" | Fix the code to follow the rules (`ja`, `pt-br` format). |
| _meta.code (…) does not match the file name (…) | Make the file name in the folder match `_meta.code`. |
| invalid language code in the file name | Rename the file in the folder to `<code>.json`. |
| _meta.name (language name) is required and must be at most 64 characters | Add `_meta.name`. |
| The value of "…" must be a string | Make the value of that key a quoted string. |
| The language file is too large / Too many keys / Too many languages | One of the limits below was exceeded. |

### Limits and upgrades

- Up to 512 KB per file, 5,000 keys and 50 languages (including the built-in ones).
- When a new version adds UI strings, keys missing from existing language files are shown in English. After upgrading, check the coverage in the language list; downloading the language again gives you a file with the new keys filled in with English, so you only need to translate those and upload it again.
- The built-in language files are in `internal/i18n/locales/` in the source. A new key must be added to both `en.json` and `ko.json`; a test checks that both files have the same keys and placeholders.

## Restricting access by IP

You can set which client IPs may reach the admin UI and SVN, separately.

| List | Scope | Blocked by | Takes effect |
|---|---|---|---|
| Admin UI | All of `/admin` (web sign-in of both web admins and SVN users) | Go admin server, 403 page | Immediately on save |
| SVN | `/svn` and every [SVN location](#svn-locations-multiple-url-paths) (all paths except `/admin`; svn clients: checkout, commit, etc.) | Apache, 403 **before authentication** | Immediately on save (Apache graceful restart) |

**Configuration**

- **Web UI**: enter each list in **System settings → Access restriction (allowed IPs)** and save. No restart is needed.
- **Environment variables**: entries in `SVNGO_ADMIN_ALLOW_IPS` and `SVNGO_SVN_ALLOW_IPS` are **combined** with the web UI lists and always allowed. They cannot be changed in the web UI and are shown read-only. Putting the admin PC or the office network here is recommended so you cannot lock yourself out by mistake. Changing them requires recreating the container.

```powershell
# .env (docker compose)
SVNGO_ADMIN_ALLOW_IPS=192.168.10.0/24
SVNGO_SVN_ALLOW_IPS=192.168.0.0/16,10.8.0.0/24
```

**Rules**

- Entries are IPv4/IPv6 addresses or CIDR ranges (`192.168.0.10`, `192.168.0.0/24`, `2001:db8::/48`), separated by newlines, commas or spaces. Text after `#` is a comment. Each list holds up to 200 entries.
- When both the environment variable and the web UI list are **empty, nothing is restricted** (the default).
- Loopback (`127.0.0.1`, `::1`) is always allowed so the container health check and checks run with `docker compose exec` are never blocked.
- Saving the admin UI list is refused if your current IP is not in it (to prevent locking yourself out). The page shows **Your current IP**.
- The SVN list is applied only if the Apache configuration test (`httpd -t`) passes. On failure, the previous list and Apache state are kept. It is a graceful restart, so checkouts and commits in progress are not interrupted and the Apache PID does not change.
- Changes are recorded in the audit log (`Access restriction changed`).

**Recovering from a lockout**

Clear the lists set in the web UI (the environment variable lists stay). The admin UI recovers immediately and SVN within 20 seconds.

```powershell
docker compose exec svngo svngo access-reset admin   # admin UI list only
docker compose exec svngo svngo access-reset svn     # SVN list only
docker compose exec svngo svngo access-reset all     # both
```

**Note**: the client IP is the connection address Apache sees. Behind Docker Desktop (Windows/macOS), NAT or a reverse proxy, all clients may appear with the same address (e.g. `172.17.0.1` or the proxy IP). If **Your current IP** differs from your PC's real address, IP restrictions cannot tell clients apart. If a reverse proxy causes this, set `SVNGO_TRUSTED_PROXIES` as described below; for other kinds of NAT, restrict access in the host firewall or the proxy instead.

### Client IP behind a reverse proxy

Behind a reverse proxy such as Caddy or Nginx, Apache only sees the proxy's address. When the proxy connects to the published port from the same host, it appears as the Docker bridge gateway (e.g. `172.17.0.1`, `172.22.0.1`). Every user then has the same IP, so IP access restriction is meaningless and repeated SVN login failures by one person block everyone.

When the proxy address is listed in `SVNGO_TRUSTED_PROXIES`, requests from it use the client IP in `X-Forwarded-For` (Apache `mod_remoteip`). IP access restriction (admin UI and SVN), SVN login blocking, the access log and **Your current IP** all use that address.

1. Before setting it, check **Your current IP** under **System settings → Access restriction**. The address shown there is the proxy address.
2. Put that address in `.env` and recreate the container.

   ```powershell
   # .env
   SVNGO_TRUSTED_PROXIES=172.22.0.1
   ```

   ```powershell
   docker compose up -d
   ```

3. Check that **Your current IP** now shows your PC's address. **Trusted proxies** on the same page shows the configured list.

The proxy only has to send `X-Forwarded-For`. Caddy's `reverse_proxy` sends it by default, so nothing else is needed.

```
svn.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

For Nginx, set `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;` and `proxy_set_header X-Forwarded-Proto $scheme;`.

**Rules**

- The header is read from the right, and the first address that is not a trusted proxy is the client IP. Values a client puts in the header itself come before the real address the proxy appends, so the IP cannot be spoofed.
- Requests from addresses not in the list ignore the header and use the connection address. The default (empty list) behaves as in earlier versions.
- Private addresses such as `192.168.0.10` are accepted as client IPs.
- Values that trust every address, such as `0.0.0.0/0` or `::/0`, would let anyone spoof their IP and are rejected (the server does not start).
- Only the Apache managed by SvnGo uses this. With an external Apache (`SVNGO_MANAGE_APACHE` off), configure `mod_remoteip` in that Apache.

**Security note**: requests from a trusted address can set the client IP through `X-Forwarded-For`. Make sure nothing but the proxy can connect from that address.

- If the proxy runs on the same host, publishing the port on loopback only (`SVNGO_HTTP_PUBLISH=127.0.0.1:8080`) and proxying to `127.0.0.1:8080` is recommended. The SvnGo port then cannot be reached directly from the LAN.
- Alternatively, put the proxy on the same Docker network as SvnGo and proxy to `http://svngo:8080`. In that case list the proxy container's fixed IP.
- Recreating the Docker network can change the gateway address. If **Your current IP** shows the gateway address again, update the value.

## SVN locations (multiple URL paths)

Repositories are served at `/svn/<repository>` by default. An **SVN location** adds another URL path whose repositories live in the folder of the same name inside the repos volume, so the URLs of an existing server with several `<Location>` blocks can be kept as they are.

| URL | Folder |
|---|---|
| `/svn/<repository>` (default) | `/data/repos/<repository>` |
| `/android/<repository>` | `/data/repos/android/<repository>` |
| `/hw/<repository>` | `/data/repos/hw/<repository>` |

```
/data/repos/            ← default location (/svn)
├── myrepo/             → /svn/myrepo
├── android/            ← location "android"
│   ├── app/            → /android/app
│   └── launcher/       → /android/launcher
└── hw/                 ← location "hw"
    └── board/          → /hw/board
```

**Managing locations** (**System settings → SVN locations**)

- **Add**: enter a name. The folder is created if it does not exist, and repositories already in it are registered. The Apache configuration is tested (`httpd -t`) and applied with a graceful restart, so checkouts and commits in progress are not interrupted and the Apache PID does not change. If the test fails, nothing changes.
- Folders in the repos volume that contain repositories but are not registered as locations are offered with a **Register /<name>** button.
- **Delete**: only possible when the location has no repositories left. The folder is removed only if it is empty.
- A location cannot be renamed. Names follow the repository name rules; `admin` and `svn` are reserved.

**Rules**

- **Repository names are unique across all locations.** Permissions (`authz`), the web UI (`/admin/repos/<name>`) and backups identify a repository by its name alone; mod_authz_svn matches `[repo:/path]` by folder name, so one `authz` and one `htpasswd` serve every location.
- A repository at `/svn` cannot have the name of a location, and a location cannot be added while a `/svn` repository has its name.
- A folder whose name is already used by a registered repository in another location is not registered; it is reported as a **name conflict** in system settings and in the sync check.
- **Create** a repository in a location by choosing it in the create form. **Move** a repository on its **Settings** page (**Rename / move**); existing working copies need `svn relocate`.
- **Restore** into a new repository asks for its location; overwriting an existing repository keeps its location.
- The SVN IP allow list and SVN brute-force blocking apply to every SVN path (everything except `/admin`).
- When **SVN base URL** is set (e.g. `https://svn.example.com/svn`), the URLs of other locations replace the trailing `/svn` (`https://svn.example.com/android/...`).
- Keep location folders on the same volume as `/data/repos`. Each location has its own work folder (`.svngo-tmp`) so creating and restoring work on any mount, but moving a repository between locations is a rename and fails across mounts.
- With an external Apache (`SVNGO_MANAGE_APACHE` off), add a `<Location /<name>>` block with `SVNParentPath /data/repos/<name>` (same directives as `/svn`) for every location yourself.

## Migrating from an existing SVN server

1. **Repositories**: copy the existing repository directories into the repos volume. When **System settings** in the web UI warns about unregistered repositories, click **Register**. They are also scanned and registered at startup.
   ```powershell
   docker compose cp .\old-repos\myrepo svngo:/data/repos/myrepo
   ```
   Alternatively, upload an `svnadmin dump` file on the **Backups** page and restore it into a new repository (.svndump / .svndump.gz).

   If the old server has several `<Location>` blocks (e.g. `/android` with `SVNParentPath /var/svn/android`), copy each parent folder to a folder of the same name (`/data/repos/android/`) and register it in **System settings → SVN locations** (or click **Register /android**). The URLs stay the same, so working copies only need `svn relocate` when the host or port changes. Rename folders whose name differs from the URL path (e.g. `SVNParentPath /var/svn/delphi/src` for `/delphi` → `/data/repos/delphi/`), and make repository names unique across all locations first.
2. **Users**: paste the existing htpasswd contents or upload the file in **Users → Import htpasswd**. bcrypt and APR1-MD5 (`$apr1$`) hashes are supported. APR1 users can keep using SVN as is, and their hash is converted to bcrypt automatically the first time they sign in to the web UI. SHA1, crypt and plain-text entries are not imported for security reasons.
3. **Permissions**: set up groups and permission rules again in the web UI. The `authz` file is generated from the DB, so do not edit it by hand; manual edits are overwritten by the next change.

## Backups

- The **Backup** action of a repository compresses the `svnadmin dump` output with gzip and stores it in the backups volume (`<repository>-YYYYmmdd-HHMMSS-r<revision>.svndump.gz`).
- Restores load into a temporary repository and then swap it in atomically, so a failed restore never damages the existing repository.
- For a full backup, keep all four volumes (`svn-repos`, `svn-config`, `svn-db`, `svn-backups`) together.

## Security design

- **Command execution**: commands run through `exec.CommandContext` with fixed arguments and no shell. Paths and URLs are validated and escaped before being passed, with timeouts and output size limits.
- **Input validation**: repository, user and group names are checked against allow-list regular expressions. Paths inside repositories are normalized, and `..` and control characters are rejected. File access cannot leave the repository root.
- **SQL**: every query uses parameter binding.
- **XSS**: `html/template` auto-escaping and a strict CSP (`default-src 'self'`, no inline scripts or styles). No external CDNs. Translations in uploaded language files are not interpreted as HTML either.
- **CSRF**: several layers of protection.
  - Every POST after sign-in: a per-session CSRF token (form field or HTMX header) is checked.
  - Login form: a double-submit token. An HttpOnly, SameSite=Strict cookie must match the form value, and over HTTPS the `__Host-` prefix prevents cookie injection from subdomains.
  - Global: Go's standard `http.CrossOriginProtection` (based on Sec-Fetch-Site/Origin) blocks POSTs from other sites or other subdomains before any handler runs.
  - In addition, Origin/Referer checks and a SameSite=Lax session cookie.
  - Signing out fully reloads the page so no token of the previous session remains.
- **Sessions**: cookies are HttpOnly and SameSite, and Secure over HTTPS. Only the SHA-256 hash of the token is stored in the DB. Changing a password revokes that user's sessions.
- **Passwords**: stored only as bcrypt, never in plain text. Passwords and credentials are never logged.
- **Login limits**: web sign-in failures are limited per IP and per account over 15 minutes (10 by default, configurable).
- **SVN brute-force blocking**: an IP with 20 failed SVN (HTTP Basic) logins within 15 minutes is blocked for 15 minutes (count and duration are configurable in system settings; 0 disables it).
  - Apache answers blocked clients with 429 **before authentication**, so no more passwords can be tried while blocked.
  - It applies immediately without restarting Apache, and the block list can be viewed and cleared in system settings.
  - Blocks and unblocks are recorded in the audit log.
- **IP access restriction**: separate allowed IP/CIDR lists for the admin UI and SVN. Apache rejects SVN requests with 403 before authentication, and the Go server rejects every admin page and sign-in request. If a stored list is corrupt, only loopback is allowed (fail-closed). See [Restricting access by IP](#restricting-access-by-ip).
- **Trusted proxy headers**: `X-Forwarded-For` is read only on requests from proxies listed in `SVNGO_TRUSTED_PROXIES`; other requests use the connection address. Values that trust every address (`0.0.0.0/0`, `::/0`) are rejected.
- **Request size limits**: SVN request bodies and XML requests (`/svn` and SVN locations) are limited. Other paths accept up to 1 MB, and the admin pages apply per-feature limits in the Go server.
- **Mandatory authz validation**: at startup the validator is self-tested: it must accept a valid file, reject an invalid one and evaluate permissions correctly. If any check fails the server does not start, and there is no path that applies auth files without validation.
- **Security headers**: CSP, X-Frame-Options DENY, nosniff, Referrer-Policy, Permissions-Policy, COOP, HSTS (over HTTPS).
- **Container**: runs as non-root (uid 1000) with a read-only root filesystem, `cap_drop: ALL` and `no-new-privileges`. PID 1 is tini.
- **Auth files**: written to a temporary file, validated with libsvn, swapped in atomically and read back for comparison, with rollback on failure. **System settings → Auth file sync check** shows whether the DB, the file contents and the libsvn evaluation agree.

## Operations

```powershell
docker compose logs -f svngo        # logs (Apache lines are prefixed with [apache])
docker compose restart svngo        # restart (data stays in the volumes)
docker compose down                 # stop (volumes kept)
docker compose down -v              # stop and delete all data (careful!)
docker compose exec svngo svngo version
```

- The Go admin server supervises Apache. If Apache exits unexpectedly it is restarted with backoff, and on the container stop signal (SIGTERM) Apache is stopped gracefully before exiting.
- The health check (`svngo healthcheck`) requests `/admin/healthz` through Apache, so it checks Apache and the Go server together.

### Multi-architecture build

```powershell
docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/svngo:1.1.0 --push .
```

## Tests

**Unit and integration tests**

When `svn`, `svnadmin`, `svnlook` and `svnauthz` are installed on the host, the tests use the real SVN tools.

```powershell
go test -count=1 ./...
```

**Docker E2E tests**

Sends HTTP requests with the svn client to a real container. The test port is 18080.

```powershell
docker compose -p svngo-e2e -f docker-compose.yml -f e2e/compose.e2e.yml up -d --build
go test -tags e2e -count=1 -v ./e2e
docker compose -p svngo-e2e -f docker-compose.yml -f e2e/compose.e2e.yml down -v
```

The E2E tests verify the following with real `svn` commands:

1. Create a repository
2. Create a user
3. HTTP checkout
4. HTTP commit
5. HTTP update
6. Change a password
7. Reject the old password
8. Change permissions (commit rejected after rw→r, directory denied, rule deleted, group permissions)
9. Block a user without permission
10. Access a new repository without restarting Apache (PID and restart count compared)
11. Commit history and diff in the web UI
12. Data kept after `docker compose restart`
13. SVN locations: add a location, checkout/commit/server-side copy at `/<location>/<repository>`, SVN IP allow list on location paths, move between locations, delete the location (Apache PID unchanged)

## Directory layout

```
cmd/svngo/            entry point (serve, healthcheck, admin-passwd, access-reset, version commands)
internal/
  server/             admin web app: handlers, sessions and CSRF, jobs (backup/restore), DB → auth file sync
    templates/        html/template pages (use translation keys, embedded in the binary)
    static/           CSS, htmx (bundled locally), small JS (embedded in the binary)
  i18n/               languages: load, validate and install language files, browser language matching, translated messages
    locales/          built-in language files en.json and ko.json (embedded in the binary)
  apache/             httpd process management, log collection, SVN brute-force blocking, SVN allowed-IP rules, SVN location <Location> blocks
  ipacl/              parsing, normalizing and matching allowed IP/CIDR lists
  svn/                runs svnadmin, svnlook, svn and the authz validator
  authz/              permission evaluation, htpasswd and authz file generation
  store/              SQLite schema, audit log, settings
  passwd/             bcrypt and APR1 hashes, htpasswd parsing
  config/             configuration from environment variables
  validate/           validation of names, passwords and permission paths
  repopath/           paths inside repositories
  fsutil/             atomic file writes
  logring/            recent log buffer (in memory)
docker/httpd.conf     Apache configuration (mod_dav_svn, authz_svn, proxy)
docker/svngo-authz.c  libsvn-based authz validator (svnauthz compatible)
docker/DOCKERHUB.md   Docker Hub Overview (English + Korean)
docker/DOCKERHUB.short.txt  Hub one-line description (EN | KO); push with docker/update-dockerhub-overview.sh
Dockerfile            multi-stage build (Go → C helper → alpine runtime)
docker-compose.yml    production compose file
e2e/                  Docker E2E tests
docs/images/          README screenshots (Korean UI; en/ holds the English UI)
README.md             this document (English)
README.ko.md          Korean version of this document
```

## Version history

| Version | Changes |
|---|---|
| 1.1.0 | [SVN locations](#svn-locations-multiple-url-paths): extra URL paths such as `/android/<repository>` served from folders of the same name, with moving repositories between locations. Multiple languages (English and Korean built in, language files can be added). [IP access restriction](#restricting-access-by-ip) for the admin UI and SVN. The DB schema is migrated automatically. |
| 1.0.0 | First release: repository, user, group and permission management, web browsing, backup and restore, SVN brute-force blocking |

## Limitations

- `svn://` (svnserve) is not supported; only HTTP/HTTPS.
- Authentication is htpasswd (Basic auth) only. There is no LDAP/AD integration.
- The web file view has no syntax highlighting. Files over 512 KB are shown partially, and the download always contains the whole file. Diffs are shown up to 5,000 lines or 2 MB, and the full raw diff can be downloaded.
- Repository sizes are computed asynchronously and cached, so the latest commits may not be reflected right away.
- Paths in permission rules cannot contain `[ ] % \` or leading/trailing spaces (authz syntax constraint).
- It is a single-container design, so horizontal scaling (multiple instances) is not supported.
- SVN locations cannot be renamed (add a new location and move the repositories instead), and repository names must be unique across all locations. With an external Apache (`SVNGO_MANAGE_APACHE` off) the `<Location>` blocks of SVN locations must be added by hand.
- SVN login blocking is per client IP, and the block list is kept only in memory, so it resets when the container restarts.
  - When all clients appear with the same IP, as on some Docker Desktop setups, repeated failures by one person can block everyone. In that case raise the allowed count or set it to 0 in system settings, and limit attempts in the proxy instead. If a reverse proxy causes this, `SVNGO_TRUSTED_PROXIES` solves it.
- IP access restriction also uses the connection address Apache sees. `X-Forwarded-For` is used only on requests from proxies listed in `SVNGO_TRUSTED_PROXIES` ([Client IP behind a reverse proxy](#client-ip-behind-a-reverse-proxy)). If `SVNGO_MANAGE_APACHE` is turned off to use an external Apache, the SVN list and `SVNGO_TRUSTED_PROXIES` are not applied.
- On some Linux hosts (AppArmor or snap Docker), `no-new-privileges` can make every executable in the container fail with `operation not permitted`. In that case remove the `security_opt` entry from `docker-compose.yml`. Docker Desktop (Windows/macOS) is not affected.
