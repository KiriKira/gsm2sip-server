package calls

import (
	"testing"

	"github.com/kirikira/gsm2sip-server/internal/asterisk"
)

func TestIncomingCallerNumber(t *testing.T) {
	channel := func(number, name string) asterisk.Channel {
		var c asterisk.Channel
		c.Caller.Number, c.Caller.Name = number, name
		return c
	}
	cases := []struct {
		name, number, display, want string
	}{
		{"from user", "09012345678", "09012345678", "09012345678"},
		{"international", "+819012345678", "", "+819012345678"},
		{"anonymous caller", "anonymous", "Anonymous", ""},
		{"display name stands in", "", "09012345678", "09012345678"},
		{"gateway username is not a number", "dev_3f680c7f", "", ""},
		{"nothing", "", "", ""},
		{"too short", "1", "", ""},
	}
	for _, tc := range cases {
		if got := incomingCallerNumber(channel(tc.number, tc.display)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
