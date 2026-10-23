// Package web serves the public status page, the JSON API and uptime badges.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/store"
)

//go:embed assets
var assets embed.FS

type Server struct {
	cfg   *config.Config
	store *store.Store
	now   func() time.Time
	log   *slog.Logger
	page  *template.Template
	css   template.CSS
	// Refresh is the meta-refresh period of the status page; zero disables it.
	Refresh int
}

func New(cfg *config.Config, st *store.Store, now func() time.Time, log *slog.Logger) (*Server, error) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	css, err := assets.ReadFile("assets/style.css")
	if err != nil {
		return nil, err
	}
	page, err := template.New("status.html").Funcs(template.FuncMap{
		"lower": strings.ToLower,
	}).ParseFS(assets, "assets/status.html")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, now: now, log: log, page: page, css: template.CSS(css), Refresh: 60}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/monitors/{id}/days", s.handleDays)
	mux.HandleFunc("GET /api/incidents", s.handleIncidents)
	mux.HandleFunc("GET /badge/{file}", s.handleBadge)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// Render writes the full status page; it is also used for static snapshots.
func (s *Server) Render(ctx context.Context, w io.Writer) error {
	data, err := s.buildPage(ctx)
	if err != nil {
		return err
	}
	return s.page.Execute(w, data)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildPage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.page.Execute(w, data); err != nil {
		s.log.Error("render status page", "err", err)
	}
}

type apiMonitor struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Group     string   `json:"group"`
	Target    string   `json:"target"`
	State     string   `json:"state"`
	Message   string   `json:"message,omitempty"`
	LatencyMS *int64   `json:"latency_ms,omitempty"`
	CheckedAt *string  `json:"checked_at,omitempty"`
	Uptime24h *float64 `json:"uptime_24h,omitempty"`
	Uptime30d *float64 `json:"uptime_30d,omitempty"`
	Uptime90d *float64 `json:"uptime_90d,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildPage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := struct {
		State    string       `json:"state"`
		Monitors []apiMonitor `json:"monitors"`
	}{State: data.Overall.State}
	for _, g := range data.Groups {
		for _, m := range g.Monitors {
			out.Monitors = append(out.Monitors, m.api)
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleDays(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.cfg.MonitorByID(id); !ok {
		http.Error(w, "unknown monitor", http.StatusNotFound)
		return
	}
	n := 90
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 366 {
			http.Error(w, "n must be 1..366", http.StatusBadRequest)
			return
		}
		n = parsed
	}
	days, err := s.store.Days(r.Context(), id, s.now(), n)
	if err != nil {
		s.fail(w, err)
		return
	}
	type apiDay struct {
		Day          string  `json:"day"`
		Checks       int     `json:"checks"`
		Failed       int     `json:"failed"`
		Degraded     int     `json:"degraded"`
		Uptime       float64 `json:"uptime"`
		AvgLatencyMS int64   `json:"avg_latency_ms"`
	}
	out := make([]apiDay, len(days))
	for i, d := range days {
		out[i] = apiDay{d.Day, d.Total, d.Total - d.Available, d.Degraded, d.Uptime(), d.AvgLatency.Milliseconds()}
	}
	writeJSON(w, out)
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Incidents(r.Context(), s.now().AddDate(0, 0, -90), 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	type apiIncident struct {
		ID              int64      `json:"id"`
		MonitorID       string     `json:"monitor_id"`
		Cause           string     `json:"cause"`
		StartedAt       time.Time  `json:"started_at"`
		ResolvedAt      *time.Time `json:"resolved_at"`
		DurationSeconds int64      `json:"duration_seconds"`
	}
	out := make([]apiIncident, 0, len(list))
	for _, inc := range list {
		item := apiIncident{
			ID: inc.ID, MonitorID: inc.MonitorID, Cause: inc.Cause,
			StartedAt:       inc.StartedAt.UTC(),
			DurationSeconds: int64(inc.Duration(s.now()).Seconds()),
		}
		if !inc.Open() {
			t := inc.ResolvedAt.UTC()
			item.ResolvedAt = &t
		}
		out = append(out, item)
	}
	writeJSON(w, out)
}

func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".svg")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, known := s.cfg.MonitorByID(id); !known {
		http.NotFound(w, r)
		return
	}
	n := 30
	if r.URL.Query().Get("period") == "90d" {
		n = 90
	}
	days, err := s.store.Days(r.Context(), id, s.now(), n)
	if err != nil {
		s.fail(w, err)
		return
	}
	ratio, has := store.Uptime(days)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "max-age=300")
	io.WriteString(w, Badge("uptime "+strconv.Itoa(n)+"d", ratio, has))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
