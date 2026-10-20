# syntax=docker/dockerfile:1.7
#
# All-in-one SoulGem image: PostgreSQL 17, a TURN relay, the API and the
# worker in one container with one data volume. It reuses the tested server
# image rather than compiling again; CI passes its local tag as a build argument.
ARG SERVER_IMAGE=ghcr.io/kirikira/gsm2sip-server:main

FROM ${SERVER_IMAGE} AS server

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

RUN --mount=type=secret,id=proxy_ca,required=false \
    set -eu; \
    if [ -s /run/secrets/proxy_ca ]; then \
      install -d /usr/local/share/ca-certificates; \
      cp /run/secrets/proxy_ca /usr/local/share/ca-certificates/build-proxy.crt; \
      echo 'Acquire::https::CAInfo "/etc/ssl/certs/ca-certificates.crt";' > /etc/apt/apt.conf.d/99build-proxy; \
    fi; \
    DEBIAN_FRONTEND=noninteractive apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl openssl coturn python3; \
    update-ca-certificates; \
    install -d /usr/share/postgresql-common/pgdg /etc/postgresql-common; \
    curl --fail --silent --show-error --location \
      https://www.postgresql.org/media/keys/ACCC4CF8.asc \
      --output /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    echo 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main' \
      > /etc/apt/sources.list.d/pgdg.list; \
    echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.conf; \
    DEBIAN_FRONTEND=noninteractive apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends postgresql-17; \
    # The packages pull in LLVM for JIT (~150 MB) and Perl for the cluster
    # wrapper scripts; this image runs the server binaries directly with JIT
    # off, so drop both and fail the build if a binary loses a library.
    rm -rf /usr/lib/postgresql/17/lib/llvmjit* /usr/lib/postgresql/17/lib/bitcode \
      /var/lib/apt/lists/* /var/lib/postgresql/17; \
    dpkg --purge --force-depends $(dpkg-query -W -f '${Package}\n' \
      'libllvm*' 'libz3-*' perl perl-modules-5.36 'libperl5*' locales 2>/dev/null); \
    for binary in postgres initdb pg_isready psql; do \
      if ldd "/usr/lib/postgresql/17/bin/$binary" | grep 'not found'; then exit 1; fi; \
    done; \
    rm -rf /usr/share/doc/* /usr/share/man/* /usr/share/locale/* /etc/apt/apt.conf.d/99build-proxy; \
    if [ -e /usr/local/share/ca-certificates/build-proxy.crt ]; then \
      rm /usr/local/share/ca-certificates/build-proxy.crt; update-ca-certificates --fresh >/dev/null; \
    fi; \
    groupadd --system --gid 10002 gsm2sip; \
    useradd --system --uid 10002 --gid 10002 --home-dir /nonexistent \
      --no-create-home --shell /usr/sbin/nologin gsm2sip; \
    install -d -o postgres -g postgres /run/postgresql; \
    install -d -m 0755 /data /usr/local/libexec

COPY --from=server /usr/local/bin/gsm2sip-api /usr/local/bin/gsm2sip-worker /usr/local/libexec/
COPY --from=server /usr/local/bin/gsm2sip-admin /usr/local/libexec/gsm2sip-admin
COPY --from=server /app/migrations/ /app/migrations/
COPY deploy/allinone/turn-config.py /usr/local/libexec/gsm2sip-turn-config.py
COPY deploy/allinone/run.sh /usr/local/bin/gsm2sip-allinone
COPY deploy/allinone/admin.sh /usr/local/bin/gsm2sip-admin
COPY deploy/allinone/healthcheck.sh /usr/local/bin/gsm2sip-allinone-healthcheck
RUN chmod 0755 /usr/local/bin/gsm2sip-allinone /usr/local/bin/gsm2sip-admin \
      /usr/local/bin/gsm2sip-allinone-healthcheck

ENV MIGRATIONS_DIR=/app/migrations

VOLUME ["/data"]
EXPOSE 8080/tcp 3478/tcp 3478/udp 49160-49259/udp
ENTRYPOINT ["/usr/local/bin/gsm2sip-allinone"]
CMD []
HEALTHCHECK --interval=15s --timeout=5s --start-period=60s --retries=4 \
  CMD ["/usr/local/bin/gsm2sip-allinone-healthcheck"]
