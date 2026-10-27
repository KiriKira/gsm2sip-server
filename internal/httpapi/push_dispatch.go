package httpapi

// Push dispatch. A wire 1 device gets one hint for SMS and calls together. A
// reception v2 device gets call and SMS hints apart, each with its own
// watermark, pacing and workers, so neither a slow endpoint nor an SMS backlog
// holds up a ringing call. Watermarks and events live in the database; what
// is kept in memory only avoids repeating wake-ups, and is safe to lose.
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/push"
)

const (
	pushCallWorkers = 4
	pushSMSWorkers  = 2
	// A call wake-up needs this long left for the phone to fetch the call and
	// ring; with less, the call is not announced.
	pushCallMinTTL         = 3 * time.Second
	pushCallAttempts       = 3
	pushCallAttemptTimeout = 3 * time.Second
	pushCallRetryDelay     = time.Second / 3
	// After a call's attempts all fail, wait this long before trying it again.
	pushCallRetryAfter = time.Second
	pushCallBatch      = 64
	pushSMSTTL         = 24 * time.Hour
	pushCallMemoryAge  = 2 * time.Minute
	pushCallMemoryMax  = 4096
)

type pushDispatcher struct {
	callSlots chan struct{}
	smsSlots  chan struct{}
	kick      chan struct{}

	mu    sync.Mutex
	busy  map[string]bool
	calls map[pushCallKey]pushCallState
}

// pushCallKey names one call's wake-ups to one endpoint generation: a new
// endpoint is told again about calls still ringing.
type pushCallKey struct {
	device     string
	generation int64
	call       string
}

type pushCallState struct {
	revision int64 // highest revision the relay accepted
	retryAt  time.Time
	seen     time.Time
}

func newPushDispatcher() *pushDispatcher {
	return &pushDispatcher{
		callSlots: make(chan struct{}, pushCallWorkers), smsSlots: make(chan struct{}, pushSMSWorkers),
		kick: make(chan struct{}, 1), busy: map[string]bool{}, calls: map[pushCallKey]pushCallState{},
	}
}

// kickWake runs push dispatch and wakes WebSocket connections now, after a
// call change committed, instead of at their next poll.
func (s *Server) kickWake() {
	select {
	case s.pushes.kick <- struct{}{}:
	default:
	}
	s.wsMu.Lock()
	close(s.wsWake)
	s.wsWake = make(chan struct{})
	s.wsMu.Unlock()
}

func (s *Server) wakeSignal() <-chan struct{} {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	return s.wsWake
}

// runPush announces new SMS and call events to registered phones, every
// second and whenever a call changes.
func (s *Server) runPush(ctx context.Context) {
	if s.push == nil {
		return
	}
	tick := time.NewTicker(pushPeriod)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.pushes.kick:
		}
		if err := s.pushPass(ctx, false); err != nil && ctx.Err() == nil {
			s.logger.Warn("push dispatch failed")
		}
	}
}

// pushPending runs one dispatch pass and waits for its wake-ups.
func (s *Server) pushPending(ctx context.Context) error {
	return s.pushPass(ctx, true)
}

type pendingPush struct {
	deviceID, ownerID, endpoint string
	generation                  int64
	version                     int
	smsEnabled, smsDue          bool
	eventCursor, callCursor     int64
	pushedEvent, pushedCall     int64
	failures                    int
}

// pushPass starts the wake-ups that are due. A device's domain is worked by
// one job at a time; jobs left running from an earlier pass are skipped.
func (s *Server) pushPass(ctx context.Context, wait bool) error {
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	rows, err := s.db.QueryContext(queryCtx, `
		SELECT device_id::text, owner_id::text, endpoint, generation, version, sms_enabled, sms_due,
			event_cursor, call_cursor, pushed_event_cursor, pushed_call_cursor, failures FROM (
			SELECT t.device_id, d.owner_id, t.endpoint, t.generation, t.failures, t.pushed_event_cursor, t.pushed_call_cursor,
				COALESCE(r.protocol_version,1) AS version, COALESCE(r.sms_enabled,true) AS sms_enabled, t.next_attempt_at<=now() AS sms_due,
				COALESCE((SELECT MAX(e.cursor) FROM server_events e WHERE e.owner_id=d.owner_id),0) AS event_cursor,
				COALESCE((SELECT MAX(c.cursor) FROM call_events c WHERE c.client_device_id=t.device_id),0) AS call_cursor
			FROM push_endpoints t JOIN devices d ON d.id=t.device_id LEFT JOIN device_reception r ON r.device_id=t.device_id
			WHERE d.state='active'
		) pending
		WHERE (version<>2 AND sms_due AND (event_cursor>pushed_event_cursor OR call_cursor>pushed_call_cursor))
		   OR (version=2 AND (call_cursor>pushed_call_cursor OR (sms_enabled AND sms_due AND event_cursor>pushed_event_cursor)))
		LIMIT $1`, pushBatchLimit)
	if err != nil {
		cancel()
		return err
	}
	var batch []pendingPush
	for rows.Next() {
		var item pendingPush
		if err := rows.Scan(&item.deviceID, &item.ownerID, &item.endpoint, &item.generation, &item.version, &item.smsEnabled, &item.smsDue,
			&item.eventCursor, &item.callCursor, &item.pushedEvent, &item.pushedCall, &item.failures); err != nil {
			rows.Close()
			cancel()
			return err
		}
		batch = append(batch, item)
	}
	err = rows.Err()
	rows.Close()
	cancel()
	if err != nil {
		return err
	}
	s.pushes.forgetOldCalls(time.Now())
	var jobs sync.WaitGroup
	start := func(item pendingPush, domain string, slots chan struct{}, run func(context.Context, pendingPush) error) {
		key := item.deviceID + "/" + domain
		if !s.pushes.claim(key) {
			return
		}
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			defer s.pushes.release(key)
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			if err := run(ctx, item); err != nil && ctx.Err() == nil {
				s.logger.Warn("push wake-up failed", "domain", domain)
			}
		}()
	}
	for _, item := range batch {
		if item.version != 2 {
			slots := s.pushes.smsSlots
			if item.callCursor > item.pushedCall {
				slots = s.pushes.callSlots
			}
			start(item, "legacy", slots, s.pushLegacy)
			continue
		}
		if item.callCursor > item.pushedCall {
			start(item, "call", s.pushes.callSlots, s.pushCalls)
		}
		if item.smsEnabled && item.smsDue && item.eventCursor > item.pushedEvent {
			start(item, "sms", s.pushes.smsSlots, s.pushSMS)
		}
	}
	if wait {
		jobs.Wait()
	}
	return nil
}

func (d *pushDispatcher) claim(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busy[key] {
		return false
	}
	d.busy[key] = true
	return true
}

func (d *pushDispatcher) release(key string) {
	d.mu.Lock()
	delete(d.busy, key)
	d.mu.Unlock()
}

// forgetOldCalls drops what it remembers about calls long past ringing. If it
// still holds too much, it forgets everything: at worst a call is announced
// again, never skipped.
func (d *pushDispatcher) forgetOldCalls(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, state := range d.calls {
		if now.Sub(state.seen) > pushCallMemoryAge {
			delete(d.calls, key)
		}
	}
	if len(d.calls) > pushCallMemoryMax {
		d.calls = map[pushCallKey]pushCallState{}
	}
}

func (d *pushDispatcher) callState(key pushCallKey) pushCallState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[key]
}

func (d *pushDispatcher) setCallState(key pushCallKey, state pushCallState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls[key] = state
}

// pushLegacy sends a wire 1 device one hint covering every new event: "call"
// when a call changed, else "sms".
func (s *Server) pushLegacy(ctx context.Context, item pendingPush) error {
	kind := "sms"
	if item.callCursor > item.pushedCall {
		kind = "call"
	}
	sendCtx, stop := context.WithTimeout(ctx, pushSendTimeout)
	sendErr := s.push.Send(sendCtx, item.endpoint, kind, item.deviceID)
	stop()
	if ctx.Err() != nil {
		return nil
	}
	var err error
	switch {
	case sendErr == nil:
		_, err = s.db.ExecContext(ctx, `
			UPDATE push_endpoints SET pushed_event_cursor=GREATEST(pushed_event_cursor,$2),
				pushed_call_cursor=GREATEST(pushed_call_cursor,$3), failures=0, delivered_at=now()
			WHERE device_id=$1 AND generation=$4`, item.deviceID, item.eventCursor, item.callCursor, item.generation)
	case endpointLost(sendErr):
		// The phone registers a working endpoint again on its next check.
		_, err = s.db.ExecContext(ctx, `DELETE FROM push_endpoints WHERE device_id=$1 AND generation=$2`, item.deviceID, item.generation)
	default:
		_, err = s.db.ExecContext(ctx, `
			UPDATE push_endpoints SET failures=failures+1, next_attempt_at=now()+$2*interval '1 millisecond'
			WHERE device_id=$1 AND generation=$3`, item.deviceID, pushBackoff(item.failures+1).Milliseconds(), item.generation)
	}
	return err
}

func endpointLost(err error) bool {
	return errors.Is(err, push.ErrEndpointGone) || errors.Is(err, push.ErrEndpointRefused) || errors.Is(err, push.ErrInvalidEndpoint)
}

// pushSMS tells a v2 device that messages changed: urgently when one arrived,
// so its notification is not held back, else at normal priority.
func (s *Server) pushSMS(ctx context.Context, item pendingPush) error {
	var received bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM server_events WHERE owner_id=$1 AND cursor>$2 AND cursor<=$3 AND event_type='message.received')`,
		item.ownerID, item.pushedEvent, item.eventCursor).Scan(&received); err != nil {
		return err
	}
	urgency := "normal"
	if received {
		urgency = "high"
	}
	wake := push.Wake{Kind: "sms", DeviceID: item.deviceID, WakeID: fmt.Sprintf("sms-event-%d", item.eventCursor),
		Revision: item.eventCursor, Generation: item.generation, TTL: pushSMSTTL, Urgency: urgency}
	sendCtx, stop := context.WithTimeout(ctx, pushSendTimeout)
	sendErr := s.push.Wake(sendCtx, item.endpoint, wake)
	stop()
	if ctx.Err() != nil {
		return nil
	}
	var err error
	switch {
	case sendErr == nil:
		_, err = s.db.ExecContext(ctx, `UPDATE push_endpoints SET pushed_event_cursor=GREATEST(pushed_event_cursor,$2), failures=0, delivered_at=now()
			WHERE device_id=$1 AND generation=$3`, item.deviceID, item.eventCursor, item.generation)
	case endpointLost(sendErr):
		_, err = s.db.ExecContext(ctx, `DELETE FROM push_endpoints WHERE device_id=$1 AND generation=$2`, item.deviceID, item.generation)
	default:
		_, err = s.db.ExecContext(ctx, `UPDATE push_endpoints SET failures=failures+1, next_attempt_at=now()+$2*interval '1 millisecond'
			WHERE device_id=$1 AND generation=$3`, item.deviceID, pushBackoff(item.failures+1).Milliseconds(), item.generation)
	}
	return err
}

type pushCallEvent struct {
	cursor int64
	callID string
}

type pushCall struct {
	revision, cursor int64
	incoming         bool
	remaining        time.Duration
	done             bool
}

// pushCalls announces each incoming call that changed since the watermark,
// with what is left of its ring window as TTL. The watermark moves past the
// events of calls announced, already announced, or no longer worth it; a call
// whose wake-ups keep failing holds it until its window runs out, while the
// calls around it are announced once.
func (s *Server) pushCalls(ctx context.Context, item pendingPush) error {
	rows, err := s.db.QueryContext(ctx, `SELECT e.cursor, e.call_id::text, e.state_revision, c.direction='incoming',
          COALESCE(GREATEST(0,floor(EXTRACT(EPOCH FROM c.expires_at-clock_timestamp())*1000)),0)::bigint
        FROM call_events e JOIN call_sessions c ON c.call_id=e.call_id
        WHERE e.client_device_id=$1 AND e.cursor>$2 ORDER BY e.cursor LIMIT $3`, item.deviceID, item.pushedCall, pushCallBatch)
	if err != nil {
		return err
	}
	read := time.Now()
	var events []pushCallEvent
	var order []string
	calls := map[string]*pushCall{}
	for rows.Next() {
		var event pushCallEvent
		var revision, remaining int64
		var incoming bool
		if err = rows.Scan(&event.cursor, &event.callID, &revision, &incoming, &remaining); err != nil {
			break
		}
		events = append(events, event)
		call, ok := calls[event.callID]
		if !ok {
			call = &pushCall{}
			calls[event.callID] = call
			order = append(order, event.callID)
		}
		call.revision = max(call.revision, revision)
		call.cursor = event.cursor
		call.incoming = incoming
		call.remaining = time.Duration(remaining) * time.Millisecond
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var accepted, failed bool
	for _, id := range order {
		call := calls[id]
		deadline := read.Add(call.remaining)
		key := pushCallKey{item.deviceID, item.generation, id}
		state := s.pushes.callState(key)
		switch {
		case !call.incoming || time.Until(deadline) < pushCallMinTTL || state.revision >= call.revision:
			// Outgoing calls are placed from the screen; nothing to announce.
			call.done = true
			continue
		case time.Now().Before(state.retryAt):
			continue
		}
		sendErr := s.pushCallWake(ctx, item, id, call, deadline)
		switch {
		case ctx.Err() != nil:
			return nil
		case sendErr == nil:
			s.pushes.setCallState(key, pushCallState{revision: call.revision, seen: time.Now()})
			call.done, accepted = true, true
		case errors.Is(sendErr, errCallWindowOver):
			call.done = true
		case endpointLost(sendErr):
			_, err = s.db.ExecContext(ctx, `DELETE FROM push_endpoints WHERE device_id=$1 AND generation=$2`, item.deviceID, item.generation)
			return err
		default:
			state.retryAt, state.seen = time.Now().Add(pushCallRetryAfter), time.Now()
			s.pushes.setCallState(key, state)
			failed = true
		}
	}
	watermark := item.pushedCall
	for _, event := range events {
		if !calls[event.callID].done {
			break
		}
		watermark = event.cursor
	}
	_, err = s.db.ExecContext(ctx, `UPDATE push_endpoints SET pushed_call_cursor=GREATEST(pushed_call_cursor,$2),
          call_failures=CASE WHEN $3 THEN call_failures+1 WHEN $4 THEN 0 ELSE call_failures END,
          delivered_at=CASE WHEN $4 THEN now() ELSE delivered_at END
        WHERE device_id=$1 AND generation=$5`, item.deviceID, watermark, failed, accepted, item.generation)
	return err
}

var errCallWindowOver = errors.New("call ring window is over")

// pushCallWake tries a call's wake-up a few times, quickly, while enough of
// its ring window is left; each attempt's TTL is what remains.
func (s *Server) pushCallWake(ctx context.Context, item pendingPush, id string, call *pushCall, deadline time.Time) error {
	var err error
	for attempt := 0; attempt < pushCallAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pushCallRetryDelay):
			}
		}
		left := time.Until(deadline)
		if left < pushCallMinTTL {
			return errCallWindowOver
		}
		wake := push.Wake{Kind: "call", DeviceID: item.deviceID, WakeID: fmt.Sprintf("call-event-%d", call.cursor), CallID: id,
			Revision: call.revision, Generation: item.generation, TTL: left, Urgency: "high"}
		sendCtx, stop := context.WithTimeout(ctx, min(pushCallAttemptTimeout, left-pushCallMinTTL))
		err = s.push.Wake(sendCtx, item.endpoint, wake)
		stop()
		if err == nil || endpointLost(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// pushEndpointStatus is what GET push-endpoint reports.
func scanPushStatus(row *sql.Row) (map[string]any, error) {
	var failures, callFailures int
	var generation int64
	var delivered sql.NullTime
	if err := row.Scan(&failures, &callFailures, &generation, &delivered); err != nil {
		return nil, err
	}
	out := map[string]any{"registered": true, "failures": failures, "call_failures": callFailures, "endpoint_generation": generation}
	if delivered.Valid {
		out["delivered_at"] = delivered.Time.UTC().Format(time.RFC3339)
	}
	return out, nil
}
