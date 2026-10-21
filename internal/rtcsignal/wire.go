package rtcsignal

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

const MaxFrameBytes = 96 * 1024

// DecodeCommand enforces a closed envelope independently of HTTP/WSS. This does
// not register a network endpoint or change the existing wake-only /v1/ws.
// Duplicate keys and explicit nulls are rejected, so other language parsers
// cannot interpret a different sender scope or operation from the same frame.
func DecodeCommand(data []byte) (Command, error) {
	if len(data) == 0 || len(data) > MaxFrameBytes || !utf8.Valid(data) {
		return Command{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueKeys(decoder); err != nil {
		return Command{}, ErrInvalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Command{}, ErrInvalid
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return Command{}, ErrInvalid
	}
	allowed := map[string]bool{"version": true, "call_id": true, "leg_id": true, "attempt_id": true, "operation_id": true, "generation": true, "revision": true, "kind": true, "sdp": true, "ice": true}
	for key := range fields {
		if !allowed[key] {
			return Command{}, ErrInvalid
		}
	}
	for _, name := range []string{"version", "call_id", "leg_id", "attempt_id", "operation_id", "generation", "revision", "kind"} {
		if _, ok := fields[name]; !ok {
			return Command{}, ErrInvalid
		}
	}
	var c Command
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return Command{}, ErrInvalid
	}
	if err := validate(c); err != nil {
		return Command{}, err
	}
	// Even empty/zero extra payloads must not bypass the closed kind schema.
	_, sdp := fields["sdp"]
	_, ice := fields["ice"]
	if (c.Kind == Offer || c.Kind == Answer) && (!sdp || ice) ||
		c.Kind == Candidate && (!ice || sdp) ||
		c.Kind != Offer && c.Kind != Answer && c.Kind != Candidate && (sdp || ice) {
		return Command{}, ErrInvalid
	}
	if ice {
		var candidate map[string]json.RawMessage
		if err := json.Unmarshal(fields["ice"], &candidate); err != nil {
			return Command{}, ErrInvalid
		}
		if len(candidate) != 3 {
			return Command{}, ErrInvalid
		}
		for _, key := range []string{"candidate", "sdp_mid", "sdp_mline_index"} {
			if _, ok := candidate[key]; !ok {
				return Command{}, ErrInvalid
			}
		}
	}
	return c, nil
}

// The wire contract has maximum nesting of two objects and no arrays. Token
// traversal avoids json.Unmarshal's last-key-wins behaviour and bounds depth.
func uniqueKeys(d *json.Decoder) error {
	return objectKeys(d, 0)
}
func objectKeys(d *json.Decoder, depth int) error {
	if depth > 1 {
		return ErrInvalid
	}
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return ErrInvalid
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalid
		}
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 || trimmed[0] == '[' {
			return ErrInvalid
		}
		if trimmed[0] == '{' {
			if err := objectKeys(json.NewDecoder(bytes.NewReader(value)), depth+1); err != nil {
				return err
			}
		}
		if trimmed[0] == '"' && !validUnicodeEscapes(trimmed) {
			return ErrInvalid
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return ErrInvalid
	}
	return nil
}

// encoding/json otherwise replaces lone UTF-16 surrogates with U+FFFD, while
// Android retains them. Reject this ambiguity before hashing/relaying strings.
func validUnicodeEscapes(raw []byte) bool {
	for index := 1; index < len(raw)-1; index++ {
		if raw[index] != '\\' {
			continue
		}
		index++
		if raw[index] != 'u' {
			continue
		}
		value, err := strconv.ParseUint(string(raw[index+1:index+5]), 16, 16)
		if err != nil {
			return false
		}
		index += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if index+6 >= len(raw)-1 || raw[index+1] != '\\' || raw[index+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[index+3:index+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			index += 6
		}
	}
	return true
}
