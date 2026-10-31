#!/usr/bin/env bash
# Docker Hub isul/svngo Overview(full_description)를 docker/DOCKERHUB.md 내용으로 갱신함.
# 사용: DOCKERHUB_USER=isul DOCKERHUB_TOKEN=... ./docker/update-dockerhub-overview.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MD="$ROOT/docker/DOCKERHUB.md"
NS="${DOCKERHUB_NAMESPACE:-isul}"
REPO="${DOCKERHUB_REPO:-svngo}"
USER="${DOCKERHUB_USER:-}"
TOKEN="${DOCKERHUB_TOKEN:-${DOCKERHUB_PASSWORD:-}}"

if [[ ! -f "$MD" ]]; then
  echo "오류: $MD 없음" >&2
  exit 1
fi
if [[ -z "$USER" || -z "$TOKEN" ]]; then
  echo "오류: DOCKERHUB_USER 와 DOCKERHUB_TOKEN(또는 DOCKERHUB_PASSWORD) 환경 변수가 필요합니다." >&2
  echo "  Hub → Account Settings → Security → New Access Token" >&2
  exit 2
fi

SHORT_FILE="$ROOT/docker/DOCKERHUB.short.txt"
if [[ -z "${DOCKERHUB_SHORT_DESCRIPTION:-}" && -f "$SHORT_FILE" ]]; then
  SHORT="$(tr -d '\n\r' < "$SHORT_FILE" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
else
  SHORT="${DOCKERHUB_SHORT_DESCRIPTION:-Lightweight SVN server: Apache mod_dav_svn + Go web admin UI, single container | 경량 SVN 서버: Apache mod_dav_svn + Go 웹 관리 UI, 단일 컨테이너}"
fi

export MD SHORT USER TOKEN NS REPO
python3 <<'PY'
import json
import os
import urllib.request

md_path = os.environ["MD"]
user = os.environ["USER"]
token = os.environ["TOKEN"]
ns = os.environ["NS"]
repo = os.environ["REPO"]
short = os.environ["SHORT"].strip()

full_text = open(md_path, encoding="utf-8").read()

login_req = urllib.request.Request(
    "https://hub.docker.com/v2/users/login/",
    data=json.dumps({"username": user, "password": token}).encode(),
    headers={"Content-Type": "application/json"},
    method="POST",
)
with urllib.request.urlopen(login_req) as resp:
    hub_token = json.load(resp)["token"]

patch_body = json.dumps(
    {"full_description": full_text, "description": short},
    ensure_ascii=False,
).encode("utf-8")

patch_req = urllib.request.Request(
    f"https://hub.docker.com/v2/repositories/{ns}/{repo}/",
    data=patch_body,
    headers={
        "Authorization": f"Bearer {hub_token}",
        "Content-Type": "application/json",
    },
    method="PATCH",
)
with urllib.request.urlopen(patch_req) as resp:
    resp.read()

print(f"OK: https://hub.docker.com/r/{ns}/{repo} Overview 갱신됨 ({len(full_text.encode('utf-8'))} bytes)")
PY
