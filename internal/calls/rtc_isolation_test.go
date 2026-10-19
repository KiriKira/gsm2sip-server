package calls

import (
	"context"
	"errors"
	"testing"
)

func TestARIReconciliationCannotRetireOrPreemptRTCCellularSlot(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	manager, closeARI := readyCallManager(t, database, newFakeARI())
	defer closeARI()
	ctx := context.Background()
	intent, err := manager.CreateIntent(ctx, f.ownerID, f.clientID, CreateIntentRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "15550100200"}, "rtc-isolation-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`UPDATE call_sessions SET control_transport='rtc',state='connecting',expires_at=clock_timestamp()-interval '1 minute' WHERE call_id=$1`, intent.CallID); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func(context.Context) error{manager.reconcile, func(ctx context.Context) error { return manager.failClosed(ctx, "ari_unavailable") }, manager.expireDueCalls} {
		if err = operation(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := beginDurable(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = manager.releaseExpiredReservation(ctx, tx, f.gatewayID); err != nil {
		t.Fatal(err)
	}
	if err = manager.preemptOrRejectSlot(ctx, tx, f.gatewayID); !errors.Is(err, ErrBusy) {
		t.Fatalf("SIP incoming preempted RTC slot: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var state string
	var slots int
	if err = database.QueryRow(`SELECT state FROM call_sessions WHERE call_id=$1`, intent.CallID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err = database.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE call_id=$1`, intent.CallID).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if state != "connecting" || slots != 1 {
		t.Fatal("ARI/SIP touched independently controlled cellular occupancy")
	}
}
