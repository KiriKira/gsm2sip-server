package calls

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestExternalMediaLeaseIsBoundToParticipantAndSelectedCall(t *testing.T) {
	for _, direction := range []string{"outgoing", "incoming"} {
		t.Run(direction, func(t *testing.T) {
			database := openCallsDatabase(t)
			f := seedCallFixture(t, database)
			var mu sync.Mutex
			writes := map[string]string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Query().Get("variable") != "GSM2SIP_RTC_UNTIL" {
					t.Error("unexpected ARI request")
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				writes[r.URL.Path] = r.URL.Query().Get("value")
				mu.Unlock()
				w.WriteHeader(204)
			}))
			defer server.Close()
			m, err := NewManager(database, Config{ARIURL: server.URL + "/ari", ARIUsername: "test", ARIPassword: "test",
				ARIApplication: "gsm2sip", SIPRealm: "gsm2sip", TokenKey: []byte("call-test-token-key-that-is-over-32-bytes")})
			if err != nil {
				t.Fatal(err)
			}
			callID := testUUID(t)
			_, err = database.Exec(`INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,gateway_endpoint_id,client_endpoint_id,gateway_channel_id,client_channel_id,incoming_winner_client_device_id)
			VALUES($1,$2,$3,$4,$5,1,$6,'+15550100200','active','gw','host','lease-gw','lease-host',$3)`, callID, f.ownerID, f.clientID, f.gatewayID, f.simID, direction)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			// A SIP-only call must never obtain the exemption.
			if err := m.RenewExternalMedia(ctx, f.ownerID, f.gatewayID, callID); !errors.Is(err, ErrForbidden) {
				t.Fatalf("SIP-only lease: %v", err)
			}
			_, err = database.Exec(`INSERT INTO rtc_media_sessions(call_id,client_device_id,leg_id,attempt_id) VALUES($1,$2,$3,$4)`, callID, f.clientID, testUUID(t), testUUID(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := m.RenewExternalMedia(ctx, testUUID(t), f.gatewayID, callID); !errors.Is(err, ErrForbidden) {
				t.Fatalf("wrong owner: %v", err)
			}
			if err := m.RenewExternalMedia(ctx, f.ownerID, testUUID(t), callID); !errors.Is(err, ErrForbidden) {
				t.Fatalf("wrong participant: %v", err)
			}
			if err := m.RenewExternalMedia(ctx, f.ownerID, f.gatewayID, callID); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if len(writes) != 1 || writes["/ari/channels/lease-gw/variable"] == "" {
				t.Fatal("gateway renewed another participant's channel")
			}
			mu.Unlock()
			if err := m.RenewExternalMedia(ctx, f.ownerID, f.clientID, callID); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			for _, path := range []string{"/ari/channels/lease-gw/variable", "/ari/channels/lease-host/variable"} {
				until, err := strconv.ParseInt(writes[path], 10, 64)
				if err != nil || until-time.Now().Unix() < 10 || until-time.Now().Unix() > 12 {
					t.Fatal("unbounded or missing lease")
				}
			}
			mu.Unlock()
			if _, err := database.Exec(`UPDATE call_sessions SET state='ended',ended_at=clock_timestamp() WHERE call_id=$1`, callID); err != nil {
				t.Fatal(err)
			}
			if err := m.RenewExternalMedia(ctx, f.ownerID, f.gatewayID, callID); !errors.Is(err, ErrForbidden) {
				t.Fatalf("ended call revived: %v", err)
			}
		})
	}
}
