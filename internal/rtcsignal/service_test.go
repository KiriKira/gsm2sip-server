package rtcsignal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Test-only transaction simulator: no production in-memory persistence path.
type testRepository struct {
	mu          sync.Mutex
	leg         Leg
	commitError error
}

func (r *testRepository) Update(ctx context.Context, p Principal, id string, update func(Leg) (Leg, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := r.leg.member(p); err != nil {
		return err
	}
	if r.leg.binding.LegID != id {
		return ErrForbidden
	}
	next, err := update(r.leg)
	if err != nil {
		return err
	}
	if r.commitError != nil {
		return r.commitError
	}
	r.leg = next
	return nil
}

func TestServiceDoesNotAcknowledgeAnUncommittedOperation(t *testing.T) {
	l := newLeg(t)
	repository := &testRepository{leg: l, commitError: errors.New("durable commit failed")}
	service, err := NewService(repository, func() time.Time { return epoch })
	if err != nil {
		t.Fatal(err)
	}
	c := command(l, Offer, "offer")
	ack, duplicate, err := service.Apply(context.Background(), client, c)
	if err == nil || ack != (Ack{}) || duplicate || repository.leg.revision != 1 {
		t.Fatal("uncommitted ACK escaped")
	}
	repository.commitError = nil
	ack, duplicate, err = service.Apply(context.Background(), client, c)
	if err != nil || duplicate || ack.Revision != 2 || repository.leg.phase != WaitingAnswer {
		t.Fatal("commit did not complete", err)
	}
}

func TestConcurrentDuplicateAndCancellationHaveOneDurableOutcome(t *testing.T) {
	repository := &testRepository{leg: newLeg(t)}
	service, _ := NewService(repository, func() time.Time { return epoch })
	c := command(repository.leg, Offer, "offer")
	var wg sync.WaitGroup
	errorsFound := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ack, _, err := service.Apply(context.Background(), client, c)
			if err != nil {
				errorsFound <- err
			} else if ack.Revision != 2 {
				errorsFound <- errors.New("duplicate revision changed")
			}
		}()
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if repository.leg.revision != 2 || len(repository.leg.receipts) != 1 || len(repository.leg.signals) != 1 {
		t.Fatal("concurrent retry was executed twice")
	}
	answer := command(repository.leg, Answer, "answer")
	cancel := command(repository.leg, Close, "cancel")
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, err := service.Apply(context.Background(), gateway, answer)
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, _, err := service.Apply(context.Background(), client, cancel); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if repository.leg.phase != Closed || len(repository.leg.signals) != 0 {
		t.Fatal("racing answer escaped termination")
	}
}

func TestReconnectSnapshotCommitsTimeoutBeforeReturningTerminalState(t *testing.T) {
	r := &testRepository{leg: negotiated(t)}
	service, _ := NewService(r, func() time.Time { return epoch.Add(time.Minute) })
	result, err := service.Read(context.Background(), client, "leg")
	if err != nil || result.Snapshot.Phase != Closed || result.Snapshot.Reason != "authorization_expired" || len(result.Signals) != 0 || r.leg.phase != Closed {
		t.Fatal("reconnect revived expired authorization", err)
	}
	r = &testRepository{leg: newLeg(t), commitError: errors.New("storage unavailable")}
	service, _ = NewService(r, func() time.Time { return epoch })
	result, err = service.Read(context.Background(), client, "leg")
	if err == nil || result.Snapshot != (Snapshot{}) || result.Signals != nil {
		t.Fatal("uncommitted snapshot escaped")
	}
}
