# syntax=docker/dockerfile:1.7
#
# Build the Asterisk 22.11.0 upstream release archive and verify its pinned
# SHA-256. PJProject is pinned to an upstream 2.17-dev commit rather than the
# older snapshot bundled with this Asterisk release.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS build

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      autoconf binutils build-essential ca-certificates curl file libcurl4-openssl-dev \
      libedit-dev libjansson-dev libncurses5-dev libnewt-dev libopus-dev libpq-dev \
      libsqlite3-dev libspeexdsp-dev libssl-dev libtool libxml2-dev libxslt1-dev \
      libsrtp2-dev pkg-config uuid-dev && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /usr/src
RUN --mount=type=secret,id=proxy_ca,required=false \
    set -eu; \
    if [ -s /run/secrets/proxy_ca ]; then \
      cat /etc/ssl/certs/ca-certificates.crt /run/secrets/proxy_ca > /tmp/session-ca-bundle.crt; \
      export CURL_CA_BUNDLE=/tmp/session-ca-bundle.crt; \
    fi; \
    curl --fail --location --silent --show-error \
      "https://downloads.asterisk.org/pub/telephony/asterisk/releases/asterisk-22.11.0.tar.gz" \
      --output asterisk.tar.gz; \
    printf '%s  %s\n' "3bd5ee040509a3d3cd9b1ba9520c18e6ec0a7e7981ca68c457dcd36ba3c54d94" asterisk.tar.gz | sha256sum --check --status; \
    curl --fail --location --silent --show-error \
      "https://github.com/pjsip/pjproject/archive/a67b8e81b0024b993f47e463c01c67c25cda116f.tar.gz" \
      --output pjproject.tar.gz; \
    printf '%s  %s\n' "86b7bb049ad33b27f487d057d92f217717c40c1bcb4233c0e8db9f964d34cbc5" pjproject.tar.gz | sha256sum --check --status; \
    curl --fail --location --silent --show-error \
      "https://github.com/traud/asterisk-opus/archive/a959f072d3f364be983dd27e6e250b038aaef747.tar.gz" \
      --output asterisk-opus.tar.gz; \
    printf '%s  %s\n' "52404693d102a577e27f6df03cb8d7c2425973372aceb193d8aff13453ec6076" asterisk-opus.tar.gz | sha256sum --check --status; \
    mkdir -p /usr/src/pjproject /usr/src/asterisk-opus; \
    tar -xzf pjproject.tar.gz -C /usr/src/pjproject --strip-components=1; \
    tar -xzf asterisk-opus.tar.gz -C /usr/src/asterisk-opus --strip-components=1; \
    tar -xzf asterisk.tar.gz --strip-components=1 -C /usr/src; \
    rm -f asterisk.tar.gz pjproject.tar.gz asterisk-opus.tar.gz /tmp/session-ca-bundle.crt

WORKDIR /usr/src/pjproject
# Build the pinned pjproject the way Asterisk builds its bundled copy: with
# Asterisk's config_site.h (NDEBUG, so pjlib's debug assertions such as
# "Calling pjlib from unknown/external thread" cannot abort Asterisk, plus
# its transaction, hash and SDP limits) and the same configure options. The
# malloc-debug shim only exists in the bundled build and is left out.
RUN set -eu; \
    asterisk_pj=/usr/src/third-party/pjproject; \
    sed '/_ASTERISK_ASTMM_H/,/#endif/d' "$asterisk_pj/patches/config_site.h" \
      > pjlib/include/pj/config_site.h; \
    grep -q '^#define NDEBUG 1' pjlib/include/pj/config_site.h; \
    if grep -q '#include "asterisk_malloc_debug.h"' pjlib/include/pj/config_site.h; then exit 1; fi; \
    command -v patch >/dev/null; \
    retrans_patch=$(echo "$asterisk_pj"/patches/0020-*.patch); \
    test -f "$retrans_patch"; \
    if ! grep -q PJSIP_INV_ABSORB_RETRANS_AFTER_ACK pjsip/include/pjsip/sip_config.h; then \
      if patch -p1 --forward --dry-run < "$retrans_patch" >/dev/null; then \
        patch -p1 --forward < "$retrans_patch"; \
      else \
        echo "pjproject predates the INVITE retransmission change; legacy behaviour already applies"; \
      fi; \
    fi; \
    ./aconfigure \
      --prefix=/usr/local \
      --enable-shared \
      --enable-epoll \
      --disable-speex-codec --disable-speex-aec --disable-bcg729 --disable-gsm-codec \
      --disable-ilbc-codec --disable-l16-codec --disable-g722-codec --disable-g7221-codec \
      --disable-opencore-amr --disable-silk --disable-opus --disable-video --disable-v4l2 \
      --disable-sound --disable-ext-sound --disable-sdl --disable-libyuv --disable-ffmpeg \
      --disable-openh264 --disable-ipp --disable-libwebrtc --disable-libsrtp --disable-upnp \
      --disable-resample --disable-g711-codec \
      --without-external-pa --without-external-srtp \
      CFLAGS="-O2 -DPJ_HAS_LINUX_EPOLL=1" && \
    make dep && \
    make -j2 && \
    make install && \
    ldconfig

WORKDIR /usr/src
# BUILD_NATIVE (on by default) compiles with -march=native, so a binary built
# on a CI runner with a newer CPU dies with SIGILL on an older home server.
RUN cd /usr/src && ./configure \
      --prefix=/usr \
      --sysconfdir=/etc \
      --localstatedir=/var \
      --with-pjproject-bundled=no \
      --with-pjproject=/usr/local \
      --with-srtp && \
    make menuselect.makeopts && \
    menuselect/menuselect \
      --enable app_dial \
      --enable app_mixmonitor \
      --enable format_wav \
      --enable app_stack \
      --enable app_stasis \
      --enable chan_pjsip \
      --enable codec_alaw \
      --enable codec_g722 \
      --enable codec_ulaw \
      --enable res_ari \
      --enable res_ari_applications \
      --enable res_ari_channels \
      --enable res_config_pgsql \
      --enable res_format_attr_opus \
      --enable res_http_websocket \
      --enable res_pjsip \
      --enable res_pjsip_authenticator_digest \
      --enable res_pjsip_caller_id \
      --enable res_pjsip_endpoint_identifier_user \
      --enable res_pjsip_history \
      --enable res_srtp \
      --enable pbx_config \
      --disable BUILD_NATIVE \
      menuselect.makeopts && \
    if grep -Eq '^MENUSELECT_CFLAGS=.*BUILD_NATIVE' menuselect.makeopts; then \
      echo 'BUILD_NATIVE would tie the image to the build machine CPU' >&2; exit 1; \
    fi && \
    make -j2 && \
    make install && \
    if [ "$(uname -m)" = x86_64 ]; then \
      for binary in /usr/sbin/asterisk /usr/lib/asterisk/modules/*.so /usr/local/lib/libpj*.so; do \
        if objdump -d "$binary" | grep -Eq '%[yz]mm[0-9]'; then \
          echo "$binary uses AVX instructions that older CPUs lack" >&2; exit 1; \
        fi; \
      done; \
    fi

# This GPL translator is maintained outside the Asterisk source tree.  Keep
# the exact source commit pinned and build against the Asterisk 22 headers.
RUN make -C /usr/src/asterisk-opus CPATH=-I/usr/src/include \
      'LIBS=-Wl,--no-as-needed -lopus' codec_opus_open_source && \
    install -m 0644 /usr/src/asterisk-opus/codecs/codec_opus_open_source.so \
      /usr/lib/asterisk/modules/codec_opus_open_source.so && \
    strip --strip-unneeded /usr/lib/asterisk/modules/codec_opus_open_source.so && \
    objdump -p /usr/lib/asterisk/modules/codec_opus_open_source.so | \
      grep -F 'NEEDED               libopus.so.0' >/dev/null

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS runtime

RUN --mount=type=bind,from=build,source=/usr/sbin,target=/mnt/build/sbin,ro \
    --mount=type=bind,from=build,source=/usr/lib,target=/mnt/build/lib,ro \
    --mount=type=bind,from=build,source=/usr/lib/asterisk/modules,target=/mnt/build/modules,ro \
    --mount=type=bind,from=build,source=/usr/local,target=/mnt/build/local,ro \
    --mount=type=bind,from=build,source=/var/lib/asterisk,target=/mnt/build/varlib,ro \
    --mount=type=bind,from=build,source=/usr/src/configs/samples,target=/mnt/build/samples,ro \
    --mount=type=bind,source=deploy/asterisk,target=/mnt/asterisk-source,ro \
    set -eu; \
    DEBIAN_FRONTEND=noninteractive apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl gettext-base libedit2 libjansson4 libopus0 libpq5 \
      libsqlite3-0 libssl3 libxml2 libxslt1.1 libsrtp2-1 libuuid1 util-linux; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --system --gid 10001 asterisk; \
    useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/asterisk \
      --no-create-home --shell /usr/sbin/nologin asterisk; \
    install -d -o asterisk -g asterisk /etc/asterisk /run/asterisk \
      /var/lib/asterisk /var/log/asterisk /var/spool/asterisk; \
    install -d /usr/lib/asterisk /usr/local/lib/pkgconfig \
      /opt/gsm2sip/asterisk/templates /usr/local/bin; \
    install -m 0755 /mnt/build/sbin/asterisk /usr/sbin/asterisk; \
    cp -a /mnt/build/lib/libasteriskssl.so* /usr/lib/; \
    # Ship only the modules modules.conf loads (autoload = no), not all ~300.
    install -d /usr/lib/asterisk/modules; \
    for module in $(sed -n 's/^\(require\|load\) *=> *\([a-z0-9_]*\.so\).*/\2/p' \
        /mnt/asterisk-source/templates/modules.conf); do \
      install -m 0755 "/mnt/build/modules/$module" /usr/lib/asterisk/modules/; \
    done; \
    cp -a /mnt/build/varlib/. /var/lib/asterisk/; \
    # Music on hold, phone provisioning, firmware and static web pages are unused.
    rm -rf /var/lib/asterisk/moh /var/lib/asterisk/phoneprov /var/lib/asterisk/firmware \
      /var/lib/asterisk/images /var/lib/asterisk/static-http; \
    cp -a /mnt/build/local/lib/*.so* /usr/local/lib/; \
    install -m 0644 /mnt/build/local/lib/pkgconfig/libpjproject.pc \
      /usr/local/lib/pkgconfig/libpjproject.pc; \
    install -m 0644 /mnt/build/samples/indications.conf.sample /etc/asterisk/indications.conf; \
    install -m 0644 /mnt/build/samples/features.conf.sample /etc/asterisk/features.conf; \
    install -m 0644 /mnt/build/samples/ccss.conf.sample /etc/asterisk/ccss.conf; \
    install -m 0644 /mnt/build/samples/phoneprov.conf.sample /etc/asterisk/phoneprov.conf; \
    cp -a /mnt/asterisk-source/templates/. /opt/gsm2sip/asterisk/templates/; \
    install -m 0755 /mnt/asterisk-source/entrypoint.sh \
      /usr/local/bin/gsm2sip-asterisk-entrypoint; \
    install -m 0755 /mnt/asterisk-source/healthcheck.sh \
      /usr/local/bin/gsm2sip-asterisk-healthcheck; \
    chown -R asterisk:asterisk /var/lib/asterisk; \
    ldconfig; \
    missing=$(ldd /usr/sbin/asterisk | awk '/not found/ {print $1}'); \
    if [ -n "$missing" ]; then \
      printf 'Asterisk runtime is missing shared libraries: %s\n' "$missing" >&2; \
      exit 1; \
    fi; \
    for module in /usr/lib/asterisk/modules/*.so; do \
      missing=$(ldd "$module" 2>/dev/null | awk '/not found/ {print $1}'); \
      if [ -n "$missing" ]; then \
        printf 'Module %s is missing shared libraries: %s\n' \
          "${module##*/}" "$missing" >&2; \
        exit 1; \
      fi; \
    done; \
    true

ENV ARI_USERNAME=gsm2sip \
    ASTERISK_ARI_BIND_ADDR=0.0.0.0 \
    POSTGRES_DB=gsm2sip \
    POSTGRES_HOST=postgres \
    POSTGRES_PORT=5432 \
    POSTGRES_USER=gsm2sip \
    RTP_START=10000 \
    RTP_END=10199

EXPOSE 5061/tcp 5060/udp 10000-10199/udp
EXPOSE 8088/tcp
ENTRYPOINT ["/usr/local/bin/gsm2sip-asterisk-entrypoint"]
CMD ["/usr/sbin/asterisk", "-f"]
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 \
  CMD ["/usr/local/bin/gsm2sip-asterisk-healthcheck"]
