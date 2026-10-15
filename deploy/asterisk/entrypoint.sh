#!/bin/sh
set -eu

: "${ARI_USERNAME:?ARI_USERNAME is required}"
: "${ARI_PASSWORD:?ARI_PASSWORD is required}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
# Optional: with neither external address set, Asterisk advertises the local
# address of the interface that reaches each peer (host networking).
ASTERISK_EXTERNAL_MEDIA_ADDRESS="${ASTERISK_EXTERNAL_MEDIA_ADDRESS:-}"
ASTERISK_EXTERNAL_SIGNALING_ADDRESS="${ASTERISK_EXTERNAL_SIGNALING_ADDRESS:-}"
ASTERISK_ARI_PORT="${ASTERISK_ARI_PORT:-8088}"
export ASTERISK_ARI_PORT

SIP_TRANSPORT="$(printf '%s' "${SIP_TRANSPORT:-tls}" | tr '[:upper:]' '[:lower:]')"
case "$SIP_TRANSPORT" in
    tls) SIP_PORT="${SIP_PORT:-5061}" ;;
    udp) SIP_PORT="${SIP_PORT:-5060}" ;;
    *) echo 'SIP_TRANSPORT must be tls or udp.' >&2; exit 1 ;;
esac
export SIP_PORT

if [ "$SIP_TRANSPORT" = tls ]; then
    test -s /run/secrets/sip_tls_cert || {
        echo 'Asterisk SIP certificate is missing or empty.' >&2
        exit 1
    }
    test -s /run/secrets/sip_tls_key || {
        echo 'Asterisk SIP private key is missing or empty.' >&2
        exit 1
    }

    # Compose's file-backed secrets are bind mounts and may keep host ownership and
    # mode. Copy them to a private, Asterisk-readable runtime directory before
    # dropping privileges; never make the private key world-readable.
    install -o asterisk -g asterisk -m 0444 /run/secrets/sip_tls_cert /run/asterisk/sip_tls_cert
    install -o asterisk -g asterisk -m 0400 /run/secrets/sip_tls_key /run/asterisk/sip_tls_key
fi

umask 077
ASTERISK_CONFIG_DIR="$(mktemp -d /tmp/gsm2sip-asterisk.XXXXXX)"
export ASTERISK_CONFIG_DIR

render() {
    template="$1"
    variables="$2"
    envsubst "$variables" \
        < "/opt/gsm2sip/asterisk/templates/${template}.template" \
        > "${ASTERISK_CONFIG_DIR}/${template}"
}

render asterisk.conf '${ASTERISK_CONFIG_DIR}'
render ari.conf '${ARI_USERNAME} ${ARI_PASSWORD}'
render http.conf '${ASTERISK_ARI_BIND_ADDR} ${ASTERISK_ARI_PORT}'
render pjsip.conf ''
envsubst '${SIP_PORT}' \
    < "/opt/gsm2sip/asterisk/templates/pjsip-transport-${SIP_TRANSPORT}.conf.template" \
    >> "${ASTERISK_CONFIG_DIR}/pjsip.conf"
if [ -n "$ASTERISK_EXTERNAL_SIGNALING_ADDRESS" ]; then
    printf 'external_signaling_address = %s\n' "$ASTERISK_EXTERNAL_SIGNALING_ADDRESS" >> "${ASTERISK_CONFIG_DIR}/pjsip.conf"
fi
if [ -n "$ASTERISK_EXTERNAL_MEDIA_ADDRESS" ]; then
    printf 'external_media_address = %s\n' "$ASTERISK_EXTERNAL_MEDIA_ADDRESS" >> "${ASTERISK_CONFIG_DIR}/pjsip.conf"
fi
render res_pgsql.conf '${POSTGRES_DB} ${POSTGRES_HOST} ${POSTGRES_PORT} ${POSTGRES_USER} ${POSTGRES_PASSWORD}'
render rtp.conf '${RTP_END} ${RTP_START}'
cp /opt/gsm2sip/asterisk/templates/extensions.conf "$ASTERISK_CONFIG_DIR/extensions.conf"
cp /opt/gsm2sip/asterisk/templates/extconfig.conf "$ASTERISK_CONFIG_DIR/extconfig.conf"
cp /opt/gsm2sip/asterisk/templates/logger.conf "$ASTERISK_CONFIG_DIR/logger.conf"
cp /opt/gsm2sip/asterisk/templates/modules.conf "$ASTERISK_CONFIG_DIR/modules.conf"
cp /opt/gsm2sip/asterisk/templates/sorcery.conf "$ASTERISK_CONFIG_DIR/sorcery.conf"
chown -R asterisk:asterisk "$ASTERISK_CONFIG_DIR"
# Point the default config path at this run's config so `asterisk -rx ...`
# from `docker exec` finds the control socket without a -C flag or warning.
ln -sfn "$ASTERISK_CONFIG_DIR/asterisk.conf" /etc/asterisk/asterisk.conf 2>/dev/null || true

unset ARI_PASSWORD POSTGRES_PASSWORD
exec setpriv --reuid=asterisk --regid=asterisk --init-groups \
    /usr/sbin/asterisk -f -C "$ASTERISK_CONFIG_DIR/asterisk.conf"
