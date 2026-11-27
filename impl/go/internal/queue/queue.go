/*
Package queue adalah waiting room di depan request path Redis.

Pembeli yang datang di-enqueue ke list "room" dan langsung dapat 202 beserta
queue_id. Di background, sejumlah worker (QUEUE_WORKERS) men-dequeue secara
FIFO, lalu menjalankan reserve yang sama dengan strategi redis. Jadi sebesar
apa pun traffic spike-nya, concurrency ke stok tidak pernah lebih dari
jumlah worker itu.

Semua state disimpan di Redis supaya beberapa instance app bisa berbagi
satu waiting room.
*/
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"wartiket/internal/redisstock"
	"wartiket/internal/tiket"
)

const (
	roomKey  = "room"
	entryTTL = 10 * time.Minute
)

type Status string

const (
	Waiting   Status = "waiting"
	Admitted  Status = "admitted"
	Confirmed Status = "confirmed"
	SoldOut   Status = "sold_out"
)

type Entry struct {
	QueueID  string `json:"queue_id"`
	Status   Status `json:"status"`
	Position *int64 `json:"position,omitempty"`
	TicketID string `json:"ticket_id,omitempty"`
	PollURL  string `json:"poll_url"`
}

/*
Observer dipanggil setiap kali entry di-admit, di-requeue, atau selesai.
Dipakai untuk metrics dan timeline.
*/
type Observer interface {
	QueueAdmitted()
	QueueRequeued()
	QueueDone(created bool)
}

type Queue struct {
	rdb     *redis.Client
	stock   *redisstock.Store
	workers int
	obs     Observer
	log     *slog.Logger
}

func New(rdb *redis.Client, stock *redisstock.Store, workers int, obs Observer, log *slog.Logger) *Queue {
	return &Queue{rdb: rdb, stock: stock, workers: workers, obs: obs, log: log}
}

func seqKey(ev int64) string      { return fmt.Sprintf("room:{%d}:seq", ev) }
func admittedKey(ev int64) string { return fmt.Sprintf("room:{%d}:admitted", ev) }
func entryKey(id string) string   { return "q:" + id }

var enqueueScript = redis.NewScript(`
-- KEYS[1] seq, KEYS[2] entri, KEYS[3] room | ARGV[1] event, ARGV[2] user, ARGV[3] queue_id, ARGV[4] ttl
local pos = redis.call('INCR', KEYS[1])
redis.call('HSET', KEYS[2], 'event_id', ARGV[1], 'user_id', ARGV[2], 'status', 'waiting', 'pos', pos)
redis.call('EXPIRE', KEYS[2], ARGV[4])
redis.call('RPUSH', KEYS[3], ARGV[1] .. '|' .. ARGV[3])
return pos
`)

/*
Kalau user ternyata sudah punya tiket, Enqueue mengembalikan tiket itu lewat
replay dan tidak meng-enqueue-nya.
*/
func (q *Queue) Enqueue(ctx context.Context, eventID int64, userID string) (e Entry, replay *tiket.Ticket, err error) {
	left, ok, err := q.stock.Stock(ctx, eventID)
	if err != nil {
		return e, nil, err
	}
	if !ok {
		return e, nil, tiket.ErrEventNotFound
	}
	if id, has, err := q.stock.ExistingTicket(ctx, eventID, userID); err != nil {
		return e, nil, err
	} else if has {
		return e, &tiket.Ticket{ID: id, EventID: eventID, UserID: userID, Strategy: tiket.Queue}, nil
	}
	if left <= 0 {
		return e, nil, tiket.ErrSoldOut // waiting room ditutup
	}

	id := tiket.NewID()
	pos, err := enqueueScript.Run(ctx, q.rdb,
		[]string{seqKey(eventID), entryKey(id), roomKey},
		eventID, userID, id, int(entryTTL.Seconds()),
	).Int64()
	if err != nil {
		return e, nil, err
	}
	admitted, _ := q.rdb.Get(ctx, admittedKey(eventID)).Int64()
	ahead := max(0, pos-admitted-1)
	return Entry{QueueID: id, Status: Waiting, Position: &ahead, PollURL: "/v1/queue/" + id}, nil, nil
}

func (q *Queue) Status(ctx context.Context, id string) (Entry, error) {
	m, err := q.rdb.HGetAll(ctx, entryKey(id)).Result()
	if err != nil {
		return Entry{}, err
	}
	if len(m) == 0 {
		return Entry{}, tiket.ErrQueueNotFound
	}
	e := Entry{QueueID: id, Status: Status(m["status"]), TicketID: m["ticket_id"], PollURL: "/v1/queue/" + id}
	if e.Status == Waiting {
		ev, _ := strconv.ParseInt(m["event_id"], 10, 64)
		pos, _ := strconv.ParseInt(m["pos"], 10, 64)
		admitted, _ := q.rdb.Get(ctx, admittedKey(ev)).Int64()
		ahead := max(0, pos-admitted-1)
		e.Position = &ahead
	}
	return e, nil
}

// Waiting adalah queue length saat ini (semua event).
func (q *Queue) Waiting(ctx context.Context) (int64, error) { return q.rdb.LLen(ctx, roomKey).Result() }

func (q *Queue) Reset(ctx context.Context, eventID int64) error {
	return q.rdb.Del(ctx, roomKey, seqKey(eventID), admittedKey(eventID)).Err()
}

// Run menyalakan worker admitter dan menunggu sampai ctx dibatalkan.
func (q *Queue) Run(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		go q.admitLoop(ctx)
	}
	<-ctx.Done()
}

func (q *Queue) admitLoop(ctx context.Context) {
	for ctx.Err() == nil {
		res, err := q.rdb.BLPop(ctx, time.Second, roomKey).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				q.log.Warn("queue: BLPOP gagal", "err", err)
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		evStr, id, ok := strings.Cut(res[1], "|")
		ev, perr := strconv.ParseInt(evStr, 10, 64)
		if !ok || perr != nil {
			q.log.Error("queue: entri rusak", "value", res[1])
			continue
		}
		q.admit(ctx, ev, id)
	}
}

func (q *Queue) admit(ctx context.Context, eventID int64, id string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	q.obs.QueueAdmitted()

	key := entryKey(id)
	pipe := q.rdb.TxPipeline()
	pipe.Incr(ctx, admittedKey(eventID))
	pipe.HSet(ctx, key, "status", string(Admitted))
	user := pipe.HGet(ctx, key, "user_id")
	if _, err := pipe.Exec(ctx); err != nil {
		q.log.Warn("queue: gagal menandai admitted", "id", id, "err", err)
		q.rdb.LPush(ctx, roomKey, fmt.Sprintf("%d|%s", eventID, id))
		q.obs.QueueRequeued()
		return
	}

	res, err := q.stock.Reserve(ctx, eventID, user.Val(), tiket.Queue)
	switch {
	case errors.Is(err, tiket.ErrSoldOut), errors.Is(err, tiket.ErrEventNotFound):
		q.rdb.HSet(ctx, key, "status", string(SoldOut))
		q.obs.QueueDone(false)
	case err != nil:
		// Requeue ke depan supaya tidak hilang.
		q.log.Warn("queue: reserve gagal, di-requeue", "id", id, "err", err)
		q.rdb.HSet(ctx, key, "status", string(Waiting))
		q.rdb.LPush(ctx, roomKey, fmt.Sprintf("%d|%s", eventID, id))
		q.obs.QueueRequeued()
	default:
		q.rdb.HSet(ctx, key, "status", string(Confirmed), "ticket_id", res.Ticket.ID)
		q.obs.QueueDone(!res.Replayed)
	}
}
