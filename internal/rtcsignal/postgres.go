package rtcsignal

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Authorizer must verify current session/device, call engine, participant scope,
// shared gateway occupancy, SIM revision and the coordinator's authorization.
// It runs inside the transaction, after the leg row is locked. There is no
// permissive default. The HTTP audio adapter supplies the call/slot authorizer.
type Authorizer interface {
	Authorize(context.Context, *sql.Tx, Principal, Binding) error
}

type PostgresRepository struct {
	db         *sql.DB
	authorizer Authorizer
	now        func() time.Time
}

func NewPostgresRepository(database *sql.DB, authorizer Authorizer, now func() time.Time) (*PostgresRepository, error) {
	if database == nil || authorizer == nil || now == nil {
		return nil, errors.New("RTC database, authorizer and clock are required")
	}
	return &PostgresRepository{database, authorizer, now}, nil
}

// Create is coordinator-only, never an endpoint accepting a client-supplied
// Binding. It does not create a business call, reserve a gateway or dial a SIM.
func (r *PostgresRepository) Create(ctx context.Context, p Principal, binding Binding, authorizedUntil time.Time) error {
	now := r.now()
	l, err := New(binding, now, authorizedUntil)
	if err != nil {
		return err
	}
	if _, err := l.member(p); err != nil {
		return err
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.authorizer.Authorize(ctx, tx, p, binding); err != nil {
		return err
	}
	data, err := l.encode(now)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rtc_signal_legs(leg_id,owner_id,checkpoint,next_expiry) VALUES($1,$2,$3,$4)`,
		binding.LegID, binding.OwnerID, data, l.nextExpiry(now)); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return ErrConflict
		}
		return err
	}
	return tx.Commit()
}

func (r *PostgresRepository) Update(ctx context.Context, p Principal, legID string, update func(Leg) (Leg, error)) error {
	if !validID(legID) || !validID(p.OwnerID) || !validID(p.DeviceID) || update == nil {
		return ErrInvalid
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT checkpoint FROM rtc_signal_legs WHERE leg_id=$1 AND owner_id::text=$2 FOR UPDATE`, legID, p.OwnerID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	l, err := restore(data)
	if err != nil {
		return err
	}
	if l.binding.LegID != legID || l.binding.OwnerID != p.OwnerID {
		return ErrForbidden
	}
	if _, err := l.member(p); err != nil {
		return err
	}
	if err := r.authorizer.Authorize(ctx, tx, p, l.binding); err != nil {
		return err
	}
	next, err := update(l)
	if err != nil {
		return err
	}
	// The transaction callback may change negotiation state, never identity.
	if next.binding != l.binding {
		return ErrForbidden
	}
	if err := r.save(ctx, tx, next, r.now()); err != nil {
		return err
	}
	return tx.Commit()
}

// Sweep removes expired SDP/ICE and closes expired authorizations in bounded
// transactions. Start the sweeper BEFORE enabling any future RTC admission;
// next_expiry is indexed and SKIP LOCKED permits multiple workers safely.
// Closing a signaling leg does not release a physical cellular call slot.
func (r *PostgresRepository) Sweep(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalid
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := r.now()
	rows, err := tx.QueryContext(ctx, `SELECT checkpoint FROM rtc_signal_legs WHERE next_expiry <= $1 ORDER BY next_expiry LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return 0, err
	}
	var legs []Leg
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return 0, err
		}
		l, err := restore(data)
		if err != nil {
			rows.Close()
			return 0, err
		}
		l, _ = l.Expire(now)
		legs = append(legs, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, l := range legs {
		if err := r.save(ctx, tx, l, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(legs), nil
}

func (r *PostgresRepository) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (r *PostgresRepository) save(ctx context.Context, tx *sql.Tx, l Leg, now time.Time) error {
	data, err := l.encode(now)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE rtc_signal_legs SET checkpoint=$1,next_expiry=$2,updated_at=$3 WHERE leg_id=$4 AND owner_id::text=$5`,
		data, l.nextExpiry(now), now, l.binding.LegID, l.binding.OwnerID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrForbidden
	}
	return nil
}

var _ Repository = (*PostgresRepository)(nil)
