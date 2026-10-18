package calls

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// RenewExternalMedia keeps only this participant's SIP control channel alive
// while its authorized WebRTC leg is polling. Asterisk's normal RTP watchdog
// resumes when this short lease expires. It never changes endpoint defaults.
// The HTTP caller must first reauthorize the exact RTC session and leg.
func (m *Manager) RenewExternalMedia(ctx context.Context, ownerID, deviceID, callID string) error {
	if !m.Configured() {
		return ErrNotConfigured
	}
	var channelID string
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(CASE WHEN c.gateway_id=$2 THEN c.gateway_channel_id ELSE c.client_channel_id END,'')
		FROM call_sessions c JOIN rtc_media_sessions r ON r.call_id=c.call_id
		WHERE c.call_id=$3 AND c.owner_id=$1 AND c.state IN ('reserved','ringing','dialing','connecting','active')
		AND (c.gateway_id=$2 OR r.client_device_id=$2)
		AND (c.direction='outgoing' AND r.client_device_id=c.client_device_id
		     OR c.direction='incoming' AND r.client_device_id=c.incoming_winner_client_device_id)
		LIMIT 1`, ownerID, deviceID, callID).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil || channelID == "" {
		// The SIP leg may not exist yet during admission/negotiation.
		return err
	}
	return m.ari.SetChannelVariable(ctx, channelID, "GSM2SIP_RTC_UNTIL", strconv.FormatInt(time.Now().Add(12*time.Second).Unix(), 10))
}
