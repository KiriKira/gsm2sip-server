package rtcsignal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedExperimentalFixturesDecode(t *testing.T) {
	files, err := filepath.Glob("../../protocol/rtc-v1/fixtures/*.json")
	if err != nil || len(files) != 6 {
		t.Fatal("missing RTC fixtures", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeCommand(data); err != nil {
			t.Fatalf("%s: %v", filepath.Base(file), err)
		}
	}
}

func TestClosedWireEnvelopeRejectsAmbiguousOrForgedIdentity(t *testing.T) {
	c := command(newLeg(t), Candidate, "ice")
	data, _ := json.Marshal(c)
	if _, err := DecodeCommand(data); err != nil {
		t.Fatal(err)
	}
	base := string(data)
	for _, invalid := range []string{
		base + "{}", "[]", "null", strings.Replace(base, `"version":1`, `"version":1,"version":2`, 1),
		strings.Replace(base, `"version":1`, `"version":1,"sender_id":"another-device"`, 1),
		strings.Replace(base, `"version":1`, `"version":1,"VERSION":1`, 1),
		strings.Replace(base, `"revision":0,`, "", 1),
		strings.Replace(base, `"sdp_mline_index":0`, `"sdp_mline_index":0,"sdp_mline_index":1`, 1),
		strings.Replace(base, `"sdp_mline_index":0`, `"sdp_mline_index":0,"SDP_MLINE_INDEX":0`, 1),
		strings.Replace(base, `"sdp_mline_index":0`, `"sdp_mline_index":null`, 1),
		strings.Replace(base, `"sdp_mline_index":0`, `"unexpected":0`, 1),
		strings.Replace(base, `"revision":0`, `"revision":0,"sdp":""`, 1),
		strings.Repeat(" ", MaxFrameBytes+1),
	} {
		if _, err := DecodeCommand([]byte(invalid)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe wire envelope accepted: %v", err)
		}
	}
}
