package rtcsignal

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
var client = Principal{"owner", "client"}
var gateway = Principal{"owner", "gateway"}

func newLeg(t *testing.T) Leg {
	t.Helper()
	l, err := New(Binding{"node", "owner", "call", "leg", "attempt", "client", "gateway", "sim", 7}, epoch, epoch.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func command(l Leg, kind Kind, id string) Command {
	c := Command{Version: Version, CallID: "call", LegID: "leg", AttemptID: "attempt", OperationID: id, Generation: l.generation, Kind: kind}
	if kind == Offer || kind == Answer || kind == Restart {
		c.Revision = l.revision
	}
	if kind == Offer || kind == Answer {
		c.SDP = "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n"
	}
	if kind == Candidate {
		c.ICE = &ICECandidate{"candidate:1 1 UDP 1 192.0.2.1 1234 typ host", "0", 0}
	}
	return c
}
func apply(t *testing.T, l Leg, p Principal, c Command) Leg {
	t.Helper()
	result, err := l.Apply(p, c, epoch.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return result.Leg
}
func negotiated(t *testing.T) Leg {
	t.Helper()
	l := newLeg(t)
	l = apply(t, l, client, command(l, Offer, "offer"))
	return apply(t, l, gateway, command(l, Answer, "answer"))
}

func TestEarlyICEIsBoundedAndDescriptionComesFirst(t *testing.T) {
	l := newLeg(t)
	ice := command(l, Candidate, "ice")
	l = apply(t, l, client, ice)
	ice.ICE.Candidate = "mutated caller buffer"
	l = apply(t, l, client, command(l, EndCandidates, "ice-end"))
	l = apply(t, l, client, command(l, Offer, "offer"))
	items, err := l.Pending(gateway, epoch.Add(2*time.Second))
	if err != nil || len(items) != 3 || items[0].Kind != Offer || items[1].Kind != Candidate || items[2].Kind != EndCandidates {
		t.Fatalf("description must precede early ICE: kinds=%v err=%v", kinds(items), err)
	}
	items[1].ICE.Candidate = "mutated recipient buffer"
	items, _ = l.Pending(gateway, epoch.Add(2*time.Second))
	if !strings.HasPrefix(items[1].ICE.Candidate, "candidate:") {
		t.Fatal("stored ICE was aliased")
	}
	if own, _ := l.Pending(client, epoch.Add(2*time.Second)); len(own) != 0 {
		t.Fatal("echoed sender signals")
	}
	if _, err := l.Apply(client, command(l, Candidate, "after-end"), epoch.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
func kinds(items []Signal) []Kind {
	var result []Kind
	for _, s := range items {
		result = append(result, s.Kind)
	}
	return result
}

func TestSDPRolesRevisionsAndNegotiationDoNotAuthorizeCellularExecution(t *testing.T) {
	l := newLeg(t)
	for _, test := range []struct {
		p    Principal
		c    Command
		want error
	}{
		{gateway, command(l, Offer, "wrong-offerer"), ErrForbidden},
		{gateway, command(l, Answer, "early-answer"), ErrConflict},
		{client, command(l, Restart, "early-restart"), ErrConflict},
	} {
		before := l.Snapshot()
		if _, err := l.Apply(test.p, test.c, epoch.Add(time.Second)); !errors.Is(err, test.want) {
			t.Fatal(err)
		}
		if l.Snapshot() != before || len(l.receipts) != 0 {
			t.Fatal("failed operation mutated leg")
		}
	}
	stale := command(l, Offer, "stale-offer")
	l = apply(t, l, client, command(l, Candidate, "ice"))
	if _, err := l.Apply(client, stale, epoch.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale SDP accepted", err)
	}
	l = apply(t, l, client, command(l, Offer, "offer"))
	if _, err := l.Apply(client, command(l, Answer, "wrong-answerer"), epoch.Add(time.Second)); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	l = apply(t, l, gateway, command(l, Answer, "answer"))
	if l.Snapshot().Phase != Negotiated {
		t.Fatal("SDP handshake did not converge")
	}
	// There is deliberately no dial, cellular answer, winner or ACTIVE transition.
	bad := command(l, Kind("active"), "fake-active")
	if _, err := l.Apply(gateway, bad, epoch.Add(time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestScopeCannotBeChangedByCallOrDeviceIdentifiers(t *testing.T) {
	l := newLeg(t)
	for _, p := range []Principal{{"other-owner", "client"}, {"owner", "another-client"}, {"", "gateway"}} {
		if _, err := l.Apply(p, command(l, Offer, "probe"), epoch); !errors.Is(err, ErrForbidden) {
			t.Fatal(err)
		}
		if _, err := l.Pending(p, epoch); !errors.Is(err, ErrForbidden) {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"call", "leg", "attempt"} {
		c := command(l, Offer, "probe")
		switch field {
		case "call":
			c.CallID = "other"
		case "leg":
			c.LegID = "other"
		case "attempt":
			c.AttemptID = "other"
		}
		if _, err := l.Apply(client, c, epoch); !errors.Is(err, ErrForbidden) {
			t.Fatal(field, err)
		}
	}
}

func TestDuplicateACKIsStableButNeverRevivesTerminalState(t *testing.T) {
	l := newLeg(t)
	c := command(l, Offer, "op")
	first, err := l.Apply(client, c, epoch)
	if err != nil {
		t.Fatal(err)
	}
	l = first.Leg
	duplicate, err := l.Apply(client, c, epoch.Add(time.Second))
	if err != nil || !duplicate.Duplicate || duplicate.Ack != first.Ack || !reflect.DeepEqual(l, duplicate.Leg) {
		t.Fatal("retry was re-executed", err)
	}
	changed := c
	changed.SDP += "a=changed\r\n"
	if _, err := l.Apply(client, changed, epoch); !errors.Is(err, ErrConflict) {
		t.Fatal("operation ID rebound", err)
	}
	l = apply(t, l, gateway, command(l, Close, "close"))
	duplicate, err = l.Apply(client, c, epoch.Add(2*time.Minute))
	if err != nil || !duplicate.Duplicate || duplicate.Leg.phase != Closed || len(duplicate.Leg.signals) != 0 {
		t.Fatal("historical ACK revived call", err)
	}
	if _, err := l.Apply(gateway, command(l, Answer, "late-answer"), epoch); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := l.Pending(client, epoch); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestRestartFencesOldICEAndHasOneOfferer(t *testing.T) {
	l := negotiated(t)
	old := command(l, Candidate, "late-old-generation")
	if _, err := l.Apply(gateway, command(l, Restart, "glare"), epoch); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	l = apply(t, l, client, command(l, Restart, "restart"))
	if l.generation != 2 || l.phase != WaitingOffer || len(l.signals) != 0 {
		t.Fatal("restart leaked old description")
	}
	if _, err := l.Apply(client, old, epoch); !errors.Is(err, ErrConflict) {
		t.Fatal("old ICE accepted", err)
	}
	l = apply(t, l, client, command(l, Offer, "offer-2"))
	l = apply(t, l, gateway, command(l, Answer, "answer-2"))
	if l.phase != Negotiated || l.generation != 2 {
		t.Fatal("restart did not negotiate")
	}
}

func TestCancellationWinsEitherOrderAgainstAnswer(t *testing.T) {
	l := newLeg(t)
	l = apply(t, l, client, command(l, Offer, "offer"))
	answer := command(l, Answer, "answer")
	close := command(l, Close, "cancel")
	closed := apply(t, l, client, close)
	if _, err := closed.Apply(gateway, answer, epoch); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	answered := apply(t, l, gateway, answer)
	closed = apply(t, answered, client, close)
	if closed.phase != Closed || len(closed.signals) != 0 {
		t.Fatal("answer survived cancellation")
	}
}

func TestCancellationWinsEitherOrderAgainstICERestart(t *testing.T) {
	l := negotiated(t)
	close := command(l, Close, "cancel")
	restart := command(l, Restart, "restart")
	restarted := apply(t, l, client, restart)
	closed := apply(t, restarted, client, close)
	if closed.phase != Closed || len(closed.signals) != 0 {
		t.Fatal("ICE restart swallowed cancellation")
	}
	closed = apply(t, l, client, close)
	if _, err := closed.Apply(client, restart, epoch); !errors.Is(err, ErrClosed) {
		t.Fatal("ICE restart revived cancelled leg", err)
	}
}

func TestTimeoutAndSignalTTLFailClosedAtBoundary(t *testing.T) {
	l := newLeg(t)
	if _, err := l.Apply(client, command(l, Offer, "late"), epoch.Add(NegotiationLifetime)); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	expired, changed := l.Expire(epoch.Add(NegotiationLifetime))
	if !changed || expired.phase != Closed || expired.reason != "negotiation_expired" {
		t.Fatal("timeout did not terminate")
	}
	if _, changed = expired.Expire(epoch.Add(time.Minute)); changed {
		t.Fatal("termination was not idempotent")
	}
	l = negotiated(t)
	if items, err := l.Pending(client, epoch.Add(1*time.Second+SignalLifetime)); !errors.Is(err, ErrReplayExpired) || len(items) != 0 {
		t.Fatal("expired SDP replayed", err)
	}
	if _, err := l.Apply(client, command(l, Restart, "expired"), epoch.Add(time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	expired, changed = l.Expire(epoch.Add(time.Minute))
	if !changed || expired.reason != "authorization_expired" {
		t.Fatal("authorization timeout ignored")
	}
}

func TestExpiredEarlyCandidateCannotHideBehindLiveDescriptionAndEOC(t *testing.T) {
	l := newLeg(t)
	first, err := l.Apply(client, command(l, Candidate, "early"), epoch)
	if err != nil {
		t.Fatal(err)
	}
	l = first.Leg
	l = apply(t, l, client, command(l, Offer, "offer"))
	l = apply(t, l, client, command(l, EndCandidates, "end"))
	// Candidate expired at t=15; SDP/EOC are still live until t=16.
	if signals, err := l.Pending(gateway, epoch.Add(SignalLifetime)); !errors.Is(err, ErrReplayExpired) || len(signals) != 0 {
		t.Fatal("partial generation presented as complete", err)
	}
	data, err := l.encode(epoch.Add(SignalLifetime))
	if err != nil {
		t.Fatal(err)
	}
	l, err = restore(data)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRepository{leg: l}
	s, _ := NewService(r, func() time.Time { return epoch.Add(SignalLifetime) })
	result, err := s.Read(context.Background(), gateway, "leg")
	if err != nil || result.ReplayComplete || len(result.Signals) != 0 || result.Snapshot.Phase != WaitingAnswer {
		t.Fatal("reconnect concealed replay gap", err)
	}
}

func TestUntrustedPayloadAndResourceBudgets(t *testing.T) {
	l := newLeg(t)
	for _, mutate := range []func(*Command){
		func(c *Command) { c.Version++ }, func(c *Command) { c.OperationID = "../secret" },
		func(c *Command) { c.SDP = strings.Repeat("x", MaxSDPBytes+1) },
		func(c *Command) { c.SDP = "\x00" }, func(c *Command) { c.ICE = &ICECandidate{} },
	} {
		c := command(l, Offer, "bad")
		mutate(&c)
		if _, err := l.Apply(client, c, epoch); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	for i := 0; i < MaxCandidates; i++ {
		c := command(l, Candidate, "ice-"+strings.Repeat("x", 1))
		// IDs are distinct; candidate content may legitimately repeat.
		c.OperationID = stringID(i)
		l = apply(t, l, client, c)
	}
	if _, err := l.Apply(client, command(l, Candidate, "overflow"), epoch); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	l = apply(t, l, client, command(l, Close, "cancel-after-overflow"))
	if l.phase != Closed {
		t.Fatal("budget blocked cancellation")
	}
}
func stringID(i int) string { return string(rune('a'+i/26)) + string(rune('a'+i%26)) }

func TestInputLegIsImmutableOnSuccess(t *testing.T) {
	l := newLeg(t)
	next := apply(t, l, client, command(l, Candidate, "ice"))
	if len(l.receipts) != 0 || len(l.signals) != 0 || l.revision != 1 || next.revision != 2 {
		t.Fatal("reducer mutated prior checkpoint")
	}
}

func TestCoordinatorRenewalCannotReviveAnExpiredOrRevokedGrant(t *testing.T) {
	l := negotiated(t)
	next, err := l.ExtendAuthorization(epoch.Add(50*time.Second), epoch.Add(110*time.Second))
	if err != nil || next.authorizedUntil != epoch.Add(110*time.Second) || next.negotiationUntil != l.negotiationUntil {
		t.Fatal("bounded renewal failed", err)
	}
	if _, err := l.ExtendAuthorization(epoch.Add(time.Minute), epoch.Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatal("expired grant revived", err)
	}
	if _, err := l.ExtendAuthorization(epoch.Add(time.Second), epoch.Add(2*time.Minute)); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded authorization accepted", err)
	}
	next, changed := next.Revoke(epoch.Add(51 * time.Second))
	if !changed || next.phase != Closed || len(next.signals) != 0 {
		t.Fatal("revocation did not close signaling")
	}
	if _, err := next.ExtendAuthorization(epoch.Add(52*time.Second), epoch.Add(112*time.Second)); !errors.Is(err, ErrClosed) {
		t.Fatal("revoked grant revived", err)
	}
	data, err := next.encode(epoch.Add(52 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restore(data)
	if err != nil || loaded.reason != "authorization_revoked" {
		t.Fatal("revocation lost across checkpoint", err)
	}
}

func FuzzCommandsCannotCrossBindingsOrReviveClosedLeg(f *testing.F) {
	f.Add("owner", "client", "call", "offer", "payload")
	f.Add("owner", "gateway", "other-call", "answer", "payload")
	f.Fuzz(func(t *testing.T, owner, device, call, kind, payload string) {
		l := newLeg(t)
		c := command(l, Kind(kind), "fuzz")
		c.CallID, c.SDP = call, payload
		p := Principal{owner, device}
		result, err := l.Apply(p, c, epoch)
		if err == nil && (owner != "owner" || device != "client" && device != "gateway" || call != "call") {
			t.Fatal("scope bypass")
		}
		closed := apply(t, l, client, command(l, Close, "cancel"))
		result, err = closed.Apply(p, c, epoch)
		if err == nil && result.Leg.phase != Closed {
			t.Fatal("terminal leg revived")
		}
	})
}
