package rtcsignal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/db"
)

func rtcDatabase(t *testing.T) *sql.DB {
	t.Helper()
	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := db.Open(address)
	if err != nil {
		t.Fatal(err)
	}
	schema := "rtc_test_" + strings.ReplaceAll(uuid(t), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	isolated, err := db.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { isolated.Close(); admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`); admin.Close() })
	if err := db.Migrate(context.Background(), isolated); err != nil {
		t.Fatal(err)
	}
	return isolated
}

func uuid(t *testing.T) string {
	t.Helper()
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		t.Fatal(err)
	}
	s := hex.EncodeToString(data[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// Tests only: proves the mandatory authorizer participates in the transaction.
// It is deliberately not a production call/SIM/slot/winner authorizer.
type activeDeviceTestAuthorizer struct{}

func (activeDeviceTestAuthorizer) Authorize(ctx context.Context, tx *sql.Tx, p Principal, b Binding) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM devices WHERE owner_id::text=$1 AND state='active' AND ((id::text=$2 AND role='client') OR (id::text=$3 AND role='gateway'))`, b.OwnerID, b.ClientID, b.GatewayID).Scan(&count)
	if err != nil {
		return err
	}
	if count != 2 || p.OwnerID != b.OwnerID || p.DeviceID != b.ClientID && p.DeviceID != b.GatewayID {
		return ErrForbidden
	}
	return nil
}

func rtcBinding(t *testing.T, database *sql.DB) Binding {
	t.Helper()
	b := Binding{uuid(t), uuid(t), uuid(t), uuid(t), uuid(t), uuid(t), uuid(t), uuid(t), 4}
	if _, err := database.Exec(`INSERT INTO owners(id,display_name) VALUES($1,'RTC test')`, b.OwnerID); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct{ id, role string }{{b.ClientID, "client"}, {b.GatewayID, "gateway"}} {
		if _, err := database.Exec(`INSERT INTO devices(id,owner_id,role,name) VALUES($1,$2,$3,'RTC device')`, d.id, b.OwnerID, d.role); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func boundCommand(l Leg, kind Kind, id string) Command {
	c := command(l, kind, id)
	c.CallID, c.LegID, c.AttemptID = l.binding.CallID, l.binding.LegID, l.binding.AttemptID
	return c
}

func TestPostgresConcurrentReplaySurvivesRepositoryRestart(t *testing.T) {
	database := rtcDatabase(t)
	b := rtcBinding(t, database)
	p := Principal{b.OwnerID, b.ClientID}
	repository, _ := NewPostgresRepository(database, activeDeviceTestAuthorizer{}, func() time.Time { return epoch })
	if err := repository.Create(context.Background(), p, b, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository, func() time.Time { return epoch })
	l, _ := New(b, epoch, epoch.Add(time.Minute))
	c := boundCommand(l, Offer, "offer")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ack, _, err := service.Apply(context.Background(), p, c)
			if err != nil || ack.Revision != 2 {
				t.Errorf("concurrent durable replay: revision=%d err=%v", ack.Revision, err)
			}
		}()
	}
	wg.Wait()
	// New repository/service instances must load the receipt from PostgreSQL.
	restarted, _ := NewPostgresRepository(database, activeDeviceTestAuthorizer{}, func() time.Time { return epoch })
	service, _ = NewService(restarted, func() time.Time { return epoch })
	ack, duplicate, err := service.Apply(context.Background(), p, c)
	if err != nil || !duplicate || ack.Revision != 2 {
		t.Fatal("durable deduplication lost on restart", err)
	}
	c.SDP += "a=changed\r\n"
	if _, _, err := service.Apply(context.Background(), p, c); !errors.Is(err, ErrConflict) {
		t.Fatal("changed retry accepted", err)
	}
	if err := restarted.Update(context.Background(), p, b.LegID, func(l Leg) (Leg, error) {
		if l.revision != 2 || len(l.receipts) != 1 || len(l.signals) != 1 {
			t.Error("operation executed more than once")
		}
		return l, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRollbackRevocationAndOwnerIsolation(t *testing.T) {
	database := rtcDatabase(t)
	b := rtcBinding(t, database)
	p := Principal{b.OwnerID, b.ClientID}
	r, _ := NewPostgresRepository(database, activeDeviceTestAuthorizer{}, func() time.Time { return epoch })
	if err := r.Create(context.Background(), p, b, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(r, func() time.Time { return epoch })
	l, _ := New(b, epoch, epoch.Add(time.Minute))
	c := boundCommand(l, Offer, "offer")
	// A real PostgreSQL constraint failure must roll back the checkpoint and
	// suppress the ACK, even though the reducer already accepted the command.
	if _, err := database.Exec(`ALTER TABLE rtc_signal_legs ADD CONSTRAINT reject_update CHECK(updated_at < '2020-01-01') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if ack, duplicate, err := service.Apply(context.Background(), p, c); err == nil || ack != (Ack{}) || duplicate {
		t.Fatal("failed commit was acknowledged")
	}
	if _, err := database.Exec(`ALTER TABLE rtc_signal_legs DROP CONSTRAINT reject_update`); err != nil {
		t.Fatal(err)
	}
	if ack, duplicate, err := service.Apply(context.Background(), p, c); err != nil || duplicate || ack.Revision != 2 {
		t.Fatal("rollback consumed operation", err)
	}
	foreign := Principal{uuid(t), b.ClientID}
	if _, _, err := service.Apply(context.Background(), foreign, c); !errors.Is(err, ErrForbidden) {
		t.Fatal("owner isolation failed", err)
	}
	if _, err := database.Exec(`UPDATE devices SET state='revoked' WHERE id=$1`, p.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Apply(context.Background(), p, c); !errors.Is(err, ErrForbidden) {
		t.Fatal("revoked device received historical ACK", err)
	}
}

func TestPostgresSweeperErasesSensitivePayloadButRetainsReceipt(t *testing.T) {
	database := rtcDatabase(t)
	b := rtcBinding(t, database)
	p := Principal{b.OwnerID, b.ClientID}
	now := epoch
	r, _ := NewPostgresRepository(database, activeDeviceTestAuthorizer{}, func() time.Time { return now })
	if err := r.Create(context.Background(), p, b, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, _ := NewService(r, func() time.Time { return now })
	l, _ := New(b, now, now.Add(time.Minute))
	c := boundCommand(l, Offer, "offer")
	c.SDP = "v=0\r\na=ice-pwd:sensitive-test-marker\r\n"
	if _, _, err := s.Apply(context.Background(), p, c); err != nil {
		t.Fatal(err)
	}
	now = epoch.Add(SignalLifetime)
	if count, err := r.Sweep(context.Background(), 10); err != nil || count != 1 {
		t.Fatal("signal sweep failed", count, err)
	}
	var data []byte
	if err := database.QueryRow(`SELECT checkpoint FROM rtc_signal_legs WHERE leg_id=$1`, b.LegID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sensitive-test-marker") {
		t.Fatal("expired SDP retained in checkpoint")
	}
	if ack, duplicate, err := s.Apply(context.Background(), p, c); err != nil || !duplicate || ack.Revision != 2 {
		t.Fatal("TTL erased deduplication", err)
	}
	now = epoch.Add(NegotiationLifetime)
	if count, err := r.Sweep(context.Background(), 10); err != nil || count != 1 {
		t.Fatal("negotiation sweep failed", count, err)
	}
	if err := r.Update(context.Background(), p, b.LegID, func(l Leg) (Leg, error) {
		if l.phase != Closed || l.reason != "negotiation_expired" {
			t.Error("timeout did not close leg")
		}
		return l, nil
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := r.Sweep(context.Background(), 10); err != nil || count != 0 {
		t.Fatal("terminal row swept repeatedly", count, err)
	}
}

func TestAdditiveRTCMigrationIsIgnoredByPreviousMigrationFileSet(t *testing.T) {
	database := rtcDatabase(t)
	// The current runner enumerates local files; exercise the pre-0015 file set
	// against an upgraded database. This checks migration-runner compatibility,
	// not the complete binary/data rollback required before a default switch.
	directory := t.TempDir()
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 15 {
		t.Fatalf("expected migration baseline plus RTC migration, got %d", len(files))
	}
	for _, file := range files {
		if filepath.Base(file) >= "0015_" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, filepath.Base(file)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MIGRATIONS_DIR", directory)
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal("old migration file set failed on upgraded DB", err)
	}
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name='0015_rtc_signal_legs.sql'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("old runner modified new migration record", err)
	}
}
