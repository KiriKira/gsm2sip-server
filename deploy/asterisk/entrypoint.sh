#!/bin/sh
set -eu

: "${ARI_USERNAME:?ARI_USERNAME is required}"
: "${ARI_PASSWORD:?ARI_PASSWORD is required}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
: "${ASTERISK_EXTERNAL_MEDIA_ADDRESS:?ASTERISK_EXTERNAL_MEDIA_ADDRESS is required}"
: "${ASTERISK_EXTERNAL_SIGNALING_ADDRESS:?ASTERISK_EXTERNAL_SIGNALING_ADDRESS is required}"

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
render http.conf '${ASTERISK_ARI_BIND_ADDR}'
render pjsip.conf '${ASTERISK_EXTERNAL_MEDIA_ADDRESS} ${ASTERISK_EXTERNAL_SIGNALING_ADDRESS}'
render res_pgsql.conf '${POSTGRES_DB} ${POSTGRES_HOST} ${POSTGRES_PORT} ${POSTGRES_USER} ${POSTGRES_PASSWORD}'
render rtp.conf '${RTP_END} ${RTP_START}'
cp /opt/gsm2sip/asterisk/templates/extensions.conf "$ASTERISK_CONFIG_DIR/extensions.conf"
cp /opt/gsm2sip/asterisk/templates/extconfig.conf "$ASTERISK_CONFIG_DIR/extconfig.conf"
cp /opt/gsm2sip/asterisk/templates/logger.conf "$ASTERISK_CONFIG_DIR/logger.conf"
cp /opt/gsm2sip/asterisk/templates/modules.conf "$ASTERISK_CONFIG_DIR/modules.conf"
cp /opt/gsm2sip/asterisk/templates/sorcery.conf "$ASTERISK_CONFIG_DIR/sorcery.conf"
chown -R asterisk:asterisk "$ASTERISK_CONFIG_DIR"

unset ARI_PASSWORD POSTGRES_PASSWORD
exec setpriv --reuid=asterisk --regid=asterisk --init-groups \
    /usr/sbin/asterisk -f -C "$ASTERISK_CONFIG_DIR/asterisk.conf"
