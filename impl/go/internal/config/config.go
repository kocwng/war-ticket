// Package config membaca konfigurasi dari environment variable.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"wartiket/internal/tiket"
)

type Config struct {
	Port            string
	DatabaseURL     string
	RedisAddr       string
	DBMaxConns      int32
	RedisPoolSize   int
	RequestTimeout  time.Duration
	MaxInflight     int // 0 = tanpa load shedding
	QueueWorkers    int
	AdminToken      string
	DefaultStrategy tiket.Strategy
}

func Load() (Config, error) {
	c := Config{
		Port:        env("PORT", "8080"),
		DatabaseURL: env("DATABASE_URL", "postgres://tiket:tiket@localhost:55432/tiket?sslmode=disable"),
		RedisAddr:   env("REDIS_ADDR", "localhost:56379"),
		AdminToken:  os.Getenv("ADMIN_TOKEN"),
	}
	var err error
	if c.DBMaxConns, err = envInt[int32]("DB_MAX_CONNS", 20); err != nil {
		return c, err
	}
	if c.RedisPoolSize, err = envInt[int]("REDIS_POOL_SIZE", 256); err != nil {
		return c, err
	}
	if c.MaxInflight, err = envInt[int]("MAX_INFLIGHT", 1000); err != nil {
		return c, err
	}
	if c.QueueWorkers, err = envInt[int]("QUEUE_WORKERS", 64); err != nil {
		return c, err
	}
	if c.RequestTimeout, err = time.ParseDuration(env("REQUEST_TIMEOUT", "2s")); err != nil {
		return c, fmt.Errorf("REQUEST_TIMEOUT: %w", err)
	}
	if c.DefaultStrategy, err = tiket.ParseStrategy(env("DEFAULT_STRATEGY", "redis")); err != nil {
		return c, err
	}
	if c.QueueWorkers >= c.RedisPoolSize {
		return c, fmt.Errorf("QUEUE_WORKERS (%d) harus lebih kecil dari REDIS_POOL_SIZE (%d), karena tiap worker memegang satu koneksi Redis selama BLPOP", c.QueueWorkers, c.RedisPoolSize)
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt[T ~int | ~int32](k string, def T) (T, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def, fmt.Errorf("%s harus angka bulat >= 0, tapi isinya %q", k, v)
	}
	return T(n), nil
}
