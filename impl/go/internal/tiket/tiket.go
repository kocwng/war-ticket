// Package tiket berisi tipe-tipe dasar yang dipakai semua strategi.
package tiket

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// Strategy adalah cara backend menjaga stok saat war tiket.
type Strategy string

const (
	Naive     Strategy = "naive"     // SELECT lalu UPDATE terpisah (sengaja bocor)
	ForUpdate Strategy = "forupdate" // row lock di Postgres
	Redis     Strategy = "redis"     // atomic DECR via Lua script, persist async
	Queue     Strategy = "queue"     // waiting room di depan request path Redis
)

func ParseStrategy(s string) (Strategy, error) {
	switch st := Strategy(s); st {
	case Naive, ForUpdate, Redis, Queue:
		return st, nil
	}
	return "", fmt.Errorf("strategi %q tidak dikenal (naive|forupdate|redis|queue)", s)
}

var (
	ErrSoldOut       = errors.New("tiket habis")
	ErrEventNotFound = errors.New("event tidak ditemukan")
	ErrQueueNotFound = errors.New("queue entry tidak ditemukan")
)

type Ticket struct {
	ID       string   `json:"ticket_id"`
	EventID  int64    `json:"event_id"`
	UserID   string   `json:"user_id"`
	Strategy Strategy `json:"strategy"`
}

/*
PurchaseResult juga mencatat apakah tiketnya baru, atau tiket lama milik
user yang menekan Beli lagi.
*/
type PurchaseResult struct {
	Ticket   Ticket
	Replayed bool
}

type Event struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Capacity int      `json:"capacity"`
	Stock    int      `json:"stock"`
	Strategy Strategy `json:"strategy"`
}

// NewID membuat UUID v4 sendiri supaya tidak perlu library tambahan.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
