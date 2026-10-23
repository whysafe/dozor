package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/probe"
	"github.com/whysafe/dozor/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (r *recorder) Notify(_ context.Context, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}

func setup(t *testing.T) (*config.Config, *store.Store, *recorder) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
failure_threshold: 3
recovery_threshold: 2
monitors:
  - {id: site, type: tcp, address: "localhost:1"}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return cfg, st, &recorder{}
}

func feed(t *testing.T, e *Engine, m config.Monitor, start time.Time, statuses ...probe.Status) {
	t.Helper()
	for i, s := range statuses {
		r := probe.Result{MonitorID: m.ID, At: start.Add(time.Duration(i) * time.Minute), Status: s}
		if s == probe.Down {
			r.Message = "соединение отклонено"
		}
		if err := e.Observe(context.Background(), m, r); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	U = probe.Up
	D = probe.Down
	W = probe.Degraded
)

func TestIncidentOpensAfterThresholdAndStartsAtFirstFailure(t *testing.T) {
	cfg, st, rec := setup(t)
	e := New(cfg, st, nil, rec, nil)
	m := cfg.Monitors[0]
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	feed(t, e, m, start, U, D, D, U, D, D, D, D)
	if len(rec.events) != 1 || rec.events[0].Kind != Opened {
		t.Fatalf("events = %+v", rec.events)
	}
	inc := rec.events[0].Incident
	if want := start.Add(4 * time.Minute); !inc.StartedAt.Equal(want) {
		t.Errorf("incident starts at %v, want first failure of the streak %v", inc.StartedAt, want)
	}
	if !e.Down(m.ID) {
		t.Error("engine does not report the monitor as down")
	}
}

func TestIncidentResolvesAfterRecoveryStreak(t *testing.T) {
	cfg, st, rec := setup(t)
	e := New(cfg, st, nil, rec, nil)
	m := cfg.Monitors[0]
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// A degraded answer counts as a recovery; one success between failures does not.
	feed(t, e, m, start, D, D, D, U, D, W, U)
	if len(rec.events) != 2 || rec.events[1].Kind != Resolved {
		t.Fatalf("events = %+v", rec.events)
	}
	resolved := rec.events[1].Incident
	if resolved.Duration(time.Time{}) != 6*time.Minute {
		t.Errorf("duration = %v", resolved.Duration(time.Time{}))
	}
	if e.Down(m.ID) {
		t.Error("monitor still down")
	}
	if _, err := st.OpenIncidentFor(context.Background(), m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Error("incident not closed in store")
	}
}

func TestRestoreContinuesOpenIncident(t *testing.T) {
	cfg, st, rec := setup(t)
	m := cfg.Monitors[0]
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	feed(t, New(cfg, st, nil, rec, nil), m, start, D, D, D)

	// A restarted engine must not open a second incident for the same outage.
	restarted := New(cfg, st, nil, rec, nil)
	if err := restarted.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	feed(t, restarted, m, start.Add(time.Hour), D, D, D, U, U)
	if len(rec.events) != 2 || rec.events[1].Kind != Resolved || rec.events[1].Incident.ID != rec.events[0].Incident.ID {
		t.Fatalf("events = %+v", rec.events)
	}
}

func TestNotifierErrorDoesNotStopMonitoring(t *testing.T) {
	cfg, st, rec := setup(t)
	rec.err = errors.New("telegram down")
	e := New(cfg, st, nil, rec, nil)
	feed(t, e, cfg.Monitors[0], time.Now(), D, D, D)
	if !e.Down(cfg.Monitors[0].ID) {
		t.Error("state lost after notification error")
	}
}

type fakeChecker struct{ status probe.Status }

func (f fakeChecker) Check(_ context.Context, m config.Monitor) probe.Result {
	return probe.Result{MonitorID: m.ID, At: time.Now(), Status: f.status, Message: "fake"}
}

func TestCheckOnceStoresResult(t *testing.T) {
	cfg, st, rec := setup(t)
	e := New(cfg, st, fakeChecker{probe.Up}, rec, nil)
	e.CheckOnce(context.Background(), cfg.Monitors[0])
	if _, ok, _ := st.Latest(context.Background(), "site"); !ok {
		t.Error("result not stored")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	cfg, st, rec := setup(t)
	e := New(cfg, st, fakeChecker{probe.Up}, rec, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if _, ok, _ := st.Latest(context.Background(), "site"); !ok {
		t.Error("first check did not run immediately")
	}
}
