package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"wartiket/internal/config"
	"wartiket/internal/httpapi"
	"wartiket/internal/persist"
	"wartiket/internal/queue"
	"wartiket/internal/redisstock"
	"wartiket/internal/stats"
	"wartiket/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server berhenti", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connectPostgres(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, PoolSize: cfg.RedisPoolSize})
	defer rdb.Close()
	if err := retry(ctx, func() error { return rdb.Ping(ctx).Err() }); err != nil {
		return fmt.Errorf("redis: %w", err)
	}

	db := store.New(pool)
	rs := redisstock.New(rdb)
	rec := stats.NewRecorder()
	q := queue.New(rdb, rs, cfg.QueueWorkers, rec, log)
	api := httpapi.New(cfg, db, rs, q, rec, log)

	go rec.Run(ctx)
	go persist.New(rdb, db, log).Run(ctx)
	go q.Run(ctx)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("server siap", "port", cfg.Port, "strategy", cfg.DefaultStrategy,
			"db_max_conns", cfg.DBMaxConns, "max_inflight", cfg.MaxInflight)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("mematikan server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func connectPostgres(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL: %w", err)
	}
	pc.MaxConns = cfg.DBMaxConns
	pc.MinConns = cfg.DBMaxConns // buka semua koneksi di awal biar run pertama tidak kalah start
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	if err := retry(ctx, func() error { return pool.Ping(ctx) }); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return pool, nil
}

// retry menunggu Postgres atau Redis siap. Waktu container baru naik biasanya
// butuh beberapa detik, jadi dicoba ulang sampai sekitar 30 detik.
func retry(ctx context.Context, f func() error) error {
	var err error
	for i := 0; i < 30; i++ {
		if err = f(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}
