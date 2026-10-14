# syntax=docker/dockerfile:1.7
#
# All-in-one image: PostgreSQL 17, Asterisk, the API and the worker in one
# container with one data volume. It reuses the two tested images rather
# than compiling again; CI passes their local tags as build arguments.
ARG SERVER_IMAGE=ghcr.io/kirikira/gsm2sip-server:main
ARG ASTERISK_IMAGE=ghcr.io/kirikira/gsm2sip-asterisk:main

FROM ${SERVER_IMAGE} AS server

FROM ${ASTERISK_IMAGE}

RUN set -eu; \
    DEBIAN_FRONTEND=noninteractive apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssl; \
    install -d /usr/share/postgresql-common/pgdg /etc/postgresql-common; \
    curl --fail --silent --show-error --location \
      https://www.postgresql.org/media/keys/ACCC4CF8.asc \
      --output /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    echo 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main' \
      > /etc/apt/sources.list.d/pgdg.list; \
    echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.conf; \
    DEBIAN_FRONTEND=noninteractive apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends postgresql-17; \
    rm -rf /var/lib/apt/lists/* /var/lib/postgresql/17; \
    groupadd --system --gid 10002 gsm2sip; \
    useradd --system --uid 10002 --gid 10002 --home-dir /nonexistent \
      --no-create-home --shell /usr/sbin/nologin gsm2sip; \
    install -d -o postgres -g postgres /run/postgresql; \
    install -d -m 0755 /data /usr/local/libexec

COPY --from=server /usr/local/bin/gsm2sip-api /usr/local/bin/gsm2sip-worker /usr/local/libexec/
COPY --from=server /usr/local/bin/gsm2sip-admin /usr/local/libexec/gsm2sip-admin
COPY --from=server /app/migrations/ /app/migrations/
COPY deploy/allinone/run.sh /usr/local/bin/gsm2sip-allinone
COPY deploy/allinone/admin.sh /usr/local/bin/gsm2sip-admin
COPY deploy/allinone/healthcheck.sh /usr/local/bin/gsm2sip-allinone-healthcheck
RUN chmod 0755 /usr/local/bin/gsm2sip-allinone /usr/local/bin/gsm2sip-admin \
      /usr/local/bin/gsm2sip-allinone-healthcheck

ENV MIGRATIONS_DIR=/app/migrations \
    HTTP_ADDR=:8080 \
    SIP_PORT=5061 \
    RTP_START=10000 \
    RTP_END=10199

VOLUME ["/data"]
EXPOSE 8080/tcp 5061/tcp 10000-10199/udp
ENTRYPOINT ["/usr/local/bin/gsm2sip-allinone"]
CMD []
HEALTHCHECK --interval=15s --timeout=5s --start-period=60s --retries=4 \
  CMD ["/usr/local/bin/gsm2sip-allinone-healthcheck"]
