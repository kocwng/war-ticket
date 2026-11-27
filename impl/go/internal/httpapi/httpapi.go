// Package httpapi berisi handler HTTP sesuai kontrak di api/openapi.yaml.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"wartiket/internal/config"
	"wartiket/internal/queue"
	"wartiket/internal/redisstock"
	"wartiket/internal/stats"
	"wartiket/internal/store"
	"wartiket/internal/tiket"
)

var requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "wartiket_http_request_duration_seconds",
	Help:    "Durasi request HTTP per route dan status.",
	Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5},
}, []string{"route", "code"})

type Server struct {
	cfg      config.Config
	db       *store.Store
	rs       *redisstock.Store
	q        *queue.Queue
	rec      *stats.Recorder
	log      *slog.Logger
	sem      chan struct{} // nil = tanpa load shedding
	strategy atomic.Value  // tiket.Strategy
}

func New(cfg config.Config, db *store.Store, rs *redisstock.Store, q *queue.Queue, rec *stats.Recorder, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, db: db, rs: rs, q: q, rec: rec, log: log}
	if cfg.MaxInflight > 0 {
		s.sem = make(chan struct{}, cfg.MaxInflight)
	}
	s.strategy.Store(cfg.DefaultStrategy)
	rec.Reset(string(cfg.DefaultStrategy))
	return s
}

func (s *Server) current() tiket.Strategy { return s.strategy.Load().(tiket.Strategy) }

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /v1/events/{eventId}", s.getEvent)
	mux.HandleFunc("POST /v1/events/{eventId}/purchase", s.purchase)
	mux.HandleFunc("GET /v1/queue/{queueId}", s.queueStatus)
	mux.HandleFunc("POST /admin/reset", s.admin(s.reset))
	mux.HandleFunc("GET /admin/report", s.admin(s.report))
	return instrument(mux)
}

// ---------- Publik ----------

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := eventID(w, r)
	if !ok {
		return
	}
	e, err := s.db.GetEvent(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	e.Strategy = s.current()
	if e.Strategy == tiket.Redis || e.Strategy == tiket.Queue {
		if n, ok, err := s.rs.Stock(r.Context(), id); err != nil {
			s.fail(w, err)
			return
		} else if ok {
			e.Stock = n // untuk strategi ini, stok yang dipegang adalah yang di Redis
		}
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) purchase(w http.ResponseWriter, r *http.Request) {
	// Kalau server sudah penuh, tolak saja dengan 503. Lebih baik sebagian
	// pembeli ditolak cepat daripada semuanya melambat bareng.
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			s.rec.Add(stats.Overloaded)
			writeError(w, http.StatusServiceUnavailable, "overloaded", "server lagi penuh, coba beberapa saat lagi")
			return
		}
	}
	s.rec.Begin()
	defer s.rec.End()

	id, ok := eventID(w, r)
	if !ok {
		s.rec.Add(stats.BadRequest)
		return
	}
	var body struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil ||
		body.UserID == "" || len(body.UserID) > 64 {
		s.rec.Add(stats.BadRequest)
		writeError(w, http.StatusBadRequest, "bad_request", "user_id wajib diisi, 1 sampai 64 karakter")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	st := s.current()
	var (
		res tiket.PurchaseResult
		err error
	)
	switch st {
	case tiket.Naive:
		res, err = s.db.PurchaseNaive(ctx, id, body.UserID)
	case tiket.ForUpdate:
		res, err = s.db.PurchaseForUpdate(ctx, id, body.UserID)
	case tiket.Redis:
		res, err = s.rs.Reserve(ctx, id, body.UserID, tiket.Redis)
	case tiket.Queue:
		entry, replay, qerr := s.q.Enqueue(ctx, id, body.UserID)
		if qerr == nil && replay == nil {
			s.rec.Add(stats.Queued)
			writeJSON(w, http.StatusAccepted, entry)
			return
		}
		if replay != nil {
			res = tiket.PurchaseResult{Ticket: *replay, Replayed: true}
		}
		err = qerr
	}

	switch {
	case err == nil && res.Replayed:
		s.rec.Add(stats.Replayed)
		writeJSON(w, http.StatusOK, res.Ticket)
	case err == nil:
		s.rec.Add(stats.Created)
		writeJSON(w, http.StatusCreated, res.Ticket)
	case errors.Is(err, tiket.ErrSoldOut):
		s.rec.Add(stats.SoldOut)
		writeError(w, http.StatusConflict, "sold_out", "tiket habis")
	case errors.Is(err, tiket.ErrEventNotFound):
		s.rec.Add(stats.NotFound)
		writeError(w, http.StatusNotFound, "not_found", "event tidak ditemukan")
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		s.rec.Add(stats.Timeout)
		writeError(w, http.StatusGatewayTimeout, "timeout", "request melewati batas waktu "+s.cfg.RequestTimeout.String())
	default:
		s.rec.Add(stats.Error)
		s.log.Error("purchase gagal", "strategy", st, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "terjadi kesalahan di server")
	}
}

func (s *Server) queueStatus(w http.ResponseWriter, r *http.Request) {
	e, err := s.q.Status(r.Context(), r.PathValue("queueId"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ---------- Admin ----------

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken != "" && r.Header.Get("X-Admin-Token") != s.cfg.AdminToken {
			writeError(w, http.StatusUnauthorized, "unauthorized", "header X-Admin-Token salah atau tidak ada")
			return
		}
		h(w, r)
	}
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventID  int64  `json:"event_id"`
		Stock    *int   `json:"stock"`
		Strategy string `json:"strategy"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil ||
		body.EventID <= 0 || body.Stock == nil || *body.Stock < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "isi event_id (> 0), stock (>= 0), dan strategy")
		return
	}
	st, err := tiket.ParseStrategy(body.Strategy)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx := r.Context()
	if err := s.db.Reset(ctx, body.EventID, *body.Stock); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.rs.Reset(ctx, body.EventID, *body.Stock); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.q.Reset(ctx, body.EventID); err != nil {
		s.fail(w, err)
		return
	}
	s.strategy.Store(st)
	s.rec.Reset(string(st))
	s.log.Info("reset", "event_id", body.EventID, "stock", *body.Stock, "strategy", st)
	w.WriteHeader(http.StatusNoContent)
}

type report struct {
	EventID        int64          `json:"event_id"`
	Strategy       tiket.Strategy `json:"strategy"`
	Implementation string         `json:"implementation"`
	Capacity       int            `json:"capacity"`
	Oversold       int64          `json:"oversold"`
	Consistent     bool           `json:"consistent"`
	DB             reportDB       `json:"db"`
	Redis          reportRedis    `json:"redis"`
	App            reportApp      `json:"app"`
	Timeline       stats.Timeline `json:"timeline"`
}
type reportDB struct {
	Stock   int `json:"stock"`
	Tickets int `json:"tickets"`
}
type reportRedis struct {
	Stock      *int  `json:"stock"`
	PersistLag int64 `json:"persist_lag"`
}
type reportApp struct {
	Sold     int64            `json:"sold"`
	Outcomes map[string]int64 `json:"outcomes"`
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("event_id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "tambahkan query event_id")
		return
	}
	ctx := r.Context()
	c, err := s.db.Counts(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	lag, err := s.rs.PersistLag(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	var redisStock *int
	if n, ok, err := s.rs.Stock(ctx, id); err != nil {
		s.fail(w, err)
		return
	} else if ok {
		redisStock = &n
	}

	st := s.current()
	sold := s.rec.Sold()
	issued := max(int64(c.Tickets), sold)
	consistent := lag == 0 &&
		c.Tickets <= c.Capacity &&
		c.Stock+c.Tickets == c.Capacity
	if (st == tiket.Redis || st == tiket.Queue) && redisStock != nil {
		consistent = consistent && *redisStock == c.Stock
	}

	writeJSON(w, http.StatusOK, report{
		EventID:        id,
		Strategy:       st,
		Implementation: "go",
		Capacity:       c.Capacity,
		Oversold:       max(0, issued-int64(c.Capacity)),
		Consistent:     consistent,
		DB:             reportDB{Stock: c.Stock, Tickets: c.Tickets},
		Redis:          reportRedis{Stock: redisStock, PersistLag: lag},
		App:            reportApp{Sold: sold, Outcomes: s.rec.Outcomes()},
		Timeline:       s.rec.Timeline(),
	})
}

// ---------- Helper ----------

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tiket.ErrEventNotFound):
		writeError(w, http.StatusNotFound, "not_found", "event tidak ditemukan")
	case errors.Is(err, tiket.ErrQueueNotFound):
		writeError(w, http.StatusNotFound, "not_found", "antrean tidak ditemukan atau sudah kedaluwarsa")
	default:
		s.log.Error("request gagal", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "terjadi kesalahan di server")
	}
}

func eventID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("eventId"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "eventId harus angka bulat positif")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]string{"error": kind, "message": msg})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (sw *statusWriter) WriteHeader(code int) { sw.code = code; sw.ResponseWriter.WriteHeader(code) }

func instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := r.Pattern // diisi ServeMux (Go 1.23+)
		if route == "" {
			route = "unmatched"
		}
		if strings.HasPrefix(route, "GET /metrics") {
			return
		}
		requestDuration.WithLabelValues(route, strconv.Itoa(sw.code)).Observe(time.Since(start).Seconds())
	})
}
