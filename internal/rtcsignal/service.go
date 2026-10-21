package rtcsignal

import (
	"context"
	"errors"
	"time"
)

// Repository is the atomic boundary for the future call-coordinator adapter.
// Update must authorize the current device/session and binding under its lock,
// serialize writers across server processes, and durably commit the returned leg
// (including deduplication receipts and pending signals) before returning nil.
// It must roll back on callback or commit failure. A process-local map is NOT a
// production implementation. Missing and foreign legs both return ErrForbidden.
// No implementation is connected to HTTP until shared SIP/RTC occupancy, ARI
// reconciliation isolation, and gateway winner fencing are integrated.
type Repository interface {
	Update(context.Context, Principal, string, func(Leg) (Leg, error)) error
}

type Service struct {
	repository Repository
	now        func() time.Time
}

type ReadResult struct {
	Snapshot       Snapshot `json:"snapshot"`
	Signals        []Signal `json:"signals"`
	ReplayComplete bool     `json:"replay_complete"`
}

func NewService(repository Repository, now func() time.Time) (*Service, error) {
	if repository == nil || now == nil {
		return nil, errors.New("RTC repository and clock are required")
	}
	return &Service{repository, now}, nil
}

// Apply returns no ACK if persistence fails. A duplicate returns its original
// ACK, never a fresh relay effect. Consumers read the committed authoritative
// snapshot/pending signals rather than treating an old ACK as the current state.
func (s *Service) Apply(ctx context.Context, p Principal, c Command) (Ack, bool, error) {
	var ack Ack
	var duplicate bool
	err := s.repository.Update(ctx, p, c.LegID, func(l Leg) (Leg, error) {
		transition, err := l.Apply(p, c, s.now())
		if err != nil {
			return Leg{}, err
		}
		ack, duplicate = transition.Ack, transition.Duplicate
		return transition.Leg, nil
	})
	if err != nil {
		return Ack{}, false, err
	}
	return ack, duplicate, nil
}

// Read is an authenticated, committed snapshot for reconnect. It deliberately
// returns no replay when the leg ended. A negotiated snapshot with no matching
// description does not prove media recovery; use a new generation under policy.
func (s *Service) Read(ctx context.Context, p Principal, legID string) (ReadResult, error) {
	var result ReadResult
	err := s.repository.Update(ctx, p, legID, func(l Leg) (Leg, error) {
		now := s.now()
		l, _ = l.Expire(now)
		result.Snapshot = l.Snapshot()
		if result.Snapshot.Phase != Closed {
			var err error
			result.Signals, err = l.Pending(p, now)
			result.ReplayComplete = err == nil
			if err != nil && !errors.Is(err, ErrReplayExpired) {
				return Leg{}, err
			}
		}
		return l, nil
	})
	if err != nil {
		return ReadResult{}, err
	}
	if result.Signals == nil {
		result.Signals = []Signal{}
	}
	return result, nil
}
