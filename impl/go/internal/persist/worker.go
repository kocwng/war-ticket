// Package persist memindahkan tiket dari stream Redis ke Postgres.
//
// Setiap pesan dijamin diproses minimal sekali. Pesan baru di-ACK setelah
// tiketnya tersimpan, dan insert-nya pakai ON CONFLICT sehingga aman diulang.
// Kalau worker mati di tengah jalan, waktu hidup lagi dia membaca dulu pesan
// pending miliknya sebelum lanjut ke pesan baru.
package persist

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"wartiket/internal/redisstock"
	"wartiket/internal/tiket"
)

type Saver interface {
	PersistTickets(ctx context.Context, ts []tiket.Ticket) error
}

type Worker struct {
	rdb      *redis.Client
	saver    Saver
	log      *slog.Logger
	consumer string
}

func New(rdb *redis.Client, saver Saver, log *slog.Logger) *Worker {
	host, _ := os.Hostname()
	return &Worker{rdb: rdb, saver: saver, log: log, consumer: "go-" + host}
}

func (w *Worker) Run(ctx context.Context) {
	w.ensureGroup(ctx)
	lastID := "0" // mulai dari pesan pending milik consumer ini
	backoff := 100 * time.Millisecond

	for ctx.Err() == nil {
		streams, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    redisstock.PersistGroup,
			Consumer: w.consumer,
			Streams:  []string{redisstock.OrdersStream, lastID},
			Count:    500,
			Block:    time.Second,
		}).Result()
		switch {
		case errors.Is(err, redis.Nil):
			lastID = ">"
			continue
		case err != nil && (strings.HasPrefix(err.Error(), "NOGROUP") || strings.HasPrefix(err.Error(), "UNBLOCKED")):
			// Stream dihapus oleh /admin/reset. Buat ulang grupnya.
			w.ensureGroup(ctx)
			lastID = "0"
			continue
		case err != nil:
			if ctx.Err() == nil {
				w.log.Warn("persist: gagal membaca stream", "err", err)
				sleep(ctx, backoff)
			}
			continue
		}

		msgs := streams[0].Messages
		if len(msgs) == 0 {
			lastID = ">" // pending sudah habis, lanjut ke pesan baru
			continue
		}

		tickets := make([]tiket.Ticket, 0, len(msgs))
		ids := make([]string, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
			if t, ok := parse(m.Values); ok {
				tickets = append(tickets, t)
			} else {
				w.log.Error("persist: pesan rusak, dilewati", "id", m.ID)
			}
		}

		if err := w.saver.PersistTickets(ctx, tickets); err != nil {
			if ctx.Err() == nil {
				w.log.Warn("persist: gagal menulis ke Postgres, dicoba lagi", "n", len(tickets), "err", err)
				sleep(ctx, backoff)
				backoff = min(backoff*2, 5*time.Second)
			}
			lastID = "0" // baca ulang pesan pending yang sama
			continue
		}
		backoff = 100 * time.Millisecond
		if err := w.rdb.XAck(ctx, redisstock.OrdersStream, redisstock.PersistGroup, ids...).Err(); err != nil {
			w.log.Warn("persist: gagal ACK", "err", err)
		}
	}
}

func (w *Worker) ensureGroup(ctx context.Context) {
	err := w.rdb.XGroupCreateMkStream(ctx, redisstock.OrdersStream, redisstock.PersistGroup, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") && ctx.Err() == nil {
		w.log.Warn("persist: gagal membuat consumer group", "err", err)
	}
}

func parse(v map[string]any) (tiket.Ticket, bool) {
	ev, err := strconv.ParseInt(str(v["event_id"]), 10, 64)
	t := tiket.Ticket{
		ID:       str(v["ticket_id"]),
		EventID:  ev,
		UserID:   str(v["user_id"]),
		Strategy: tiket.Strategy(str(v["strategy"])),
	}
	return t, err == nil && t.ID != "" && t.UserID != ""
}

func str(v any) string { s, _ := v.(string); return s }

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
