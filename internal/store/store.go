// Package store keeps check results, daily rollups and incidents in SQLite.
//
// Raw results are pruned after the retention period, while the daily rollup
// is kept forever, so long-range uptime stays cheap to compute and survives pruning.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whysafe/dozor/internal/probe"
)

var migrations = []string{
	`CREATE TABLE results (
		monitor_id TEXT    NOT NULL,
		at         INTEGER NOT NULL,
		status     TEXT    NOT NULL,
		latency_ms INTEGER NOT NULL,
		message    TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX results_monitor_at ON results (monitor_id, at);

	CREATE TABLE daily (
		monitor_id  TEXT    NOT NULL,
		day         TEXT    NOT NULL,
		total       INTEGER NOT NULL,
		available   INTEGER NOT NULL,
		degraded    INTEGER NOT NULL,
		latency_sum INTEGER NOT NULL,
		PRIMARY KEY (monitor_id, day)
	);

	CREATE TABLE incidents (
		id          INTEGER PRIMARY KEY,
		monitor_id  TEXT    NOT NULL,
		started_at  INTEGER NOT NULL,
		resolved_at INTEGER,
		cause       TEXT    NOT NULL
	);
	CREATE INDEX incidents_monitor ON incidents (monitor_id, started_at);`,

	`ALTER TABLE results ADD COLUMN cert_expiry INTEGER NOT NULL DEFAULT 0;`,
}

type Store struct {
	db  *sql.DB
	loc *time.Location
}

type DayStat struct {
	Day        string
	Total      int
	Available  int
	Degraded   int
	AvgLatency time.Duration
}

func (d DayStat) Uptime() float64 {
	if d.Total == 0 {
		return 1
	}
	return float64(d.Available) / float64(d.Total)
}

type Incident struct {
	ID         int64
	MonitorID  string
	StartedAt  time.Time
	ResolvedAt time.Time
	Cause      string
}

func (i Incident) Open() bool { return i.ResolvedAt.IsZero() }

func (i Incident) Duration(now time.Time) time.Duration {
	if i.Open() {
		return now.Sub(i.StartedAt)
	}
	return i.ResolvedAt.Sub(i.StartedAt)
}

// Open opens (or creates) the database at path. Use ":memory:" for a throwaway store.
// loc decides where calendar days begin for the daily rollup.
func Open(path string, loc *time.Location) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes and keeps ":memory:" databases
	// from being split across pool connections.
	db.SetMaxOpenConns(1)
	if loc == nil {
		loc = time.Local
	}
	s := &Store{db: db, loc: loc}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("база создана более новой версией dozor (схема %d, поддерживается %d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// DayKey returns the rollup key of the calendar day containing t.
func (s *Store) DayKey(t time.Time) string { return t.In(s.loc).Format(time.DateOnly) }

func (s *Store) Location() *time.Location { return s.loc }

func (s *Store) Record(ctx context.Context, r probe.Result) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var certExpiry int64
	if !r.CertExpiry.IsZero() {
		certExpiry = r.CertExpiry.Unix()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO results (monitor_id, at, status, latency_ms, message, cert_expiry) VALUES (?, ?, ?, ?, ?, ?)`,
		r.MonitorID, r.At.UnixMilli(), string(r.Status), r.Latency.Milliseconds(), r.Message, certExpiry); err != nil {
		return err
	}

	available, degraded := boolInt(r.Status.Available()), boolInt(r.Status == probe.Degraded)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO daily (monitor_id, day, total, available, degraded, latency_sum) VALUES (?, ?, 1, ?, ?, ?)
		 ON CONFLICT (monitor_id, day) DO UPDATE SET
		   total = total + 1,
		   available = available + excluded.available,
		   degraded = degraded + excluded.degraded,
		   latency_sum = latency_sum + excluded.latency_sum`,
		r.MonitorID, s.DayKey(r.At), available, degraded, r.Latency.Milliseconds()); err != nil {
		return err
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) Latest(ctx context.Context, monitorID string) (probe.Result, bool, error) {
	rows, err := s.results(ctx,
		`SELECT monitor_id, at, status, latency_ms, message, cert_expiry FROM results
		 WHERE monitor_id = ? ORDER BY at DESC LIMIT 1`, monitorID)
	if err != nil || len(rows) == 0 {
		return probe.Result{}, false, err
	}
	return rows[0], true, nil
}

// Since returns results of a monitor from t onwards, oldest first.
func (s *Store) Since(ctx context.Context, monitorID string, t time.Time) ([]probe.Result, error) {
	return s.results(ctx,
		`SELECT monitor_id, at, status, latency_ms, message, cert_expiry FROM results
		 WHERE monitor_id = ? AND at >= ? ORDER BY at`, monitorID, t.UnixMilli())
}

func (s *Store) results(ctx context.Context, query string, args ...any) ([]probe.Result, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []probe.Result
	for rows.Next() {
		var (
			r                    probe.Result
			at, latency, expires int64
			status               string
		)
		if err := rows.Scan(&r.MonitorID, &at, &status, &latency, &r.Message, &expires); err != nil {
			return nil, err
		}
		r.At = time.UnixMilli(at)
		r.Status = probe.Status(status)
		r.Latency = time.Duration(latency) * time.Millisecond
		if expires != 0 {
			r.CertExpiry = time.Unix(expires, 0)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Days returns the rollup for the last n calendar days ending with the day of
// now, oldest first. Days without checks are returned with Total == 0.
func (s *Store) Days(ctx context.Context, monitorID string, now time.Time, n int) ([]DayStat, error) {
	local := now.In(s.loc)
	first := time.Date(local.Year(), local.Month(), local.Day()-(n-1), 0, 0, 0, 0, s.loc)

	rows, err := s.db.QueryContext(ctx,
		`SELECT day, total, available, degraded, latency_sum FROM daily
		 WHERE monitor_id = ? AND day >= ? AND day <= ?`,
		monitorID, first.Format(time.DateOnly), local.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := map[string]DayStat{}
	for rows.Next() {
		var (
			d          DayStat
			latencySum int64
		)
		if err := rows.Scan(&d.Day, &d.Total, &d.Available, &d.Degraded, &latencySum); err != nil {
			return nil, err
		}
		if d.Total > 0 {
			d.AvgLatency = time.Duration(latencySum/int64(d.Total)) * time.Millisecond
		}
		byDay[d.Day] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]DayStat, n)
	for i := range n {
		key := first.AddDate(0, 0, i).Format(time.DateOnly)
		d, ok := byDay[key]
		if !ok {
			d = DayStat{Day: key}
		}
		out[i] = d
	}
	return out, nil
}

// Uptime aggregates daily rollups into a single availability ratio.
// ok is false when there were no checks at all.
func Uptime(days []DayStat) (ratio float64, ok bool) {
	var total, available int
	for _, d := range days {
		total += d.Total
		available += d.Available
	}
	if total == 0 {
		return 0, false
	}
	return float64(available) / float64(total), true
}

func (s *Store) OpenIncident(ctx context.Context, monitorID string, startedAt time.Time, cause string) (Incident, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incidents (monitor_id, started_at, cause) VALUES (?, ?, ?)`,
		monitorID, startedAt.UnixMilli(), cause)
	if err != nil {
		return Incident{}, err
	}
	id, err := res.LastInsertId()
	return Incident{ID: id, MonitorID: monitorID, StartedAt: startedAt, Cause: cause}, err
}

func (s *Store) ResolveIncident(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL`, at.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("инцидент %d не найден или уже закрыт", id)
	}
	return nil
}

var ErrNotFound = errors.New("not found")

// OpenIncidentFor returns the unresolved incident of a monitor, if any.
func (s *Store) OpenIncidentFor(ctx context.Context, monitorID string) (Incident, error) {
	list, err := s.incidents(ctx,
		`SELECT id, monitor_id, started_at, resolved_at, cause FROM incidents
		 WHERE monitor_id = ? AND resolved_at IS NULL ORDER BY started_at DESC LIMIT 1`, monitorID)
	if err != nil {
		return Incident{}, err
	}
	if len(list) == 0 {
		return Incident{}, ErrNotFound
	}
	return list[0], nil
}

// Incidents returns incidents that started after since or are still open, newest first.
func (s *Store) Incidents(ctx context.Context, since time.Time, limit int) ([]Incident, error) {
	return s.incidents(ctx,
		`SELECT id, monitor_id, started_at, resolved_at, cause FROM incidents
		 WHERE started_at >= ? OR resolved_at IS NULL ORDER BY started_at DESC LIMIT ?`,
		since.UnixMilli(), limit)
}

func (s *Store) incidents(ctx context.Context, query string, args ...any) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Incident
	for rows.Next() {
		var (
			inc      Incident
			started  int64
			resolved sql.NullInt64
		)
		if err := rows.Scan(&inc.ID, &inc.MonitorID, &started, &resolved, &inc.Cause); err != nil {
			return nil, err
		}
		inc.StartedAt = time.UnixMilli(started)
		if resolved.Valid {
			inc.ResolvedAt = time.UnixMilli(resolved.Int64)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// Prune deletes raw results older than before; rollups and incidents are kept.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM results WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
