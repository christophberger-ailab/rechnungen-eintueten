package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/store"
)

// Runner makes sure only one pipeline run is in flight at a time, no matter
// whether it was started by the scheduler, the CLI or the dashboard button.
type Runner struct {
	db *store.DB

	mu      sync.Mutex
	running bool
}

// NewRunner returns a runner working on db.
func NewRunner(db *store.DB) *Runner { return &Runner{db: db} }

// Running reports whether a run is currently in progress.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// acquire claims the single run slot, reporting whether it was free.
func (r *Runner) acquire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *Runner) release() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

// ErrBusy is returned when a run is already in progress.
var ErrBusy = errors.New("es läuft bereits eine Verarbeitung")

// RunOnce executes the pipeline and waits for it to finish.
func (r *Runner) RunOnce(ctx context.Context, trigger string) (*store.Run, error) {
	if !r.acquire() {
		return nil, ErrBusy
	}
	defer r.release()

	p, err := New(r.db)
	if err != nil {
		return nil, err
	}
	return p.Run(ctx, trigger)
}

// Start kicks off a run in the background and reports whether it started.
func (r *Runner) Start(ctx context.Context, trigger string) bool {
	if r.Running() {
		return false
	}
	go func() {
		if _, err := r.RunOnce(ctx, trigger); err != nil && !errors.Is(err, ErrBusy) {
			r.db.Log(0, 0, "run", "error", "%v", err)
		}
	}()
	return true
}

// Schedule runs the pipeline once a day at the given wall clock time
// ("HH:MM") until ctx is cancelled.
func (r *Runner) Schedule(ctx context.Context, dailyAt string) {
	for {
		wait := until(time.Now(), dailyAt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if _, err := r.RunOnce(ctx, "scheduler"); err != nil && !errors.Is(err, ErrBusy) {
				r.db.Log(0, 0, "run", "error", "%v", err)
			}
		}
	}
}

// until returns how long to wait from now until the next occurrence of the
// given "HH:MM" wall clock time.
func until(now time.Time, hhmm string) time.Duration {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		t, _ = time.Parse("15:04", "03:00")
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}
