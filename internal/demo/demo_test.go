package demo

import (
	"context"
	"testing"
	"time"

	"github.com/whysafe/dozor/internal/monitor"
	"github.com/whysafe/dozor/internal/store"
)

func TestSeedProducesIncidentsThroughEngine(t *testing.T) {
	cfg := Config()
	st, err := store.Open(":memory:", cfg.Location)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	engine := monitor.New(cfg, st, nil, nil, nil)
	if err := Seed(context.Background(), cfg, engine, now); err != nil {
		t.Fatal(err)
	}

	if !engine.Down("cabinet") {
		t.Error("the ongoing cabinet outage should leave an open incident")
	}
	incidents, err := st.Incidents(context.Background(), now.AddDate(0, 0, -90), 50)
	if err != nil {
		t.Fatal(err)
	}
	byMonitor := map[string]int{}
	for _, inc := range incidents {
		byMonitor[inc.MonitorID]++
	}
	for _, id := range []string{"api", "shop", "smtp", "cabinet"} {
		if byMonitor[id] != 1 {
			t.Errorf("%s: %d incidents, want 1 (%v)", id, byMonitor[id], byMonitor)
		}
	}

	days, _ := st.Days(context.Background(), "cdn", now, 90)
	if days[0].Total != 0 || days[89].Total == 0 {
		t.Error("cdn history should start 50 days ago")
	}
}
