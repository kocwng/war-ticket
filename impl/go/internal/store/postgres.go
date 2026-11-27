/*
Package store berisi semua query ke Postgres, yaitu strategi naive,
strategi forupdate, dan penyimpanan batch dari worker persist.
*/
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wartiket/internal/tiket"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) GetEvent(ctx context.Context, id int64) (tiket.Event, error) {
	e := tiket.Event{ID: id}
	err := s.pool.QueryRow(ctx,
		`SELECT name, capacity, stock FROM events WHERE id = $1`, id,
	).Scan(&e.Name, &e.Capacity, &e.Stock)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, tiket.ErrEventNotFound
	}
	return e, err
}

/*
PurchaseNaive mengecek stok lalu menguranginya di query terpisah, tanpa
transaksi. Pola ini sering lolos ke production, dan di sini sengaja dipakai
sebagai pembanding.
*/
func (s *Store) PurchaseNaive(ctx context.Context, eventID int64, userID string) (tiket.PurchaseResult, error) {
	if t, ok, err := s.existingTicket(ctx, eventID, userID); err != nil || ok {
		return tiket.PurchaseResult{Ticket: t, Replayed: ok}, err
	}

	var stock int
	err := s.pool.QueryRow(ctx, `SELECT stock FROM events WHERE id = $1`, eventID).Scan(&stock)
	if errors.Is(err, pgx.ErrNoRows) {
		return tiket.PurchaseResult{}, tiket.ErrEventNotFound
	}
	if err != nil {
		return tiket.PurchaseResult{}, err
	}
	if stock <= 0 {
		return tiket.PurchaseResult{}, tiket.ErrSoldOut
	}

	/*
		Di sini celahnya. Selama UPDATE di bawah belum jalan, ratusan request lain
		bisa ikut lolos cek "stock > 0" di atas.
	*/
	if _, err := s.pool.Exec(ctx,
		`UPDATE events SET stock = stock - 1, updated_at = now() WHERE id = $1`, eventID); err != nil {
		return tiket.PurchaseResult{}, err
	}

	t := tiket.Ticket{ID: tiket.NewID(), EventID: eventID, UserID: userID, Strategy: tiket.Naive}
	tag, err := s.pool.Exec(ctx, insertTicket, t.ID, t.EventID, t.UserID, string(t.Strategy))
	if err != nil {
		return tiket.PurchaseResult{}, err
	}
	if tag.RowsAffected() == 0 {
		/*
			User yang sama ternyata sudah dapat tiket lewat request lain yang jalan
			barengan. Stoknya terlanjur berkurang, ini kelemahan lain versi naive.
		*/
		return s.replay(ctx, eventID, userID)
	}
	return tiket.PurchaseResult{Ticket: t}, nil
}

/*
PurchaseForUpdate me-lock baris stok selama transaction. Hasilnya selalu
benar, tapi semua request jadi menunggu lock yang sama.
*/
func (s *Store) PurchaseForUpdate(ctx context.Context, eventID int64, userID string) (tiket.PurchaseResult, error) {
	if t, ok, err := s.existingTicket(ctx, eventID, userID); err != nil || ok {
		return tiket.PurchaseResult{Ticket: t, Replayed: ok}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return tiket.PurchaseResult{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op setelah commit

	var stock int
	err = tx.QueryRow(ctx, `SELECT stock FROM events WHERE id = $1 FOR UPDATE`, eventID).Scan(&stock)
	if errors.Is(err, pgx.ErrNoRows) {
		return tiket.PurchaseResult{}, tiket.ErrEventNotFound
	}
	if err != nil {
		return tiket.PurchaseResult{}, err
	}
	if stock <= 0 {
		return tiket.PurchaseResult{}, tiket.ErrSoldOut
	}

	t := tiket.Ticket{ID: tiket.NewID(), EventID: eventID, UserID: userID, Strategy: tiket.ForUpdate}
	tag, err := tx.Exec(ctx, insertTicket, t.ID, t.EventID, t.UserID, string(t.Strategy))
	if err != nil {
		return tiket.PurchaseResult{}, err
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		return s.replay(ctx, eventID, userID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events SET stock = stock - 1, updated_at = now() WHERE id = $1`, eventID); err != nil {
		return tiket.PurchaseResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return tiket.PurchaseResult{}, err
	}
	return tiket.PurchaseResult{Ticket: t}, nil
}

/*
PersistTickets menyimpan tiket dari strategi Redis dalam satu statement.
Tiket di-insert dengan ON CONFLICT, lalu stok di-decrement sebanyak tiket
yang benar-benar masuk. Jadi kalau message yang sama diproses dua kali,
hasilnya tetap benar (idempotent).
*/
func (s *Store) PersistTickets(ctx context.Context, ts []tiket.Ticket) error {
	if len(ts) == 0 {
		return nil
	}
	ids := make([]string, len(ts))
	events := make([]int64, len(ts))
	users := make([]string, len(ts))
	strategies := make([]string, len(ts))
	for i, t := range ts {
		ids[i], events[i], users[i], strategies[i] = t.ID, t.EventID, t.UserID, string(t.Strategy)
	}
	_, err := s.pool.Exec(ctx, `
		WITH ins AS (
			INSERT INTO tickets (id, event_id, user_id, strategy)
			SELECT u.id::uuid, u.event_id, u.user_id, u.strategy
			FROM unnest($1::text[], $2::bigint[], $3::text[], $4::text[]) AS u(id, event_id, user_id, strategy)
			ON CONFLICT (event_id, user_id) DO NOTHING
			RETURNING event_id
		), cnt AS (
			SELECT event_id, count(*) AS n FROM ins GROUP BY event_id
		)
		UPDATE events e SET stock = e.stock - cnt.n, updated_at = now()
		FROM cnt WHERE e.id = cnt.event_id`,
		ids, events, users, strategies)
	return err
}

// Reset menyiapkan event untuk run benchmark baru.
func (s *Store) Reset(ctx context.Context, eventID int64, stock int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM tickets WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO events (id, name, capacity, stock) VALUES ($1, $2, $3, $3)
		ON CONFLICT (id) DO UPDATE SET capacity = $3, stock = $3, updated_at = now()`,
		eventID, fmt.Sprintf("Event #%d", eventID), stock); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type Counts struct {
	Capacity int
	Stock    int
	Tickets  int
}

func (s *Store) Counts(ctx context.Context, eventID int64) (Counts, error) {
	var c Counts
	err := s.pool.QueryRow(ctx, `
		SELECT e.capacity, e.stock, (SELECT count(*) FROM tickets t WHERE t.event_id = e.id)
		FROM events e WHERE e.id = $1`, eventID,
	).Scan(&c.Capacity, &c.Stock, &c.Tickets)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, tiket.ErrEventNotFound
	}
	return c, err
}

const insertTicket = `
	INSERT INTO tickets (id, event_id, user_id, strategy)
	VALUES ($1::text::uuid, $2, $3, $4)
	ON CONFLICT (event_id, user_id) DO NOTHING`

func (s *Store) existingTicket(ctx context.Context, eventID int64, userID string) (tiket.Ticket, bool, error) {
	t := tiket.Ticket{EventID: eventID, UserID: userID}
	var strategy string
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, strategy FROM tickets WHERE event_id = $1 AND user_id = $2`, eventID, userID,
	).Scan(&t.ID, &strategy)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	t.Strategy = tiket.Strategy(strategy)
	return t, true, nil
}

func (s *Store) replay(ctx context.Context, eventID int64, userID string) (tiket.PurchaseResult, error) {
	t, ok, err := s.existingTicket(ctx, eventID, userID)
	if err != nil {
		return tiket.PurchaseResult{}, err
	}
	if !ok {
		return tiket.PurchaseResult{}, fmt.Errorf("tiket user %s hilang setelah konflik insert", userID)
	}
	return tiket.PurchaseResult{Ticket: t, Replayed: true}, nil
}
