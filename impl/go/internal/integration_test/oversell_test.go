//go:build integration

// Test race condition langsung ke Postgres dan Redis, bukan mock.
// Cara menjalankannya:
//
//	docker compose up -d postgres redis
//	go test -tags integration ./internal/integration_test/ -v
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"wartiket/internal/redisstock"
	"wartiket/internal/store"
	"wartiket/internal/tiket"
)

const (
	eventID = 9001 // terpisah dari event benchmark
	stock   = 50
	buyers  = 400
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func setup(t *testing.T) (*store.Store, *redisstock.Store, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := pgxpool.ParseConfig(env("DATABASE_URL", "postgres://tiket:tiket@localhost:55432/tiket?sslmode=disable"))
	if err != nil {
		t.Fatal(err)
	}
	pc.MaxConns = 40
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil || pool.Ping(ctx) != nil {
		t.Skip("Postgres belum jalan, nyalakan dulu dengan `docker compose up -d postgres redis`")
	}
	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "localhost:56379"), PoolSize: 100})
	if rdb.Ping(ctx).Err() != nil {
		t.Skip("Redis belum jalan")
	}
	t.Cleanup(func() { pool.Close(); rdb.Close() })
	return store.New(pool), redisstock.New(rdb), pool
}

// rush melepas `buyers` goroutine yang membeli bersamaan, lalu menghitung
// berapa tiket baru yang terbit.
func rush(t *testing.T, buy func(ctx context.Context, user string) (tiket.PurchaseResult, error)) int64 {
	t.Helper()
	var created atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := buy(ctx, fmt.Sprintf("it-%d", i))
			switch {
			case err == nil && !res.Replayed:
				created.Add(1)
			case err == nil, errors.Is(err, tiket.ErrSoldOut):
			default:
				t.Errorf("error tak terduga: %v", err)
			}
		}(i)
	}
	close(start) // semua goroutine dilepas bersamaan
	wg.Wait()
	return created.Load()
}

func TestForUpdateNeverOversells(t *testing.T) {
	db, _, _ := setup(t)
	ctx := context.Background()
	if err := db.Reset(ctx, eventID, stock); err != nil {
		t.Fatal(err)
	}
	created := rush(t, func(ctx context.Context, u string) (tiket.PurchaseResult, error) {
		return db.PurchaseForUpdate(ctx, eventID, u)
	})
	c, err := db.Counts(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if created != stock || c.Tickets != stock || c.Stock != 0 {
		t.Fatalf("FOR UPDATE harus tepat %d tiket: created=%d tickets=%d stock=%d", stock, created, c.Tickets, c.Stock)
	}
}

func TestRedisNeverOversells(t *testing.T) {
	db, rs, _ := setup(t)
	ctx := context.Background()
	if err := rs.Reset(ctx, eventID, stock); err != nil {
		t.Fatal(err)
	}
	created := rush(t, func(ctx context.Context, u string) (tiket.PurchaseResult, error) {
		return rs.Reserve(ctx, eventID, u, tiket.Redis)
	})
	left, _, err := rs.Stock(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if created != stock || left != 0 {
		t.Fatalf("Redis harus tepat %d tiket: created=%d stok_redis=%d", stock, created, left)
	}
	_ = db
}

func TestRedisReplayIsIdempotent(t *testing.T) {
	_, rs, _ := setup(t)
	ctx := context.Background()
	if err := rs.Reset(ctx, eventID, 5); err != nil {
		t.Fatal(err)
	}
	first, err := rs.Reserve(ctx, eventID, "same-user", tiket.Redis)
	if err != nil {
		t.Fatal(err)
	}
	again, err := rs.Reserve(ctx, eventID, "same-user", tiket.Redis)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Ticket.ID != first.Ticket.ID {
		t.Fatalf("beli ulang harus mengembalikan tiket yang sama: %+v vs %+v", first, again)
	}
	if left, _, _ := rs.Stock(ctx, eventID); left != 4 {
		t.Fatalf("beli ulang tidak boleh mengurangi stok, sisa=%d", left)
	}
}

// Test ini sengaja tidak pernah gagal. Tujuannya memperlihatkan bahwa versi
// naive bisa oversold. Jumlahnya tergantung timing, jadi cukup dicatat di log.
func TestNaiveCanOversell(t *testing.T) {
	db, _, _ := setup(t)
	ctx := context.Background()
	if err := db.Reset(ctx, eventID, stock); err != nil {
		t.Fatal(err)
	}
	created := rush(t, func(ctx context.Context, u string) (tiket.PurchaseResult, error) {
		return db.PurchaseNaive(ctx, eventID, u)
	})
	t.Logf("naive: %d tiket terbit untuk stok %d (oversold %d)", created, stock, max(0, created-stock))
}
