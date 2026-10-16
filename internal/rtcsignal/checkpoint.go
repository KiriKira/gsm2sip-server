package rtcsignal

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

const maxCheckpointBytes = 4 * 1024 * 1024

// Checkpoints are private storage format, not the wire snapshot. They contain
// expiring SDP/ICE and MUST NOT be logged, returned to clients or backed by a
// store without TTL cleanup. Receipts retain only payload hashes, not payloads.
type checkpoint struct {
	Format          int
	Snapshot        Snapshot
	Candidates      [2]int
	EndedCandidates [2]bool
	Receipts        map[string]receipt
	Signals         []Signal
}

func (l Leg) encode(now time.Time) ([]byte, error) {
	l = l.clone(now)
	data, err := json.Marshal(checkpoint{1, l.Snapshot(), l.candidates, l.endedCandidates, l.receipts, l.signals})
	if err != nil || len(data) > maxCheckpointBytes {
		return nil, ErrLimit
	}
	return data, nil
}

func restore(data []byte) (Leg, error) {
	if len(data) > maxCheckpointBytes {
		return Leg{}, ErrInvalid
	}
	var c checkpoint
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Leg{}, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return Leg{}, ErrInvalid
	}
	s := c.Snapshot
	// Reuse binding and lease validation without reviving an expired grant.
	if _, err := New(s.Binding, s.AuthorizedUntil.Add(-MaxAuthorizationLifetime), s.AuthorizedUntil); err != nil {
		return Leg{}, ErrInvalid
	}
	if c.Format != 1 || s.Revision < 1 || s.Generation < 1 || s.Generation > MaxGenerations ||
		s.AuthorizedUntil.IsZero() || s.NegotiationUntil.IsZero() || s.NegotiationUntil.After(s.AuthorizedUntil) ||
		len(c.Receipts) > MaxOperations+1 || len(c.Signals) > 2*MaxCandidates+4 {
		return Leg{}, ErrInvalid
	}
	switch s.Phase {
	case WaitingOffer, WaitingAnswer, Negotiated:
		if s.Reason != "" {
			return Leg{}, ErrInvalid
		}
	case Closed:
		if len(c.Signals) != 0 || (s.Reason != "participant_closed" && s.Reason != "authorization_expired" && s.Reason != "negotiation_expired" && s.Reason != "authorization_revoked") {
			return Leg{}, ErrInvalid
		}
	default:
		return Leg{}, ErrInvalid
	}
	for _, count := range c.Candidates {
		if count < 0 || count > MaxCandidates {
			return Leg{}, ErrInvalid
		}
	}
	for key, receipt := range c.Receipts {
		parts := strings.Split(key, "/")
		if len(parts) != 2 || (parts[0] != s.Binding.ClientID && parts[0] != s.Binding.GatewayID) ||
			!validID(parts[1]) || receipt.Ack.OperationID != parts[1] || receipt.Ack.Revision < 2 ||
			receipt.Ack.Revision > s.Revision || receipt.Ack.Generation < 1 || receipt.Ack.Generation > s.Generation {
			return Leg{}, ErrInvalid
		}
	}
	sequences := map[int64]bool{}
	for _, signal := range c.Signals {
		if signal.Generation != s.Generation || signal.Sequence < 2 || signal.Sequence > s.Revision ||
			sequences[signal.Sequence] || signal.ExpiresAt.IsZero() || signal.ExpiresAt.After(s.AuthorizedUntil) {
			return Leg{}, ErrInvalid
		}
		sequences[signal.Sequence] = true
		if !((signal.SenderID == s.Binding.ClientID && signal.RecipientID == s.Binding.GatewayID) ||
			(signal.SenderID == s.Binding.GatewayID && signal.RecipientID == s.Binding.ClientID)) {
			return Leg{}, ErrInvalid
		}
		command := Command{Version: Version, CallID: s.Binding.CallID, LegID: s.Binding.LegID,
			AttemptID: s.Binding.AttemptID, OperationID: "checkpoint", Generation: signal.Generation,
			Kind: signal.Kind, SDP: signal.SDP, ICE: signal.ICE}
		if signal.Kind == Offer || signal.Kind == Answer {
			command.Revision = signal.Sequence
		}
		if signal.Kind == Close || signal.Kind == Restart || validate(command) != nil {
			return Leg{}, ErrInvalid
		}
		if signal.Kind == Offer && signal.SenderID != s.Binding.ClientID || signal.Kind == Answer && signal.SenderID != s.Binding.GatewayID {
			return Leg{}, ErrInvalid
		}
	}
	if c.Receipts == nil {
		c.Receipts = map[string]receipt{}
	}
	return Leg{s.Binding, s.Phase, s.Revision, s.Generation, s.AuthorizedUntil, s.NegotiationUntil,
		s.Reason, c.Candidates, c.EndedCandidates, c.Receipts, c.Signals}, nil
}

func (l Leg) nextExpiry(now time.Time) *time.Time {
	if l.phase == Closed {
		return nil
	}
	value := l.authorizedUntil
	if l.phase != Negotiated {
		value = minTime(value, l.negotiationUntil)
	}
	for _, signal := range l.signals {
		if signal.ExpiresAt.After(now) {
			value = minTime(value, signal.ExpiresAt)
		}
	}
	return &value
}
