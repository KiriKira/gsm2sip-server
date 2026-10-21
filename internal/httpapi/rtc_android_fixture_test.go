package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// Opt-in native Android relay fixture. This is a test binary, never an API
// backdoor. Business state is explicitly a fixture here; full SIP/Telecom
// acceptance is a separate run and must not be inferred from this test.
func TestRTCAndroidRelayFixture(t *testing.T) {
	path := os.Getenv("RTC_ANDROID_FIXTURE_FILE")
	if path == "" {
		t.Skip("native Android fixture not requested")
	}
	database, f, api, callID := rtcFixture(t, "outgoing")
	rtcRegister(t, api, f.gatewayToken)
	if _, err := database.db.Exec(`UPDATE call_sessions SET expires_at=clock_timestamp()+interval '10 minutes' WHERE call_id=$1`, callID); err != nil {
		t.Fatal(err)
	}
	addr := os.Getenv("RTC_ANDROID_FIXTURE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18095"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go server.Serve(listener)
	config, _ := json.Marshal(map[string]string{"call_id": callID, "client_token": f.clientToken, "gateway_token": f.gatewayToken, "base_url": "http://10.0.2.2:18095/v1"})
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(8 * time.Minute)
	for {
		select {
		case <-deadline:
			t.Fatal("Android fixture timed out")
		case <-ticker.C:
			if _, err := os.Stat(path + ".stop"); err == nil {
				return
			}
			// Model only the business authority transition for the relay test.
			_, err := database.db.Exec(`UPDATE call_sessions SET state='dialing' WHERE call_id=$1 AND state='reserved' AND EXISTS(SELECT 1 FROM rtc_media_sessions WHERE call_id=$1)`, callID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
