# syntax=docker/dockerfile:1
# SvnGo: Go admin server + Apache httpd + mod_dav_svn in one small image.

ARG ALPINE_VERSION=3.22
ARG GO_VERSION=1.26

# ---- Go admin server (cross-compiled on the build platform) ----
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
ARG VERSION=1.1.0
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X svngo/internal/server.Version=${VERSION}" -o /out/svngo ./cmd/svngo

# ---- svngo-authz: authz validator linked against libsvn_repos ----
FROM alpine:${ALPINE_VERSION} AS cbuild
RUN apk add --no-cache build-base subversion-dev apr-dev apr-util-dev
COPY docker/svngo-authz.c /src/
RUN gcc -O2 -Wall -Werror -o /out-svngo-authz /src/svngo-authz.c \
      -I/usr/include/subversion-1 $(apr-1-config --cppflags --includes) \
      -lsvn_repos-1 -lsvn_subr-1 $(apr-1-config --link-ld) \
 && strip /out-svngo-authz

# ---- runtime ----
FROM alpine:${ALPINE_VERSION}
ARG UID=1000
ARG GID=1000
RUN apk add --no-cache apache2 apache2-proxy apache2-ssl apache2-webdav mod_dav_svn subversion \
      tini tzdata ca-certificates \
 && rm -rf /etc/apache2/conf.d/* /var/www/localhost \
 && addgroup -g ${GID} -S svngo \
 && adduser -u ${UID} -G svngo -S -D -H -h /data -s /sbin/nologin svngo \
 && mkdir -p /data/repos /data/config /data/db /data/backups /run/svngo \
 && chown -R svngo:svngo /data /run/svngo \
 && chmod 750 /data/repos /data/config /data/db /data/backups

COPY --from=gobuild /out/svngo /usr/local/bin/svngo
COPY --from=cbuild /out-svngo-authz /usr/local/bin/svngo-authz
COPY docker/httpd.conf /etc/svngo/httpd.conf

ENV SVNGO_MANAGE_APACHE=1 \
    SVNGO_HTTP_PORT=8080 \
    SVNGO_HTTPS_PORT=8443 \
    SVNGO_LISTEN=127.0.0.1:9000 \
    SVNGO_RUN_DIR=/run/svngo \
    SVNGO_REPOS_DIR=/data/repos \
    SVNGO_CONFIG_DIR=/data/config \
    SVNGO_DB_DIR=/data/db \
    SVNGO_BACKUP_DIR=/data/backups \
    LANG=C.UTF-8 \
    TZ=Asia/Seoul

USER svngo
WORKDIR /data
EXPOSE 8080 8443
VOLUME ["/data/repos", "/data/config", "/data/db", "/data/backups"]
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/usr/local/bin/svngo", "healthcheck"]
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/svngo"]
CMD ["serve"]
