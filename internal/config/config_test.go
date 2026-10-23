package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
monitors:
  - id: site
    type: http
    url: https://example.com
  - id: db
    type: tcp
    address: db.local:5432
    interval: 30s
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != time.Minute || cfg.FailureThreshold != 3 || cfg.RecoveryThreshold != 2 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	site := cfg.Monitors[0]
	if site.Name != "site" || site.Method != "GET" || site.Timeout != 10*time.Second || site.CertWarnDays != 14 {
		t.Errorf("unexpected monitor defaults: %+v", site)
	}
	if cfg.Monitors[1].Interval != 30*time.Second {
		t.Errorf("per-monitor interval ignored: %v", cfg.Monitors[1].Interval)
	}
	if cfg.Location == nil {
		t.Error("location not resolved")
	}
}

func TestParseCollectsAllErrors(t *testing.T) {
	_, err := Parse([]byte(`
timezone: Mars/Olympus
monitors:
  - id: Bad_ID
    type: http
    url: ftp://example.com
  - id: dup
    type: tcp
    address: no-port
  - id: dup
    type: ping
    interval: 2s
`))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"timezone", "Bad_ID", "http(s)://", "host:port", "id повторяется", "type должен быть", "interval меньше 5s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("intreval: 30s\nmonitors: []\n"))
	if err == nil || !strings.Contains(err.Error(), "intreval") {
		t.Fatalf("typo not reported: %v", err)
	}
}

func TestParseExpandsEnv(t *testing.T) {
	t.Setenv("HOOK_URL", "https://hooks.example.com/abc")
	cfg, err := Parse([]byte(`
notify:
  webhook: {url: "${HOOK_URL}"}
  telegram: {chat_id: "42"}
monitors:
  - {id: a, type: tcp, address: "localhost:1"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Webhook.URL != "https://hooks.example.com/abc" {
		t.Errorf("env not expanded: %q", cfg.Notify.Webhook.URL)
	}
	if cfg.Notify.Telegram.TokenEnv != DefaultTelegramTokenEnv {
		t.Errorf("token env default: %q", cfg.Notify.Telegram.TokenEnv)
	}
}

func TestTimeoutMustBeShorterThanInterval(t *testing.T) {
	_, err := Parse([]byte(`
monitors:
  - {id: a, type: tcp, address: "localhost:1", interval: 10s, timeout: 10s}
`))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}
