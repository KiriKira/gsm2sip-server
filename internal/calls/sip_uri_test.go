package calls

import "testing"

func TestSIPURIFollowsTransport(t *testing.T) {
	secure := (&Manager{config: Config{SIPRealm: "gsm2sip"}}).sipURI("tok")
	if secure != "sips:call.tok@gsm2sip;transport=tls" {
		t.Fatalf("TLS call URI changed: %s", secure)
	}
	plain := (&Manager{config: Config{SIPRealm: "gsm2sip", SIPInsecure: true}}).sipURI("tok")
	if plain != "sip:call.tok@gsm2sip;transport=udp" {
		t.Fatalf("UDP call URI is wrong: %s", plain)
	}
}
