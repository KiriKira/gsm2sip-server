package rtcsignal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
		strings.Replace(base, `candidate:1`, `candidate:\ud800`, 1),
		strings.Replace(base, `candidate:1`, `candidate:\udc00`, 1),
		strings.Repeat(" ", MaxFrameBytes+1),
	} {
		if _, err := DecodeCommand([]byte(invalid)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe wire envelope accepted: %v", err)
		}
	}
}

func TestUnicodeScalarsDoNotChangeAcrossWireParsers(t *testing.T) {
	c := command(newLeg(t), Offer, "unicode")
	c.SDP = "v=0\r\na=probe:😀\r\n"
	data, _ := json.Marshal(c)
	data = []byte(strings.Replace(string(data), "😀", `\ud83d\ude00`, 1))
	decoded, err := DecodeCommand(data)
	if err != nil || decoded.SDP != c.SDP {
		t.Fatal("valid UTF-16 pair was not preserved", err)
	}
}

func FuzzClosedCommandDecoder(f *testing.F) {
	fixture, err := os.ReadFile("../../protocol/rtc-v1/fixtures/candidate.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(fixture)
	f.Add([]byte(`{"sdp":"\ud800"}`))
	f.Add([]byte{0xc3, 0x28})
	f.Fuzz(func(t *testing.T, data []byte) {
		command, err := DecodeCommand(data)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip, err := DecodeCommand(encoded)
		if err != nil || !reflect.DeepEqual(command, roundTrip) {
			t.Fatal("accepted command changed on canonical wire round trip", err)
		}
	})
}
