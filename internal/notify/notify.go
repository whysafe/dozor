// Package notify delivers incident events to Telegram and generic webhooks.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/whysafe/dozor/internal/monitor"
	"github.com/whysafe/dozor/internal/textru"
)

// Multi fans an event out to every notifier and reports all failures together.
type Multi []monitor.Notifier

func (m Multi) Notify(ctx context.Context, e monitor.Event) error {
	var errs []error
	for _, n := range m {
		if err := n.Notify(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type Telegram struct {
	Token   string
	ChatID  string
	BaseURL string
	Client  *http.Client
	Loc     *time.Location
}

func (t *Telegram) Notify(ctx context.Context, e monitor.Event) error {
	payload := map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     Message(e, t.Loc),
		"disable_web_page_preview": true,
	}
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	// The token is part of the URL, so errors must never echo the URL back.
	if err := postJSON(ctx, t.Client, base+"/bot"+t.Token+"/sendMessage", payload); err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	return nil
}

// Message renders the plain-text notification shown in Telegram.
func Message(e monitor.Event, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder
	switch e.Kind {
	case monitor.Opened:
		fmt.Fprintf(&b, "🔴 %s недоступен\n", e.Monitor.Name)
		fmt.Fprintf(&b, "%s\n", e.Monitor.Target())
		fmt.Fprintf(&b, "Причина: %s\n", e.Incident.Cause)
		fmt.Fprintf(&b, "Начало: %s", e.Incident.StartedAt.In(loc).Format("02.01 15:04:05"))
	case monitor.Resolved:
		fmt.Fprintf(&b, "🟢 %s снова работает\n", e.Monitor.Name)
		fmt.Fprintf(&b, "%s\n", e.Monitor.Target())
		fmt.Fprintf(&b, "Простой: %s\n", textru.Duration(e.Incident.Duration(e.Incident.ResolvedAt)))
		fmt.Fprintf(&b, "Причина была: %s", e.Incident.Cause)
	}
	return b.String()
}

type Webhook struct {
	URL    string
	Client *http.Client
}

type webhookPayload struct {
	Event           string     `json:"event"`
	MonitorID       string     `json:"monitor_id"`
	MonitorName     string     `json:"monitor_name"`
	Target          string     `json:"target"`
	IncidentID      int64      `json:"incident_id"`
	Cause           string     `json:"cause"`
	StartedAt       time.Time  `json:"started_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	DurationSeconds int64      `json:"duration_seconds,omitempty"`
}

func (w *Webhook) Notify(ctx context.Context, e monitor.Event) error {
	p := webhookPayload{
		Event:       string(e.Kind),
		MonitorID:   e.Monitor.ID,
		MonitorName: e.Monitor.Name,
		Target:      e.Monitor.Target(),
		IncidentID:  e.Incident.ID,
		Cause:       e.Incident.Cause,
		StartedAt:   e.Incident.StartedAt.UTC(),
	}
	if !e.Incident.Open() {
		resolved := e.Incident.ResolvedAt.UTC()
		p.ResolvedAt = &resolved
		p.DurationSeconds = int64(e.Incident.Duration(resolved).Seconds())
	}
	if err := postJSON(ctx, w.Client, w.URL, p); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}

// postJSON never includes the URL in its errors: for Telegram the URL holds the bot token.
func postJSON(ctx context.Context, client *http.Client, url string, payload any) error {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return errors.New("некорректный адрес")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return errors.New("нет связи с сервером уведомлений")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("ответ %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}
