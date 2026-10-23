// Package demo fills a store with 90 days of believable history so the status
// page can be shown without real services: a few outages, slow days, a
// monitor added later and one incident that is still open.
package demo

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/monitor"
	"github.com/whysafe/dozor/internal/probe"
)

const yamlConfig = `
title: Статус сервисов «Северного ветра»
timezone: Europe/Moscow
interval: 5m
timeout: 10s
monitors:
  - {id: shop, name: Интернет-магазин, group: Для клиентов, type: http, url: "https://shop.example.com", keyword: Каталог, slow_ms: 1200}
  - {id: cabinet, name: Личный кабинет, group: Для клиентов, type: http, url: "https://lk.example.com/health"}
  - {id: api, name: Публичный API, group: Для клиентов, type: http, url: "https://api.example.com/v1/ping"}
  - {id: cdn, name: CDN картинок, group: Для клиентов, type: http, url: "https://img.example.com/ping.gif"}
  - {id: db, name: PostgreSQL, group: Инфраструктура, type: tcp, address: "db.internal.example.com:5432"}
  - {id: smtp, name: Почта (SMTP), group: Инфраструктура, type: tcp, address: "mail.example.com:587"}
`

// Config returns the configuration of the demo installation.
func Config() *config.Config {
	cfg, err := config.Parse([]byte(yamlConfig))
	if err != nil {
		panic("demo config: " + err.Error())
	}
	return cfg
}

type outage struct {
	monitor string
	ago     time.Duration // start, counted back from now
	length  time.Duration // zero means still ongoing
	status  probe.Status
	message string
}

var outages = []outage{
	{"api", 12*24*time.Hour - 3*time.Hour, 95 * time.Minute, probe.Down, "код ответа 502, ожидался 200–206"},
	{"shop", 63*24*time.Hour + 5*time.Hour, 80 * time.Minute, probe.Down, "нет ответа за 10 с"},
	{"shop", 40 * 24 * time.Hour, 6 * time.Hour, probe.Degraded, "медленный ответ: 1840 мс при пороге 1200 мс"},
	{"smtp", 3*24*time.Hour + 7*time.Hour, 25 * time.Minute, probe.Down, "соединение отклонено"},
	{"db", 27*24*time.Hour + 2*time.Hour, 3 * time.Hour, probe.Degraded, "медленный ответ при резервном копировании"},
	{"cabinet", 35 * time.Minute, 0, probe.Down, "нет ответа за 10 с"},
	{"cdn", 2 * 24 * time.Hour, 0, probe.Degraded, "сертификат истекает через 9 дней"},
}

// cdnAge: the CDN monitor was added later, so older days have no data.
const cdnAge = 50 * 24 * time.Hour

var baseLatency = map[string]float64{"shop": 420, "cabinet": 260, "api": 140, "cdn": 60, "db": 4, "smtp": 35}

// Seed feeds generated results through the engine, so incidents are opened
// and resolved by the same logic as in production.
func Seed(ctx context.Context, cfg *config.Config, engine *monitor.Engine, now time.Time) error {
	start := now.Add(-90 * 24 * time.Hour)
	for _, m := range cfg.Monitors {
		rng := rand.New(rand.NewPCG(42, seedFor(m.ID)))
		t := start
		if m.ID == "cdn" {
			t = now.Add(-cdnAge)
		}
		for !t.After(now) {
			if err := engine.Observe(ctx, m, result(m, t, now, rng)); err != nil {
				return err
			}
			t = t.Add(step(now.Sub(t)))
		}
	}
	return nil
}

// step keeps the last two weeks detailed and older history coarse, which is
// enough for daily bars and keeps seeding fast.
func step(age time.Duration) time.Duration {
	if age > 14*24*time.Hour {
		return 20 * time.Minute
	}
	return 5 * time.Minute
}

func result(m config.Monitor, t, now time.Time, rng *rand.Rand) probe.Result {
	hour := float64(t.Hour()) + float64(t.Minute())/60
	// Busier in the afternoon: latency follows a daily wave plus noise.
	wave := 1 + 0.35*math.Sin((hour-9)/24*2*math.Pi)
	latency := baseLatency[m.ID] * wave * (0.8 + 0.4*rng.Float64())
	r := probe.Result{MonitorID: m.ID, At: t, Status: probe.Up, Latency: ms(latency)}

	for _, o := range outages {
		from := now.Add(-o.ago)
		to := from.Add(o.length)
		if o.monitor != m.ID || t.Before(from) || (o.length > 0 && !t.Before(to)) {
			continue
		}
		r.Status, r.Message = o.status, o.message
		switch o.status {
		case probe.Down:
			r.Latency = m.Timeout
		case probe.Degraded:
			r.Latency = ms(latency * 3.5)
		}
		return r
	}

	// Rare one-off failures: a single blip never reaches the incident threshold.
	if rng.Float64() < 0.0006 {
		r.Status, r.Message, r.Latency = probe.Down, "соединение сброшено", ms(latency)
	}
	return r
}

func ms(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func seedFor(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return h.Sum64()
}
