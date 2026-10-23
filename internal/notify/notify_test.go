package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/monitor"
	"github.com/whysafe/dozor/internal/store"
)

func event(kind monitor.EventKind) monitor.Event {
	start := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	inc := store.Incident{ID: 7, MonitorID: "api", StartedAt: start, Cause: "код ответа 502"}
	if kind == monitor.Resolved {
		inc.ResolvedAt = start.Add(95 * time.Minute)
	}
	return monitor.Event{
		Kind:     kind,
		Monitor:  config.Monitor{ID: "api", Name: "Публичный API", Type: config.TypeHTTP, URL: "https://api.example.com"},
		Incident: inc,
	}
}

func TestTelegramSendsMessage(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	tg := &Telegram{Token: "123:abc", ChatID: "-100", BaseURL: srv.URL, Loc: time.UTC}
	if err := tg.Notify(context.Background(), event(monitor.Opened)); err != nil {
		t.Fatal(err)
	}
	if path != "/bot123:abc/sendMessage" || got["chat_id"] != "-100" {
		t.Errorf("path %q, payload %v", path, got)
	}
	text, _ := got["text"].(string)
	for _, want := range []string{"Публичный API недоступен", "https://api.example.com", "код ответа 502", "01.10 09:30:00"} {
		if !strings.Contains(text, want) {
			t.Errorf("message lacks %q:\n%s", want, text)
		}
	}
}

func TestTelegramErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"ok":false,"description":"chat not found"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	tg := &Telegram{Token: "SECRET-TOKEN", ChatID: "1", BaseURL: srv.URL}
	err := tg.Notify(context.Background(), event(monitor.Opened))
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("token leaked into error: %v", err)
	}

	srv.Close()
	err = tg.Notify(context.Background(), event(monitor.Opened))
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("connection error leaks token or is nil: %v", err)
	}
}

func TestResolvedMessageShowsDowntime(t *testing.T) {
	msg := Message(event(monitor.Resolved), time.UTC)
	if !strings.Contains(msg, "снова работает") || !strings.Contains(msg, "Простой: 1 ч 35 мин") {
		t.Errorf("message:\n%s", msg)
	}
}

func TestWebhookPayload(t *testing.T) {
	var got webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	wh := &Webhook{URL: srv.URL}
	if err := wh.Notify(context.Background(), event(monitor.Resolved)); err != nil {
		t.Fatal(err)
	}
	if got.Event != "resolved" || got.IncidentID != 7 || got.DurationSeconds != 95*60 || got.ResolvedAt == nil {
		t.Errorf("payload = %+v", got)
	}
}

type failing struct{ calls *int }

func (f failing) Notify(context.Context, monitor.Event) error {
	*f.calls++
	return errors.New("boom")
}

func TestMultiCallsEveryNotifier(t *testing.T) {
	calls := 0
	err := Multi{failing{&calls}, failing{&calls}}.Notify(context.Background(), event(monitor.Opened))
	if calls != 2 || err == nil {
		t.Errorf("calls=%d err=%v", calls, err)
	}
}
