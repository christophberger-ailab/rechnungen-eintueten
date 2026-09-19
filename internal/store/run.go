package store

import (
	"fmt"
	"time"
)

// Run records one execution of the pipeline, whether started by the scheduler,
// the CLI or the dashboard button.
type Run struct {
	ID         int64
	Trigger    string
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string // running, ok, error
	Found      int
	Processed  int
	Failed     int
	Summary    string
}

// Event is a single log line tied to a run and optionally to an invoice.
type Event struct {
	ID        int64
	RunID     int64
	InvoiceID int64
	TS        time.Time
	Module    string
	Level     string
	Message   string
}

// StartRun opens a new run record.
func (db *DB) StartRun(trigger string) (*Run, error) {
	res, err := db.Exec(`INSERT INTO runs (trigger) VALUES (?)`, trigger)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Run{ID: id, Trigger: trigger, StartedAt: time.Now(), Status: "running"}, nil
}

// FinishRun closes a run record with its counters and status.
func (db *DB) FinishRun(r *Run) error {
	_, err := db.Exec(`UPDATE runs SET finished_at=datetime('now'), status=?, found=?, processed=?,
		failed=?, summary=? WHERE id=?`, r.Status, r.Found, r.Processed, r.Failed, r.Summary, r.ID)
	return err
}

// LastRun returns the most recent run, or nil when the pipeline never ran.
func (db *DB) LastRun() (*Run, error) {
	runs, err := db.Runs(1)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return runs[0], nil
}

// Runs returns the newest runs first.
func (db *DB) Runs(limit int) ([]*Run, error) {
	rows, err := db.Query(`SELECT id, trigger, started_at, finished_at, status, found, processed,
		failed, summary FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		var r Run
		var started string
		var finished *string
		if err := rows.Scan(&r.ID, &r.Trigger, &started, &finished, &r.Status, &r.Found,
			&r.Processed, &r.Failed, &r.Summary); err != nil {
			return nil, err
		}
		r.StartedAt = parseTime(started)
		if finished != nil {
			r.FinishedAt = parseTime(*finished)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// Log appends an event. Logging must never break the pipeline, so errors are
// swallowed here; the caller already has the message.
func (db *DB) Log(runID, invoiceID int64, module, level, format string, args ...any) {
	db.Exec(`INSERT INTO events (run_id, invoice_id, module, level, message) VALUES (?, ?, ?, ?, ?)`,
		nullID(runID), nullID(invoiceID), module, level, fmt.Sprintf(format, args...))
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// Events returns the newest events, optionally restricted to one run.
func (db *DB) Events(runID int64, limit int) ([]*Event, error) {
	q := `SELECT id, coalesce(run_id,0), coalesce(invoice_id,0), ts, module, level, message
		FROM events %s ORDER BY id DESC LIMIT ?`
	args := []any{limit}
	where := ""
	if runID != 0 {
		where = "WHERE run_id=?"
		args = []any{runID, limit}
	}
	rows, err := db.Query(fmt.Sprintf(q, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var ts string
		if err := rows.Scan(&e.ID, &e.RunID, &e.InvoiceID, &ts, &e.Module, &e.Level, &e.Message); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		out = append(out, &e)
	}
	return out, rows.Err()
}
