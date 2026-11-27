// Package redisstock menyimpan stok di Redis dan menguranginya lewat skrip
// Lua, jadi cek stok dan pengurangannya terjadi dalam satu langkah atomik.
// Setiap tiket yang terbit dicatat ke stream "orders", lalu worker di
// package persist yang menyimpannya ke Postgres.
package redisstock

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"wartiket/internal/tiket"
)

const (
	OrdersStream = "orders"
	PersistGroup = "persist"
)

// Stream "orders" sengaja tidak pakai hash tag event, jadi skrip ini menyentuh
// key di slot yang berbeda. Di Redis single node tidak masalah. Kalau nanti
// pindah ke Redis Cluster, stream-nya perlu dipecah per event, misalnya
// "ev:{id}:orders".
var reserveScript = redis.NewScript(`
-- KEYS[1] stok, KEYS[2] hash pembeli, KEYS[3] stream pesanan
-- ARGV[1] event_id, ARGV[2] user_id, ARGV[3] ticket_id baru, ARGV[4] strategi
if redis.call('EXISTS', KEYS[1]) == 0 then
  return {-1, ''}                          -- event tidak ada
end
local existing = redis.call('HGET', KEYS[2], ARGV[2])
if existing then
  return {1, existing}                     -- beli ulang: tiket lama
end
local left = redis.call('DECR', KEYS[1])
if left < 0 then
  redis.call('INCR', KEYS[1])
  return {0, ''}                           -- habis
end
redis.call('HSET', KEYS[2], ARGV[2], ARGV[3])
redis.call('XADD', KEYS[3], '*',
  'event_id', ARGV[1], 'user_id', ARGV[2], 'ticket_id', ARGV[3], 'strategy', ARGV[4])
return {2, ARGV[3]}                        -- tiket baru
`)

type Store struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Store { return &Store{rdb: rdb} }

func StockKey(eventID int64) string  { return fmt.Sprintf("ev:{%d}:stock", eventID) }
func BuyersKey(eventID int64) string { return fmt.Sprintf("ev:{%d}:buyers", eventID) }

func (s *Store) Reserve(ctx context.Context, eventID int64, userID string, st tiket.Strategy) (tiket.PurchaseResult, error) {
	newID := tiket.NewID()
	res, err := reserveScript.Run(ctx, s.rdb,
		[]string{StockKey(eventID), BuyersKey(eventID), OrdersStream},
		eventID, userID, newID, string(st),
	).Slice()
	if err != nil {
		return tiket.PurchaseResult{}, err
	}
	code, _ := res[0].(int64)
	ticketID, _ := res[1].(string)
	t := tiket.Ticket{ID: ticketID, EventID: eventID, UserID: userID, Strategy: st}
	switch code {
	case -1:
		return tiket.PurchaseResult{}, tiket.ErrEventNotFound
	case 0:
		return tiket.PurchaseResult{}, tiket.ErrSoldOut
	case 1:
		return tiket.PurchaseResult{Ticket: t, Replayed: true}, nil
	case 2:
		return tiket.PurchaseResult{Ticket: t}, nil
	}
	return tiket.PurchaseResult{}, fmt.Errorf("kode skrip reserve tidak dikenal: %v", res[0])
}

// ExistingTicket dipakai waiting room supaya user yang sudah punya tiket
// tidak perlu antre lagi.
func (s *Store) ExistingTicket(ctx context.Context, eventID int64, userID string) (string, bool, error) {
	id, err := s.rdb.HGet(ctx, BuyersKey(eventID), userID).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return id, err == nil, err
}

// Stock mengembalikan stok di Redis. ok bernilai false kalau event ini belum
// pernah di-reset.
func (s *Store) Stock(ctx context.Context, eventID int64) (int, bool, error) {
	n, err := s.rdb.Get(ctx, StockKey(eventID)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// PersistLag menghitung pesanan yang belum tersimpan ke Postgres, yaitu pesan
// yang belum dibaca worker ditambah yang sudah dibaca tapi belum di-ACK.
func (s *Store) PersistLag(ctx context.Context) (int64, error) {
	groups, err := s.rdb.XInfoGroups(ctx, OrdersStream).Result()
	if err != nil {
		if isNoSuchKey(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, g := range groups {
		if g.Name == PersistGroup {
			return g.Lag + g.Pending, nil
		}
	}
	return s.rdb.XLen(ctx, OrdersStream).Result()
}

func (s *Store) Reset(ctx context.Context, eventID int64, stock int) error {
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, StockKey(eventID), BuyersKey(eventID), OrdersStream)
	pipe.Set(ctx, StockKey(eventID), stock, 0)
	pipe.XGroupCreateMkStream(ctx, OrdersStream, PersistGroup, "$")
	_, err := pipe.Exec(ctx)
	return err
}

func isNoSuchKey(err error) bool {
	return err != nil && (err.Error() == "ERR no such key" || errors.Is(err, redis.Nil))
}
