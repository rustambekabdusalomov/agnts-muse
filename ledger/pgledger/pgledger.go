// Package pgledger is a PostgreSQL budget.Store, safe for any number of
// processes sharing one database.
//
// Every operation is one transaction that locks the accounts it touches in
// a fixed order (by id), so concurrent holds serialize per account without
// deadlocking, and a CHECK constraint (total >= held) makes overspending
// impossible even for writes that bypass this package. Reap uses SKIP
// LOCKED, so several servers can expire holds at once without contending.
package pgledger

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

//go:embed schema.sql
var schema string

// DB is what the ledger needs from a pool (*pgxpool.Pool satisfies it).
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Ledger is a PostgreSQL budget.Store.
type Ledger struct {
	db  DB
	now func() time.Time
}

var _ budget.Store = (*Ledger)(nil)

// Option configures a Ledger.
type Option func(*Ledger)

// WithClock sets the clock hold expiry uses (time.Now by default). The
// database clock is not used, so every server must keep time with NTP.
func WithClock(now func() time.Time) Option { return func(l *Ledger) { l.now = now } }

// New returns a ledger on db. Call Migrate once before use.
func New(db DB, opts ...Option) *Ledger {
	l := &Ledger{db: db, now: time.Now}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Migrate creates the ledger's tables if they do not exist. It takes an
// advisory lock, so servers starting together migrate once.
func (l *Ledger) Migrate(ctx context.Context) error {
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // after Commit it is a no-op
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agnts-muse/pgledger'))`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, schema); err != nil {
		return fmt.Errorf("pgledger: migrate: %w", err)
	}
	return tx.Commit(ctx)
}

// tx runs fn in a transaction.
func (l *Ledger) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // after Commit it is a no-op
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lock creates missing accounts and locks them in id order, returning
// each one's available money in the order of ids.
func lock(ctx context.Context, tx pgx.Tx, ids []budget.AccountID) ([]money.USD, error) {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	// Create and lock in one global order (by id), so two transactions
	// never wait on each other's accounts in opposite orders.
	names = slices.Sorted(slices.Values(names))
	if _, err := tx.Exec(ctx, `INSERT INTO muse_accounts (id) SELECT unnest($1::text[]) ON CONFLICT (id) DO NOTHING`, names); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id, total - held FROM muse_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, names)
	if err != nil {
		return nil, err
	}
	avail := map[string]money.USD{}
	for rows.Next() {
		var id string
		var a int64
		if err := rows.Scan(&id, &a); err != nil {
			return nil, err
		}
		avail[id] = money.USD(a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]money.USD, len(ids))
	for i, id := range ids {
		out[i] = avail[string(id)]
	}
	return out, nil
}

// reserve allocates amount across payers in order and against every cap,
// after locking them. It returns the legs, in payer-then-cap order.
func reserve(ctx context.Context, tx pgx.Tx, payers, caps []budget.AccountID, amount money.USD) ([]budget.Leg, error) {
	avail, err := lock(ctx, tx, append(append([]budget.AccountID{}, payers...), caps...))
	if err != nil {
		return nil, err
	}
	parts, ok := budget.Split(avail[:len(payers)], amount)
	if !ok {
		return nil, fmt.Errorf("%w: need %v", budget.ErrInsufficient, amount)
	}
	for i, c := range caps {
		if avail[len(payers)+i] < amount {
			return nil, fmt.Errorf("%w: %s", budget.ErrCapReached, c)
		}
	}
	legs := make([]budget.Leg, 0, len(payers)+len(caps))
	for i, p := range payers {
		legs = append(legs, budget.Leg{Account: p, Held: parts[i]})
	}
	for _, c := range caps {
		legs = append(legs, budget.Leg{Account: c, Cap: true, Held: amount})
	}
	ids, deltas := make([]string, len(legs)), make([]int64, len(legs))
	for i, leg := range legs {
		ids[i], deltas[i] = string(leg.Account), int64(leg.Held)
	}
	_, err = tx.Exec(ctx, `UPDATE muse_accounts a SET held = a.held + v.d, updated_at = now()
		FROM unnest($1::text[], $2::bigint[]) AS v(id, d) WHERE a.id = v.id`, ids, deltas)
	return legs, err
}

// Hold implements budget.Ledger.
func (l *Ledger) Hold(ctx context.Context, req budget.HoldRequest) (budget.Hold, error) {
	if err := req.Validate(); err != nil {
		return budget.Hold{}, err
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = budget.DefaultTTL
	}
	if h, err := l.byKey(ctx, l.db, req.Key); err == nil {
		return h, nil
	} else if !errors.Is(err, budget.ErrNotFound) {
		return budget.Hold{}, err
	}
	var h budget.Hold
	err := l.tx(ctx, func(tx pgx.Tx) error {
		legs, err := reserve(ctx, tx, req.Payers, req.Caps, req.Amount)
		if err != nil {
			return err
		}
		exp := l.now().Add(ttl)
		var id int64
		err = tx.QueryRow(ctx, `INSERT INTO muse_holds (key, state, amount, ttl_ms, expires_at) VALUES ($1, 'open', $2, $3, $4)
			ON CONFLICT (key) DO NOTHING RETURNING id`, req.Key, int64(req.Amount), ttl.Milliseconds(), exp).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errKeyRace // a concurrent Hold with the same key won
		}
		if err != nil {
			return err
		}
		if err := insertLegs(ctx, tx, id, legs); err != nil {
			return err
		}
		h = budget.Hold{ID: holdID(id), Key: req.Key, State: budget.StateOpen, Amount: req.Amount, Legs: legs, ExpiresAt: exp}
		return nil
	})
	if errors.Is(err, errKeyRace) {
		return l.byKey(ctx, l.db, req.Key)
	}
	return h, err
}

var errKeyRace = errors.New("pgledger: key taken concurrently")

func insertLegs(ctx context.Context, tx pgx.Tx, id int64, legs []budget.Leg) error {
	rows := make([][]any, len(legs))
	for i, leg := range legs {
		rows[i] = []any{id, i, string(leg.Account), leg.Cap, int64(leg.Held)}
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"muse_hold_legs"}, []string{"hold_id", "position", "account_id", "is_cap", "held"}, pgx.CopyFromRows(rows))
	return err
}

// querier is what byKey and load need: a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (l *Ledger) byKey(ctx context.Context, q querier, key string) (budget.Hold, error) {
	var id int64
	if err := q.QueryRow(ctx, `SELECT id FROM muse_holds WHERE key = $1`, key).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return budget.Hold{}, fmt.Errorf("%w: hold key %s", budget.ErrNotFound, key)
		}
		return budget.Hold{}, err
	}
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return budget.Hold{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	h, _, err := load(ctx, tx, id, false)
	return h, err
}

// stored is a hold's row beyond budget.Hold.
type stored struct {
	ttl                         time.Duration
	charged, released, unbilled money.USD
}

// load reads a hold and its legs, locking the hold row when forUpdate.
func load(ctx context.Context, tx pgx.Tx, id int64, forUpdate bool) (budget.Hold, stored, error) {
	q := `SELECT key, state, amount, ttl_ms, expires_at, COALESCE(charged, 0), COALESCE(released, 0), COALESCE(unbilled, 0) FROM muse_holds WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	h := budget.Hold{ID: holdID(id)}
	var st stored
	var amount, ttl, charged, released, unbilled int64
	var state string
	err := tx.QueryRow(ctx, q, id).Scan(&h.Key, &state, &amount, &ttl, &h.ExpiresAt, &charged, &released, &unbilled)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, st, fmt.Errorf("%w: hold %d", budget.ErrNotFound, id)
	}
	if err != nil {
		return h, st, err
	}
	h.State, h.Amount = budget.State(state), money.USD(amount)
	st = stored{ttl: time.Duration(ttl) * time.Millisecond, charged: money.USD(charged), released: money.USD(released), unbilled: money.USD(unbilled)}
	rows, err := tx.Query(ctx, `SELECT account_id, is_cap, held, charged FROM muse_hold_legs WHERE hold_id = $1 ORDER BY position`, id)
	if err != nil {
		return h, st, err
	}
	defer rows.Close()
	for rows.Next() {
		var leg budget.Leg
		var acct string
		var held, ch int64
		if err := rows.Scan(&acct, &leg.Cap, &held, &ch); err != nil {
			return h, st, err
		}
		leg.Account, leg.Held, leg.Charged = budget.AccountID(acct), money.USD(held), money.USD(ch)
		h.Legs = append(h.Legs, leg)
	}
	return h, st, rows.Err()
}

func holdID(id int64) budget.HoldID { return budget.HoldID(strconv.FormatInt(id, 10)) }

func parseID(id budget.HoldID) (int64, error) {
	n, err := strconv.ParseInt(string(id), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: hold %s", budget.ErrNotFound, id)
	}
	return n, nil
}

// openForUpdate locks an open hold, expiring it first if its TTL passed.
// The expiry is written even though the caller then fails with ErrClosed,
// so the transaction must commit: it returns errExpired for that case.
func (l *Ledger) openForUpdate(ctx context.Context, tx pgx.Tx, id budget.HoldID) (budget.Hold, stored, int64, error) {
	n, err := parseID(id)
	if err != nil {
		return budget.Hold{}, stored{}, 0, err
	}
	h, st, err := load(ctx, tx, n, true)
	if err != nil {
		return h, st, n, err
	}
	if h.State == budget.StateOpen && !l.now().Before(h.ExpiresAt) {
		if err := unhold(ctx, tx, n, h, budget.StateExpired); err != nil {
			return h, st, n, err
		}
		return h, st, n, errExpired
	}
	if h.State != budget.StateOpen {
		return h, st, n, fmt.Errorf("%w: hold %s is %s", budget.ErrClosed, id, h.State)
	}
	return h, st, n, nil
}

var errExpired = fmt.Errorf("%w: hold expired", budget.ErrClosed)

// commitThen runs fn in a transaction that commits even when fn returns
// errExpired, so lazy expiry is saved, and then returns that error.
func (l *Ledger) commitThen(ctx context.Context, fn func(pgx.Tx) error) error {
	var expired bool
	err := l.tx(ctx, func(tx pgx.Tx) error {
		err := fn(tx)
		if errors.Is(err, errExpired) {
			expired = true
			return nil
		}
		return err
	})
	if err == nil && expired {
		return errExpired
	}
	return err
}

// Extend implements budget.Ledger.
func (l *Ledger) Extend(ctx context.Context, id budget.HoldID, by money.USD) (budget.Hold, error) {
	if by <= 0 {
		return budget.Hold{}, fmt.Errorf("%w: extend by %v", budget.ErrInvalid, by)
	}
	var out budget.Hold
	err := l.commitThen(ctx, func(tx pgx.Tx) error {
		h, st, n, err := l.openForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		var payers, caps []budget.AccountID
		for _, leg := range h.Legs {
			if leg.Cap {
				caps = append(caps, leg.Account)
			} else {
				payers = append(payers, leg.Account)
			}
		}
		more, err := reserve(ctx, tx, payers, caps, by)
		if err != nil {
			out = h
			return err
		}
		for i := range h.Legs {
			h.Legs[i].Held += more[i].Held
			if _, err := tx.Exec(ctx, `UPDATE muse_hold_legs SET held = $3 WHERE hold_id = $1 AND position = $2`, n, i, int64(h.Legs[i].Held)); err != nil {
				return err
			}
		}
		h.Amount += by
		h.ExpiresAt = l.now().Add(st.ttl)
		if _, err := tx.Exec(ctx, `UPDATE muse_holds SET amount = $2, expires_at = $3 WHERE id = $1`, n, int64(h.Amount), h.ExpiresAt); err != nil {
			return err
		}
		out = h
		return nil
	})
	return out, err
}

// Commit implements budget.Ledger.
func (l *Ledger) Commit(ctx context.Context, id budget.HoldID, actual money.USD) (budget.Receipt, error) {
	if actual < 0 {
		return budget.Receipt{}, fmt.Errorf("%w: commit %v", budget.ErrInvalid, actual)
	}
	var rec budget.Receipt
	err := l.commitThen(ctx, func(tx pgx.Tx) error {
		h, st, n, err := l.openForUpdate(ctx, tx, id)
		if errors.Is(err, budget.ErrClosed) && h.State == budget.StateCommitted {
			rec = budget.Receipt{HoldID: id, Key: h.Key, Charged: st.charged, Released: st.released, Unbilled: st.unbilled, Legs: h.Legs}
			return nil // committing twice returns the first receipt
		}
		if err != nil {
			return err
		}
		legs, charged, unbilled := budget.Settle(h.Legs, actual)
		accts := make([]budget.AccountID, len(legs))
		for i, leg := range legs {
			accts[i] = leg.Account
		}
		if _, err := lock(ctx, tx, accts); err != nil {
			return err
		}
		for i, leg := range legs {
			var after int64
			if err := tx.QueryRow(ctx, `UPDATE muse_accounts SET held = held - $2, total = total - $3, updated_at = now() WHERE id = $1 RETURNING total`,
				string(leg.Account), int64(leg.Held), int64(leg.Charged)).Scan(&after); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE muse_hold_legs SET charged = $3 WHERE hold_id = $1 AND position = $2`, n, i, int64(leg.Charged)); err != nil {
				return err
			}
			if leg.Charged > 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO muse_entries (account_id, kind, amount, balance_after, hold_id) VALUES ($1, 'charge', $2, $3, $4)`,
					string(leg.Account), -int64(leg.Charged), after, n); err != nil {
					return err
				}
			}
		}
		released := h.Amount - charged
		if _, err := tx.Exec(ctx, `UPDATE muse_holds SET state = 'committed', charged = $2, released = $3, unbilled = $4, closed_at = now() WHERE id = $1`,
			n, int64(charged), int64(released), int64(unbilled)); err != nil {
			return err
		}
		rec = budget.Receipt{HoldID: id, Key: h.Key, Charged: charged, Released: released, Unbilled: unbilled, Legs: legs}
		return nil
	})
	return rec, err
}

// Release implements budget.Ledger.
func (l *Ledger) Release(ctx context.Context, id budget.HoldID) error {
	err := l.commitThen(ctx, func(tx pgx.Tx) error {
		h, _, n, err := l.openForUpdate(ctx, tx, id)
		switch {
		case errors.Is(err, budget.ErrClosed) && (h.State == budget.StateReleased || h.State == budget.StateExpired):
			return nil
		case err != nil:
			return err
		}
		return unhold(ctx, tx, n, h, budget.StateReleased)
	})
	if errors.Is(err, errExpired) {
		return nil // releasing an expired hold is a no-op
	}
	return err
}

// unhold returns a hold's reserved money to its accounts.
func unhold(ctx context.Context, tx pgx.Tx, n int64, h budget.Hold, s budget.State) error {
	ids := make([]budget.AccountID, len(h.Legs))
	for i, leg := range h.Legs {
		ids[i] = leg.Account
	}
	if _, err := lock(ctx, tx, ids); err != nil {
		return err
	}
	for _, leg := range h.Legs {
		if _, err := tx.Exec(ctx, `UPDATE muse_accounts SET held = held - $2, updated_at = now() WHERE id = $1`, string(leg.Account), int64(leg.Held)); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE muse_holds SET state = $2, released = amount, closed_at = now() WHERE id = $1`, n, string(s))
	return err
}

// Reap implements budget.Store. Holds another server is working on are
// skipped, not waited for.
func (l *Ledger) Reap(ctx context.Context, now time.Time) (int, error) {
	n := 0
	err := l.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM muse_holds WHERE state = 'open' AND expires_at <= $1 ORDER BY id FOR UPDATE SKIP LOCKED`, now)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, id := range ids {
			h, _, err := load(ctx, tx, id, false)
			if err != nil {
				return err
			}
			if err := unhold(ctx, tx, id, h, budget.StateExpired); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// Deposit implements budget.Funds.
func (l *Ledger) Deposit(ctx context.Context, id budget.AccountID, amount money.USD, key string) (budget.Balance, error) {
	if id == "" || key == "" {
		return budget.Balance{}, fmt.Errorf("%w: deposit needs an account and a key", budget.ErrInvalid)
	}
	var b budget.Balance
	err := l.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lock(ctx, tx, []budget.AccountID{id}); err != nil {
			return err
		}
		var after int64
		err := tx.QueryRow(ctx, `SELECT balance_after FROM muse_entries WHERE kind = 'deposit' AND key = $1`, key).Scan(&after)
		if err == nil {
			b, err = balance(ctx, tx, id)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		cur, err := balance(ctx, tx, id)
		if err != nil {
			return err
		}
		if amount < 0 && cur.Total+amount < cur.Held {
			b = cur
			return fmt.Errorf("%w: %s has %v available", budget.ErrInsufficient, id, cur.Available)
		}
		if _, err := tx.Exec(ctx, `UPDATE muse_accounts SET total = total + $2, updated_at = now() WHERE id = $1`, string(id), int64(amount)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO muse_entries (account_id, kind, amount, balance_after, key) VALUES ($1, 'deposit', $2, $3, $4)`,
			string(id), int64(amount), int64(cur.Total+amount), key); err != nil {
			return err
		}
		b, err = balance(ctx, tx, id)
		return err
	})
	return b, err
}

// Balance implements budget.Funds.
func (l *Ledger) Balance(ctx context.Context, id budget.AccountID) (budget.Balance, error) {
	var total, held int64
	err := l.db.QueryRow(ctx, `SELECT total, held FROM muse_accounts WHERE id = $1`, string(id)).Scan(&total, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return budget.Balance{}, nil
	}
	return budget.Balance{Total: money.USD(total), Held: money.USD(held), Available: money.USD(total - held)}, err
}

func balance(ctx context.Context, tx pgx.Tx, id budget.AccountID) (budget.Balance, error) {
	var total, held int64
	if err := tx.QueryRow(ctx, `SELECT total, held FROM muse_accounts WHERE id = $1`, string(id)).Scan(&total, &held); err != nil {
		return budget.Balance{}, err
	}
	return budget.Balance{Total: money.USD(total), Held: money.USD(held), Available: money.USD(total - held)}, nil
}
