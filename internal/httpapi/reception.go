package httpapi

// Reception v2: a client's lasting choice, per node, to be offered incoming
// calls and to get SMS wake-ups. Unlike the call-control capability it does
// not lapse when the phone sleeps or a push fails; it changes only when the
// client writes it.
import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

type receptionConfig struct {
	Version         int   `json:"version"`
	IncomingEnabled bool  `json:"incoming_enabled"`
	SMSEnabled      bool  `json:"sms_enabled"`
	Revision        int64 `json:"revision"`
}

type receptionUpdate struct {
	Version          int    `json:"version"`
	ExpectedRevision *int64 `json:"expected_revision"`
	IncomingEnabled  *bool  `json:"incoming_enabled"`
	SMSEnabled       *bool  `json:"sms_enabled"`
}

// receptionCandidateSQL selects the clients an incoming call rings: a v2
// client while it keeps incoming calls on, any other client by the
// call-control capability rules. d aliases devices, r device_reception and c
// rtc_control_capabilities, both LEFT JOINed.
const receptionCandidateSQL = `CASE WHEN r.protocol_version=2 THEN r.incoming_enabled ELSE COALESCE(` + rtcCandidateSQL + `,false) END`

// readReception returns the stored configuration, or for a device that never
// wrote one the version 1 view: incoming follows its call-control capability.
func readReception(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, deviceID string, lock bool) (receptionConfig, error) {
	var out receptionConfig
	query := `SELECT protocol_version,incoming_enabled,sms_enabled,revision FROM device_reception WHERE device_id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	err := q.QueryRowContext(ctx, query, deviceID).Scan(&out.Version, &out.IncomingEnabled, &out.SMSEnabled, &out.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		out = receptionConfig{Version: 1, SMSEnabled: true}
		err = q.QueryRowContext(ctx, `SELECT COALESCE((SELECT enabled FROM rtc_control_capabilities WHERE device_id=$1),false)`, deviceID).Scan(&out.IncomingEnabled)
	}
	return out, err
}

func (s *Server) getReception(w http.ResponseWriter, r *http.Request, p Principal) {
	if p.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive calls and messages.", false)
		return
	}
	out, err := readReception(r.Context(), s.db, p.DeviceID, false)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// putReception replaces the calling client's configuration if it still has
// expected_revision (0 before the first write). A 409 means another write
// came first: the client reads the current value and writes its latest choice
// again. Turning incoming calls off withdraws this device from calls still
// ringing; a call it already began to accept goes on.
func (s *Server) putReception(w http.ResponseWriter, r *http.Request, p Principal) {
	if p.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive calls and messages.", false)
		return
	}
	if !s.requireRTCControl(w) {
		return
	}
	var body receptionUpdate
	if !decodeOrError(w, r, &body, 1024) {
		return
	}
	if (body.Version != 1 && body.Version != 2) || body.ExpectedRevision == nil || *body.ExpectedRevision < 0 || body.IncomingEnabled == nil || body.SMSEnabled == nil {
		writeError(w, http.StatusBadRequest, "INVALID_RECEPTION", "version, expected_revision, incoming_enabled and sms_enabled are required.", false)
		return
	}
	ctx := r.Context()
	tx, err := beginDurable(ctx, s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	// Device before calls, the order call control locks in. The exclusive
	// lock waits for incoming calls being created, which share-lock the
	// devices they ring: a call either sees the new setting or is already
	// committed and withdrawn below.
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT state='active' FROM devices WHERE id=$1 AND owner_id=$2 FOR NO KEY UPDATE`, p.DeviceID, p.OwnerID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !active {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Authentication is required.", false)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	current, err := readReception(ctx, tx, p.DeviceID, true)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if current.Revision != *body.ExpectedRevision {
		writeError(w, http.StatusConflict, "RECEPTION_REVISION_CONFLICT", "The reception settings changed; read them and write again.", false)
		return
	}
	out := receptionConfig{Version: body.Version, IncomingEnabled: *body.IncomingEnabled, SMSEnabled: *body.SMSEnabled, Revision: current.Revision + 1}
	if _, err = tx.ExecContext(ctx, `INSERT INTO device_reception(device_id,protocol_version,incoming_enabled,sms_enabled,revision) VALUES($1,$2,$3,$4,$5)
      ON CONFLICT(device_id) DO UPDATE SET protocol_version=EXCLUDED.protocol_version,incoming_enabled=EXCLUDED.incoming_enabled,
        sms_enabled=EXCLUDED.sms_enabled,revision=EXCLUDED.revision,updated_at=now()`,
		p.DeviceID, out.Version, out.IncomingEnabled, out.SMSEnabled, out.Revision); err != nil {
		writeDBUnavailable(w)
		return
	}
	if !out.IncomingEnabled {
		if err = withdrawRingingParticipant(ctx, tx, p.DeviceID); err != nil {
			writeCallError(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	s.logger.Info("reception settings changed", "version", out.Version, "incoming", out.IncomingEnabled, "sms", out.SMSEnabled, "revision", out.Revision)
	s.kickWake()
	writeJSON(w, http.StatusOK, out)
}

// withdrawRingingParticipant ends deviceID's part in incoming calls that only
// ring it. Other devices keep ringing, and the cellular call and its slot are
// left alone: turning reception off never hangs a call up. A call this device
// is accepting or has won is not touched.
func withdrawRingingParticipant(ctx context.Context, tx *sql.Tx, deviceID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT c.call_id::text FROM call_sessions c JOIN rtc_control_calls t ON t.call_id=c.call_id
      JOIN call_participants p ON p.call_id=c.call_id AND p.client_device_id=$1
      WHERE c.direction='incoming' AND c.control_transport='rtc' AND c.state NOT IN ('ended','unknown') AND p.state='ringing'
      ORDER BY c.call_id FOR UPDATE OF c,t`, deviceID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		var state string
		var withdrawn bool
		// Recheck under the call lock: an accept that committed first wins.
		err = tx.QueryRowContext(ctx, `WITH w AS (UPDATE call_participants p SET state='ended',reason='reception_disabled',ended_at=clock_timestamp(),state_revision=state_revision+1,updated_at=clock_timestamp()
          FROM rtc_control_calls t WHERE p.call_id=$1 AND p.client_device_id=$2 AND p.state='ringing' AND t.call_id=p.call_id
            AND t.candidate_client_id IS DISTINCT FROM $2 RETURNING 1)
          SELECT c.state,EXISTS(SELECT 1 FROM w) FROM call_sessions c WHERE c.call_id=$1`, id, deviceID).Scan(&state, &withdrawn)
		if err != nil {
			return err
		}
		if withdrawn {
			if err = controlBump(ctx, tx, id, state, ""); err != nil {
				return err
			}
		}
	}
	return nil
}
