package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/probe"
	"github.com/whysafe/dozor/internal/store"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
title: Тестовый статус
timezone: UTC
monitors:
  - {id: site, name: "Сайт <b>", group: Клиентам, type: http, url: "https://example.com"}
  - {id: db, name: База, group: Инфраструктура, type: tcp, address: "db:5432"}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	for i := range 10 {
		at := now.Add(-time.Duration(i) * 10 * time.Minute)
		st.Record(ctx, probe.Result{MonitorID: "site", At: at, Status: probe.Up, Latency: 120 * time.Millisecond})
		status := probe.Up
		if i < 4 {
			status = probe.Down
		}
		st.Record(ctx, probe.Result{MonitorID: "db", At: at, Status: status, Latency: 5 * time.Millisecond, Message: "соединение отклонено"})
	}
	st.OpenIncident(ctx, "db", now.Add(-40*time.Minute), "соединение отклонено")

	srv, err := New(cfg, st, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestStatusPage(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := get(t, ts.URL+"/")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		"Тестовый статус", "Частичный сбой", "Недоступно: 1 из 2",
		"Клиентам", "Инфраструктура", "Сайт &lt;b&gt;", "Недоступен",
		"Инциденты за 30 дней", "продолжается 40 мин", `class="spark__line"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Count(body, `class="bar bar--`) != 2*historyDays {
		t.Errorf("expected %d day bars", 2*historyDays)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security header missing")
	}
}

func TestStatusAPI(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := get(t, ts.URL+"/api/status")
	var out struct {
		State    string       `json:"state"`
		Monitors []apiMonitor `json:"monitors"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "down" || len(out.Monitors) != 2 {
		t.Fatalf("got %+v", out)
	}
	site, db := out.Monitors[0], out.Monitors[1]
	if site.State != "up" || *site.LatencyMS != 120 || *site.Uptime24h != 1 {
		t.Errorf("site = %+v", site)
	}
	if db.State != "down" || *db.Uptime24h != 0.6 {
		t.Errorf("db = %+v", db)
	}
}

func TestDaysAPI(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := get(t, ts.URL+"/api/monitors/db/days?n=2")
	var days []map[string]any
	json.Unmarshal([]byte(body), &days)
	if len(days) != 2 || days[1]["day"] != "2026-10-20" || days[1]["failed"] != 4.0 {
		t.Fatalf("days = %s", body)
	}
	if resp, _ := get(t, ts.URL+"/api/monitors/nope/days"); resp.StatusCode != 404 {
		t.Errorf("unknown monitor: %d", resp.StatusCode)
	}
	if resp, _ := get(t, ts.URL+"/api/monitors/db/days?n=1000"); resp.StatusCode != 400 {
		t.Errorf("bad n: %d", resp.StatusCode)
	}
}

func TestIncidentsAPI(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := get(t, ts.URL+"/api/incidents")
	if !strings.Contains(body, `"resolved_at": null`) || !strings.Contains(body, `"duration_seconds": 2400`) {
		t.Errorf("incidents = %s", body)
	}
}

func TestBadge(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := get(t, ts.URL+"/badge/site.svg")
	if resp.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(body, "100%") || !strings.Contains(body, "#2e7d4f") {
		t.Errorf("badge = %s", body)
	}
	if _, body := get(t, ts.URL+"/badge/db.svg?period=90d"); !strings.Contains(body, "uptime 90d") || !strings.Contains(body, "60.00%") {
		t.Errorf("db badge = %s", body)
	}
	if resp, _ := get(t, ts.URL+"/badge/nope.svg"); resp.StatusCode != 404 {
		t.Errorf("unknown badge: %d", resp.StatusCode)
	}
}

func TestSparklineSkipsEmptyBuckets(t *testing.T) {
	results := []probe.Result{
		{At: now.Add(-23 * time.Hour), Status: probe.Up, Latency: 100 * time.Millisecond},
		{At: now.Add(-time.Hour), Status: probe.Up, Latency: 300 * time.Millisecond},
		{At: now.Add(-time.Hour), Status: probe.Down, Latency: 10 * time.Second},
	}
	line, area, caption := sparkline(results, now)
	if strings.Count(line, ",") != 2 || !strings.HasPrefix(area, "M") || caption != "100–300 мс" {
		t.Errorf("line=%q area=%q caption=%q", line, area, caption)
	}
	if l, _, _ := sparkline(nil, now); l != "" {
		t.Error("empty input must give empty sparkline")
	}
}

func TestDayBarClasses(t *testing.T) {
	cases := []struct {
		d    store.DayStat
		want string
	}{
		{store.DayStat{Day: "2026-10-01"}, "none"},
		{store.DayStat{Day: "2026-10-01", Total: 100, Available: 100}, "ok"},
		{store.DayStat{Day: "2026-10-01", Total: 100, Available: 99}, "minor"},
		{store.DayStat{Day: "2026-10-01", Total: 100, Available: 97}, "warn"},
		{store.DayStat{Day: "2026-10-01", Total: 100, Available: 90}, "bad"},
		{store.DayStat{Day: "2026-10-01", Total: 100, Available: 100, Degraded: 20}, "minor"},
	}
	for _, c := range cases {
		if got := dayBar(c.d, time.UTC).Class; got != c.want {
			t.Errorf("%+v → %s, want %s", c.d, got, c.want)
		}
	}
}
