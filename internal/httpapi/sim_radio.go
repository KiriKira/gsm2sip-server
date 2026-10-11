package httpapi

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var networkTypePattern = regexp.MustCompile(`^[A-Za-z0-9_+ -]{1,16}$`)

// validSIMRadio checks optional per-SIM display telemetry. Missing fields are
// allowed because Android may withhold any of them; present fields must be
// bounded and printable so they can be shown on another device verbatim.
func validSIMRadio(radio *SIMRadio) bool {
	if radio == nil {
		return true
	}
	if radio.SignalLevel != nil && (*radio.SignalLevel < 0 || *radio.SignalLevel > 4) {
		return false
	}
	if radio.NetworkType != nil && !networkTypePattern.MatchString(*radio.NetworkType) {
		return false
	}
	if radio.NetworkOperator != nil {
		value := *radio.NetworkOperator
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 64 {
			return false
		}
		for _, r := range value {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

// storeSIMRadio records the latest radio observation for a SIM in the
// heartbeat snapshot. A SIM omitted from the snapshot has no current radio,
// so its old observation is cleared; a reported SIM without radio data (an
// older gateway) keeps the previous value, whose timestamp shows its age.
func storeSIMRadio(ctx context.Context, tx *sql.Tx, simID string, radio *SIMRadio, present bool) error {
	if !present {
		_, err := tx.ExecContext(ctx, `
			UPDATE sim_bindings SET signal_level=NULL,network_type=NULL,network_operator=NULL,
			roaming=NULL,radio_updated_at=NULL WHERE sim_id=$1 AND radio_updated_at IS NOT NULL`, simID)
		return err
	}
	if radio == nil {
		return nil
	}
	var operator *string
	if radio.NetworkOperator != nil {
		trimmed := strings.TrimSpace(*radio.NetworkOperator)
		operator = &trimmed
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE sim_bindings SET signal_level=$1,network_type=$2,network_operator=$3,
		roaming=$4,radio_updated_at=now() WHERE sim_id=$5`,
		radio.SignalLevel, radio.NetworkType, operator, radio.Roaming, simID)
	return err
}
