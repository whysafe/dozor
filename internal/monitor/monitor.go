// Package monitor schedules checks and turns streaks of results into incidents.
package monitor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/probe"
	"github.com/whysafe/dozor/internal/store"
)

type EventKind string

const (
	Opened   EventKind = "opened"
	Resolved EventKind = "resolved"
)

type Event struct {
	Kind     EventKind
	Monitor  config.Monitor
	Incident store.Incident
	Result   probe.Result
}

type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

type Checker interface {
	Check(ctx context.Context, m config.Monitor) probe.Result
}

// state is the per-monitor streak counter. An incident opens after
// FailureThreshold failures in a row and closes after RecoveryThreshold
// successes in a row, so a single lost packet never pages anyone.
type state struct {
	fails      int
	oks        int
	firstFail  time.Time
	firstCause string
	incident   *store.Incident
}

type Engine struct {
	cfg      *config.Config
	store    *store.Store
	checker  Checker
	notifier Notifier
	log      *slog.Logger

	mu     sync.Mutex
	states map[string]*state
}

func New(cfg *config.Config, st *store.Store, checker Checker, notifier Notifier, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{cfg: cfg, store: st, checker: checker, notifier: notifier, log: log, states: map[string]*state{}}
	for _, m := range cfg.Monitors {
		e.states[m.ID] = &state{}
	}
	return e
}

// Restore picks up incidents left open by a previous run, so a restart during
// an outage neither forgets it nor opens a duplicate.
func (e *Engine) Restore(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.cfg.Monitors {
		inc, err := e.store.OpenIncidentFor(ctx, m.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		e.states[m.ID].incident = &inc
	}
	return nil
}

// Down reports whether the monitor currently has an open incident.
func (e *Engine) Down(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.states[id]
	return ok && st.incident != nil
}

// Observe stores a result, advances the streak state and notifies on transitions.
func (e *Engine) Observe(ctx context.Context, m config.Monitor, r probe.Result) error {
	if err := e.store.Record(ctx, r); err != nil {
		return err
	}
	event, err := e.advance(ctx, m, r)
	if err != nil || event == nil {
		return err
	}

	e.log.Info("incident "+string(event.Kind), "monitor", m.ID, "incident", event.Incident.ID, "cause", event.Incident.Cause)
	if e.notifier != nil {
		if err := e.notifier.Notify(ctx, *event); err != nil {
			// A broken notification channel must not stop monitoring.
			e.log.Error("notification failed", "monitor", m.ID, "err", err)
		}
	}
	return nil
}

func (e *Engine) advance(ctx context.Context, m config.Monitor, r probe.Result) (*Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.states[m.ID]
	if st == nil {
		st = &state{}
		e.states[m.ID] = st
	}

	if r.Status == probe.Down {
		st.oks = 0
		if st.fails == 0 {
			st.firstFail, st.firstCause = r.At, r.Message
		}
		st.fails++
		if st.incident != nil || st.fails < e.cfg.FailureThreshold {
			return nil, nil
		}
		inc, err := e.store.OpenIncident(ctx, m.ID, st.firstFail, st.firstCause)
		if err != nil {
			return nil, err
		}
		st.incident = &inc
		return &Event{Kind: Opened, Monitor: m, Incident: inc, Result: r}, nil
	}

	st.fails = 0
	if st.incident == nil {
		return nil, nil
	}
	st.oks++
	if st.oks < e.cfg.RecoveryThreshold {
		return nil, nil
	}
	inc := *st.incident
	if err := e.store.ResolveIncident(ctx, inc.ID, r.At); err != nil {
		return nil, err
	}
	inc.ResolvedAt = r.At
	st.incident, st.oks = nil, 0
	return &Event{Kind: Resolved, Monitor: m, Incident: inc, Result: r}, nil
}

// Run checks every monitor on its own interval until ctx is cancelled.
// Start times are spread across the first interval to avoid a burst of
// simultaneous requests on startup.
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	n := len(e.cfg.Monitors)
	for i, m := range e.cfg.Monitors {
		offset := m.Interval * time.Duration(i) / time.Duration(n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.loop(ctx, m, offset)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.pruneLoop(ctx)
	}()
	wg.Wait()
}

func (e *Engine) loop(ctx context.Context, m config.Monitor, offset time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(offset):
	}

	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		e.CheckOnce(ctx, m)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// CheckOnce runs one check of m and observes the result.
func (e *Engine) CheckOnce(ctx context.Context, m config.Monitor) probe.Result {
	r := e.checker.Check(ctx, m)
	if ctx.Err() != nil {
		return r // shutting down: a cancelled check is not an outage
	}
	if err := e.Observe(ctx, m, r); err != nil {
		e.log.Error("store result", "monitor", m.ID, "err", err)
	}
	e.log.Debug("checked", "monitor", m.ID, "status", r.Status, "latency", r.Latency, "message", r.Message)
	return r
}

func (e *Engine) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		cutoff := time.Now().AddDate(0, 0, -e.cfg.RetentionDays)
		if n, err := e.store.Prune(ctx, cutoff); err != nil && ctx.Err() == nil {
			e.log.Error("prune", "err", err)
		} else if n > 0 {
			e.log.Info("pruned old results", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
