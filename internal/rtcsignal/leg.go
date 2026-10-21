// Package rtcsignal implements the version-1 WebRTC negotiation leg reducer.
// It does not authorize or execute cellular calls. Its input binding must come
// from the call coordinator, after shared gateway occupancy and SIM checks.
package rtcsignal

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Version                  = 1
	MaxSDPBytes              = 64 * 1024
	MaxCandidateBytes        = 2048
	MaxCandidates            = 128  // per endpoint, per generation
	MaxOperations            = 2304 // bounds durable deduplication; never evict and re-execute
	MaxGenerations           = 8
	SignalLifetime           = 15 * time.Second
	NegotiationLifetime      = 30 * time.Second
	MaxAuthorizationLifetime = 60 * time.Second
)

var (
	ErrInvalid       = errors.New("invalid RTC signal")
	ErrForbidden     = errors.New("RTC leg is not available to this device")
	ErrConflict      = errors.New("RTC signal conflicts with leg state")
	ErrExpired       = errors.New("RTC leg authorization or negotiation expired")
	ErrClosed        = errors.New("RTC leg is closed")
	ErrLimit         = errors.New("RTC leg signal budget exceeded")
	ErrReplayExpired = errors.New("RTC generation replay is incomplete")
)

type Principal struct{ OwnerID, DeviceID string }

// Binding is trusted coordinator input, never copied from an untrusted frame.
// A leg identifies exactly one client/gateway pair, not all ringing clients.
type Binding struct {
	// Retained only to decode existing checkpoints; new legs omit this legacy
	// field. Session, owner, call and paired-device identities authorize access.
	NodeID          string `json:"node_id,omitempty"`
	OwnerID         string `json:"owner_id"`
	CallID          string `json:"call_id"`
	LegID           string `json:"leg_id"`
	AttemptID       string `json:"attempt_id"`
	ClientID        string `json:"client_id"`
	GatewayID       string `json:"gateway_id"`
	SIMID           string `json:"sim_id"`
	MappingRevision int64  `json:"mapping_revision"`
}

type Phase string

const (
	WaitingOffer  Phase = "waiting_offer"
	WaitingAnswer Phase = "waiting_answer"
	Negotiated    Phase = "negotiated" // NOT cellular ACTIVE or an incoming winner
	Closed        Phase = "closed"
)

type Kind string

const (
	Offer         Kind = "offer"
	Answer        Kind = "answer"
	Candidate     Kind = "candidate"
	EndCandidates Kind = "end_of_candidates"
	Restart       Kind = "restart"
	Close         Kind = "close"
)

// Command has no sender/recipient field. Authentication supplies the sender;
// the binding supplies the only possible recipient. Revision is mandatory for
// SDP/restart and zero for trickle ICE/close, which must tolerate racing frames.
type Command struct {
	Version     int           `json:"version"`
	CallID      string        `json:"call_id"`
	LegID       string        `json:"leg_id"`
	AttemptID   string        `json:"attempt_id"`
	OperationID string        `json:"operation_id"`
	Generation  int           `json:"generation"`
	Revision    int64         `json:"revision"`
	Kind        Kind          `json:"kind"`
	SDP         string        `json:"sdp,omitempty"`
	ICE         *ICECandidate `json:"ice,omitempty"`
}

type ICECandidate struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdp_mid"`
	SDPMLineIndex int    `json:"sdp_mline_index"`
}

type Ack struct {
	OperationID string `json:"operation_id"`
	Revision    int64  `json:"revision"`
	Generation  int    `json:"generation"`
}

type Signal struct {
	Sequence    int64         `json:"sequence"`
	SenderID    string        `json:"sender_id"`
	RecipientID string        `json:"recipient_id"`
	Generation  int           `json:"generation"`
	Kind        Kind          `json:"kind"`
	SDP         string        `json:"sdp,omitempty"`
	ICE         *ICECandidate `json:"ice,omitempty"`
	ExpiresAt   time.Time     `json:"expires_at"`
}

type receipt struct {
	Digest [32]byte
	Ack    Ack
}

// Leg owns its state. Apply returns a new value; failure never partially mutates
// the old value. A repository must serialize updates and atomically persist the
// new leg, receipt and pending signals BEFORE returning ACK or relaying frames.
// This package intentionally has no process-local production repository.
type Leg struct {
	binding          Binding
	phase            Phase
	revision         int64
	generation       int
	authorizedUntil  time.Time
	negotiationUntil time.Time
	reason           string
	candidates       [2]int
	endedCandidates  [2]bool
	receipts         map[string]receipt
	signals          []Signal
}

type Snapshot struct {
	Binding          Binding   `json:"binding"`
	Phase            Phase     `json:"phase"`
	Revision         int64     `json:"revision"`
	Generation       int       `json:"generation"`
	AuthorizedUntil  time.Time `json:"authorized_until"`
	NegotiationUntil time.Time `json:"negotiation_until"`
	Reason           string    `json:"reason,omitempty"`
}

type Transition struct {
	Leg       Leg
	Ack       Ack
	Duplicate bool
}

func New(binding Binding, now, authorizedUntil time.Time) (Leg, error) {
	for _, value := range []string{binding.OwnerID, binding.CallID, binding.LegID,
		binding.AttemptID, binding.ClientID, binding.GatewayID, binding.SIMID} {
		if !validID(value) {
			return Leg{}, ErrInvalid
		}
	}
	if binding.ClientID == binding.GatewayID || binding.MappingRevision <= 0 || now.IsZero() ||
		!authorizedUntil.After(now) || authorizedUntil.Sub(now) > MaxAuthorizationLifetime {
		return Leg{}, ErrInvalid
	}
	return Leg{binding: binding, phase: WaitingOffer, revision: 1, generation: 1,
		authorizedUntil: authorizedUntil, negotiationUntil: minTime(now.Add(NegotiationLifetime), authorizedUntil),
		receipts: make(map[string]receipt)}, nil
}

func (l Leg) Snapshot() Snapshot {
	return Snapshot{l.binding, l.phase, l.revision, l.generation, l.authorizedUntil, l.negotiationUntil, l.reason}
}

func (l Leg) member(p Principal) (int, error) {
	if p.OwnerID == l.binding.OwnerID && p.OwnerID != "" {
		if p.DeviceID == l.binding.ClientID {
			return 0, nil
		}
		if p.DeviceID == l.binding.GatewayID {
			return 1, nil
		}
	}
	return 0, ErrForbidden
}

func (l Leg) Apply(p Principal, c Command, now time.Time) (Transition, error) {
	side, err := l.member(p)
	if err != nil {
		return Transition{}, err
	}
	if err = validate(c); err != nil {
		return Transition{}, err
	}
	if c.CallID != l.binding.CallID || c.LegID != l.binding.LegID || c.AttemptID != l.binding.AttemptID {
		return Transition{}, ErrForbidden
	}
	// Authenticate on every retry, including duplicates, before revealing an ACK.
	key := p.DeviceID + "/" + c.OperationID
	encoded, _ := json.Marshal(c)
	digest := sha256.Sum256(encoded)
	if previous, exists := l.receipts[key]; exists {
		if previous.Digest != digest {
			return Transition{}, ErrConflict
		}
		// Historical ACKs do not restore state or re-emit SDP/ICE after termination.
		return Transition{Leg: l, Ack: previous.Ack, Duplicate: true}, nil
	}
	if l.phase == Closed {
		return Transition{}, ErrClosed
	}
	if now.IsZero() || !now.Before(l.authorizedUntil) {
		return Transition{}, ErrExpired
	}
	// Close remains possible after negotiation timeout, even with a stale revision.
	if c.Kind != Close && l.phase != Negotiated && !now.Before(l.negotiationUntil) {
		return Transition{}, ErrExpired
	}
	// Close terminates this exact call/leg/attempt, including after an ICE
	// restart racing the user's cancellation. It can never target a future
	// generation or a different attempt. Other messages must match exactly.
	if c.Kind == Close && c.Generation > l.generation || c.Kind != Close && c.Generation != l.generation {
		return Transition{}, ErrConflict
	}
	if len(l.receipts) >= MaxOperations && c.Kind != Close {
		return Transition{}, ErrLimit
	}
	if (c.Kind == Offer || c.Kind == Answer || c.Kind == Restart) && c.Revision != l.revision {
		return Transition{}, ErrConflict
	}
	next := l.clone(now)
	switch c.Kind {
	case Offer:
		if side != 0 {
			return Transition{}, ErrForbidden
		}
		if l.phase != WaitingOffer {
			return Transition{}, ErrConflict
		}
		next.phase = WaitingAnswer
	case Answer:
		if side != 1 {
			return Transition{}, ErrForbidden
		}
		if l.phase != WaitingAnswer {
			return Transition{}, ErrConflict
		}
		next.phase = Negotiated
	case Candidate, EndCandidates:
		if l.endedCandidates[side] {
			return Transition{}, ErrConflict
		}
		if c.Kind == Candidate {
			if l.candidates[side] >= MaxCandidates {
				return Transition{}, ErrLimit
			}
			next.candidates[side]++
		} else {
			next.endedCandidates[side] = true
		}
	case Restart:
		if side != 0 {
			return Transition{}, ErrForbidden
		}
		if l.phase != Negotiated {
			return Transition{}, ErrConflict
		}
		if l.generation >= MaxGenerations {
			return Transition{}, ErrLimit
		}
		next.generation++
		next.phase = WaitingOffer
		next.negotiationUntil = minTime(now.Add(NegotiationLifetime), l.authorizedUntil)
		next.candidates, next.endedCandidates = [2]int{}, [2]bool{}
		next.signals = nil // never replay an old ICE generation
	case Close:
		next.phase, next.reason, next.signals = Closed, "participant_closed", nil
	default:
		return Transition{}, ErrInvalid
	}
	next.revision++
	ack := Ack{c.OperationID, next.revision, next.generation}
	next.receipts[key] = receipt{digest, ack}
	if c.Kind != Close && c.Kind != Restart {
		recipient := l.binding.GatewayID
		if side == 1 {
			recipient = l.binding.ClientID
		}
		next.signals = append(next.signals, Signal{next.revision, p.DeviceID, recipient, l.generation,
			c.Kind, c.SDP, cloneICE(c.ICE), minTime(now.Add(SignalLifetime), l.authorizedUntil)})
	}
	return Transition{Leg: next, Ack: ack}, nil
}

// Pending returns descriptions before early ICE, only for this recipient and
// generation. The receiving endpoint must buffer ICE until setRemoteDescription
// succeeds. On reconnect a missing/expired description requires a new generation;
// this method is not proof that a previously sent description was applied.
func (l Leg) Pending(p Principal, now time.Time) ([]Signal, error) {
	side, err := l.member(p)
	if err != nil {
		return nil, err
	}
	if l.phase == Closed {
		return nil, ErrClosed
	}
	if now.IsZero() || !now.Before(l.authorizedUntil) ||
		(l.phase != Negotiated && !now.Before(l.negotiationUntil)) {
		return nil, ErrExpired
	}
	var descriptions, ice []Signal
	var liveCandidates int
	var liveEnd, liveDescription bool
	for _, signal := range l.signals {
		if signal.RecipientID != p.DeviceID || !now.Before(signal.ExpiresAt) {
			continue
		}
		signal.ICE = cloneICE(signal.ICE)
		if signal.Kind == Offer || signal.Kind == Answer {
			liveDescription = true
			descriptions = append(descriptions, signal)
		} else {
			if signal.Kind == Candidate {
				liveCandidates++
			} else if signal.Kind == EndCandidates {
				liveEnd = true
			}
			ice = append(ice, signal)
		}
	}
	// Counters survive TTL cleanup. Without them a still-live EOC could hide an
	// earlier expired candidate and appear to be a complete replay.
	remote := 1 - side
	wantDescription := side == 1 && l.phase != WaitingOffer || side == 0 && l.phase == Negotiated
	if liveCandidates != l.candidates[remote] || liveEnd != l.endedCandidates[remote] || wantDescription && !liveDescription {
		return nil, ErrReplayExpired
	}
	return append(descriptions, ice...), nil
}

// Expire is a coordinator tick. Persist the returned state before notifying
// participants; it does not release physical gateway occupancy or redial.
func (l Leg) Expire(now time.Time) (Leg, bool) {
	if l.phase == Closed || now.IsZero() {
		return l, false
	}
	reason := ""
	if !now.Before(l.authorizedUntil) {
		reason = "authorization_expired"
	} else if l.phase != Negotiated && !now.Before(l.negotiationUntil) {
		reason = "negotiation_expired"
	}
	if reason == "" {
		return l, false
	}
	next := l.clone(now)
	next.phase, next.reason, next.signals = Closed, reason, nil
	next.revision++
	return next, true
}

// ExtendAuthorization is coordinator-only: the authorizer must have rechecked
// both devices, the business call, gateway fencing and SIM inside the same
// durable transaction. It is deliberately not a participant wire command.
// Renewal never moves the negotiation deadline or starts cellular execution.
func (l Leg) ExtendAuthorization(now, until time.Time) (Leg, error) {
	if l.phase == Closed {
		return Leg{}, ErrClosed
	}
	if now.IsZero() || !now.Before(l.authorizedUntil) ||
		(l.phase != Negotiated && !now.Before(l.negotiationUntil)) {
		return Leg{}, ErrExpired
	}
	if !until.After(l.authorizedUntil) || until.Sub(now) > MaxAuthorizationLifetime {
		return Leg{}, ErrInvalid
	}
	next := l.clone(now)
	next.authorizedUntil = until
	next.revision++
	return next, nil
}

// Revoke closes signaling authorization, retaining only historical receipts.
// The coordinator must additionally enforce bounded audio/physical-call cleanup
// at the gateway; clearing the signaling connection alone cannot stop SRTP.
func (l Leg) Revoke(now time.Time) (Leg, bool) {
	if l.phase == Closed {
		return l, false
	}
	next := l.clone(now)
	next.phase, next.reason, next.signals = Closed, "authorization_revoked", nil
	next.revision++
	return next, true
}

func (l Leg) clone(now time.Time) Leg {
	next := l
	next.receipts = make(map[string]receipt, len(l.receipts)+1)
	for key, value := range l.receipts {
		next.receipts[key] = value
	}
	next.signals = nil
	for _, value := range l.signals {
		if now.Before(value.ExpiresAt) {
			value.ICE = cloneICE(value.ICE)
			next.signals = append(next.signals, value)
		}
	}
	return next
}

func cloneICE(c *ICECandidate) *ICECandidate {
	if c == nil {
		return nil
	}
	value := *c
	return &value
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func validID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validate(c Command) error {
	if c.Version != Version || !validID(c.CallID) || !validID(c.LegID) || !validID(c.AttemptID) ||
		!validID(c.OperationID) || c.Generation < 1 || c.Generation > MaxGenerations {
		return ErrInvalid
	}
	if c.Kind == Offer || c.Kind == Answer {
		if c.Revision < 1 || c.ICE != nil || len(c.SDP) == 0 || len(c.SDP) > MaxSDPBytes ||
			!utf8.ValidString(c.SDP) || strings.ContainsRune(c.SDP, 0) {
			return ErrInvalid
		}
		return nil
	}
	if c.SDP != "" {
		return ErrInvalid
	}
	if c.Kind == Candidate {
		if c.ICE == nil || len(c.ICE.Candidate) == 0 || len(c.ICE.Candidate) > MaxCandidateBytes ||
			!utf8.ValidString(c.ICE.Candidate) || strings.ContainsAny(c.ICE.Candidate, "\r\n\x00") ||
			!validID(c.ICE.SDPMid) || c.ICE.SDPMLineIndex < 0 || c.ICE.SDPMLineIndex > 15 {
			return ErrInvalid
		}
	} else if c.ICE != nil {
		return ErrInvalid
	}
	switch c.Kind {
	case Restart:
		if c.Revision < 1 {
			return ErrInvalid
		}
	case Candidate, EndCandidates, Close:
		if c.Revision != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
