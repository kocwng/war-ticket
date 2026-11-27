// Package stats menghitung hasil setiap request beli dan merekam timeline
// tiap 10 ms. Timeline ini yang nanti diputar ulang di halaman demo.
//
// Semua penghitung ada di memori proses, jadi angkanya hanya mewakili satu
// instance. Itu cukup, karena benchmark memang menjalankan satu instance
// per implementasi.
package stats

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Outcome string

const (
	Created    Outcome = "created"
	Replayed   Outcome = "replayed"
	SoldOut    Outcome = "sold_out"
	Timeout    Outcome = "timeout"
	Overloaded Outcome = "overloaded"
	Queued     Outcome = "queued"
	BadRequest Outcome = "bad_request"
	NotFound   Outcome = "not_found"
	Error      Outcome = "error"
)

var allOutcomes = []Outcome{Created, Replayed, SoldOut, Timeout, Overloaded, Queued, BadRequest, NotFound, Error}

var (
	outcomesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wartiket_purchase_outcomes_total",
		Help: "Hasil akhir request beli per strategi.",
	}, []string{"strategy", "outcome"})
	inflightGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "wartiket_purchase_inflight",
		Help: "Request beli yang sedang diproses.",
	})
	waitingGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "wartiket_queue_waiting",
		Help: "Pembeli di ruang tunggu (instance ini).",
	})
)

const (
	SampleInterval = 10 * time.Millisecond
	maxPoints      = 12000 // 2 menit
	idleStop       = 2 * time.Second
)

var Columns = []string{"t_ms", "sold", "inflight", "waiting"}

type Recorder struct {
	counts   map[Outcome]*atomic.Int64
	sold     atomic.Int64
	inflight atomic.Int64
	enqueued atomic.Int64
	admitted atomic.Int64
	strategy atomic.Value // string

	mu       sync.Mutex
	points   [][4]int64
	started  time.Time // nol = belum ada aktivitas sejak reset
	lastBusy time.Time
	stopped  bool
}

func NewRecorder() *Recorder {
	r := &Recorder{counts: make(map[Outcome]*atomic.Int64, len(allOutcomes))}
	for _, o := range allOutcomes {
		r.counts[o] = new(atomic.Int64)
	}
	r.strategy.Store("")
	return r
}

// Reset mengosongkan semua penghitung dan menyiapkan rekaman timeline baru.
func (r *Recorder) Reset(strategy string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.counts {
		c.Store(0)
	}
	r.sold.Store(0)
	r.enqueued.Store(0)
	r.admitted.Store(0)
	r.strategy.Store(strategy)
	r.points = r.points[:0]
	r.started = time.Time{}
	r.stopped = false
}

func (r *Recorder) Add(o Outcome) {
	if c, ok := r.counts[o]; ok {
		c.Add(1)
	}
	if o == Created {
		r.sold.Add(1)
	}
	if o == Queued {
		r.enqueued.Add(1)
		waitingGauge.Inc()
	}
	outcomesTotal.WithLabelValues(r.strategy.Load().(string), string(o)).Inc()
}

func (r *Recorder) Begin() { r.inflight.Add(1); inflightGauge.Inc() }
func (r *Recorder) End()   { r.inflight.Add(-1); inflightGauge.Dec() }

// Tiga method di bawah ini memenuhi interface queue.Observer.
func (r *Recorder) QueueAdmitted() { r.admitted.Add(1); waitingGauge.Dec() }
func (r *Recorder) QueueRequeued() { r.admitted.Add(-1); waitingGauge.Inc() }
func (r *Recorder) QueueDone(created bool) {
	if created {
		r.Add(Created)
	} else {
		r.Add(SoldOut)
	}
}

func (r *Recorder) Sold() int64 { return r.sold.Load() }

func (r *Recorder) Outcomes() map[string]int64 {
	m := make(map[string]int64, len(r.counts))
	for o, c := range r.counts {
		m[string(o)] = c.Load()
	}
	return m
}

func (r *Recorder) waiting() int64 { return max(0, r.enqueued.Load()-r.admitted.Load()) }

// Run mengambil sampel tiap 10 ms. Rekaman baru mulai saat ada aktivitas
// pertama setelah reset, dan berhenti sendiri kalau 2 detik tidak ada apa-apa.
func (r *Recorder) Run(ctx context.Context) {
	tk := time.NewTicker(SampleInterval)
	defer tk.Stop()
	var lastSold int64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			r.sample(now, &lastSold)
		}
	}
}

func (r *Recorder) sample(now time.Time, lastSold *int64) {
	sold, inflight, waiting := r.sold.Load(), r.inflight.Load(), r.waiting()
	busy := inflight > 0 || waiting > 0 || sold != *lastSold
	*lastSold = sold

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if r.started.IsZero() {
		if !busy {
			return
		}
		r.started = now
	}
	if busy {
		r.lastBusy = now
	} else if now.Sub(r.lastBusy) > idleStop {
		r.stopped = true
		return
	}
	if len(r.points) >= maxPoints {
		r.stopped = true
		return
	}
	r.points = append(r.points, [4]int64{now.Sub(r.started).Milliseconds(), sold, inflight, waiting})
}

type Timeline struct {
	IntervalMs int64      `json:"interval_ms"`
	Columns    []string   `json:"columns"`
	Points     [][4]int64 `json:"points"`
}

func (r *Recorder) Timeline() Timeline {
	r.mu.Lock()
	defer r.mu.Unlock()
	pts := make([][4]int64, len(r.points))
	copy(pts, r.points)
	return Timeline{IntervalMs: SampleInterval.Milliseconds(), Columns: Columns, Points: pts}
}
