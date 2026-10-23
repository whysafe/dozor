package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/whysafe/dozor/internal/probe"
)

var moscow = time.FixedZone("MSK", 3*60*60)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), moscow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func result(id string, at time.Time, status probe.Status, latency int) probe.Result {
	return probe.Result{MonitorID: id, At: at, Status: status, Latency: time.Duration(latency) * time.Millisecond}
}

func TestDailyRollupUsesLocalCalendarDay(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// 22:30 UTC on Oct 1 is already Oct 2 in Moscow.
	late := time.Date(2026, 10, 1, 22, 30, 0, 0, time.UTC)
	for _, r := range []probe.Result{
		result("site", late, probe.Up, 100),
		result("site", late.Add(time.Minute), probe.Down, 300),
		result("site", late.Add(2*time.Minute), probe.Degraded, 200),
		result("site", late.Add(-2*time.Hour), probe.Up, 50),
	} {
		if err := s.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	days, err := s.Days(ctx, "site", late, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 3 || days[2].Day != "2026-10-02" || days[1].Day != "2026-10-01" || days[0].Total != 0 {
		t.Fatalf("unexpected days: %+v", days)
	}
	today := days[2]
	if today.Total != 3 || today.Available != 2 || today.Degraded != 1 || today.AvgLatency != 200*time.Millisecond {
		t.Errorf("rollup wrong: %+v", today)
	}
	if days[1].Total != 1 {
		t.Errorf("previous day: %+v", days[1])
	}
	ratio, ok := Uptime(days)
	if !ok || ratio != 0.75 {
		t.Errorf("uptime = %v, %v", ratio, ok)
	}
}

func TestLatestAndSince(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		if err := s.Record(ctx, result("api", base.Add(time.Duration(i)*time.Minute), probe.Up, 10*(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	latest, ok, err := s.Latest(ctx, "api")
	if err != nil || !ok || latest.Latency != 50*time.Millisecond || !latest.At.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("latest = %+v, %v, %v", latest, ok, err)
	}
	if _, ok, _ := s.Latest(ctx, "missing"); ok {
		t.Error("latest for unknown monitor")
	}
	since, err := s.Since(ctx, "api", base.Add(3*time.Minute))
	if err != nil || len(since) != 2 || since[0].Latency != 40*time.Millisecond {
		t.Fatalf("since = %+v, %v", since, err)
	}
}

func TestIncidentLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	inc, err := s.OpenIncident(ctx, "db", start, "соединение отклонено")
	if err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenIncidentFor(ctx, "db")
	if err != nil || open.ID != inc.ID || !open.Open() {
		t.Fatalf("open incident = %+v, %v", open, err)
	}
	if err := s.ResolveIncident(ctx, inc.ID, start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveIncident(ctx, inc.ID, start.Add(time.Hour)); err == nil {
		t.Error("resolving twice must fail")
	}
	if _, err := s.OpenIncidentFor(ctx, "db"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	list, err := s.Incidents(ctx, start.Add(-time.Hour), 10)
	if err != nil || len(list) != 1 || list[0].Duration(time.Now()) != 10*time.Minute {
		t.Fatalf("incidents = %+v, %v", list, err)
	}
}

func TestPruneKeepsRollups(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	old := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.Record(ctx, result("site", old, probe.Up, 10))
	s.Record(ctx, result("site", recent, probe.Up, 10))

	n, err := s.Prune(ctx, recent.Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	days, _ := s.Days(ctx, "site", recent, 90)
	if ratio, ok := Uptime(days); !ok || ratio != 1 || days[28].Total != 1 || days[28].Day != "2026-08-01" {
		t.Errorf("rollup lost after prune: %v %v", ratio, ok)
	}
}

func TestReopenKeepsSchemaAndData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "re.db")
	s, err := Open(path, moscow)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(context.Background(), result("a", time.Now(), probe.Up, 1))
	s.Close()

	s, err = Open(path, moscow)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, _ := s.Latest(context.Background(), "a"); !ok {
		t.Error("data lost on reopen")
	}
}
