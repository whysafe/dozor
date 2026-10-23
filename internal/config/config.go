// Package config loads and validates the dozor YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	TypeHTTP = "http"
	TypeTCP  = "tcp"

	DefaultTelegramTokenEnv = "DOZOR_TELEGRAM_TOKEN"
)

type Config struct {
	Title             string        `yaml:"title"`
	Timezone          string        `yaml:"timezone"`
	Interval          time.Duration `yaml:"interval"`
	Timeout           time.Duration `yaml:"timeout"`
	FailureThreshold  int           `yaml:"failure_threshold"`
	RecoveryThreshold int           `yaml:"recovery_threshold"`
	RetentionDays     int           `yaml:"retention_days"`
	Notify            Notify        `yaml:"notify"`
	Monitors          []Monitor     `yaml:"monitors"`

	Location *time.Location `yaml:"-"`
}

type Notify struct {
	Telegram *Telegram `yaml:"telegram"`
	Webhook  *Webhook  `yaml:"webhook"`
}

type Telegram struct {
	ChatID   string `yaml:"chat_id"`
	TokenEnv string `yaml:"token_env"`
}

type Webhook struct {
	URL string `yaml:"url"`
}

type Monitor struct {
	ID           string        `yaml:"id"`
	Name         string        `yaml:"name"`
	Group        string        `yaml:"group"`
	Type         string        `yaml:"type"`
	URL          string        `yaml:"url"`
	Address      string        `yaml:"address"`
	Method       string        `yaml:"method"`
	ExpectStatus []int         `yaml:"expect_status"`
	Keyword      string        `yaml:"keyword"`
	Interval     time.Duration `yaml:"interval"`
	Timeout      time.Duration `yaml:"timeout"`
	SlowMS       int           `yaml:"slow_ms"`
	CertWarnDays int           `yaml:"cert_warn_days"`
}

// Target is the human-readable thing being checked.
func (m Monitor) Target() string {
	if m.Type == TypeTCP {
		return m.Address
	}
	return m.URL
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse expands ${ENV} references, decodes strictly (unknown keys are errors,
// so a typo like "intreval" does not silently fall back to the default),
// applies defaults and validates.
func Parse(data []byte) (*Config, error) {
	expanded := os.ExpandEnv(string(data))
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("файл конфигурации пуст")
		}
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Title == "" {
		c.Title = "Статус сервисов"
	}
	if c.Timezone == "" {
		c.Timezone = "Local"
	}
	if c.Interval == 0 {
		c.Interval = time.Minute
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.FailureThreshold == 0 {
		c.FailureThreshold = 3
	}
	if c.RecoveryThreshold == 0 {
		c.RecoveryThreshold = 2
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 30
	}
	if c.Notify.Telegram != nil && c.Notify.Telegram.TokenEnv == "" {
		c.Notify.Telegram.TokenEnv = DefaultTelegramTokenEnv
	}
	for i := range c.Monitors {
		m := &c.Monitors[i]
		if m.Name == "" {
			m.Name = m.ID
		}
		if m.Interval == 0 {
			m.Interval = c.Interval
		}
		if m.Timeout == 0 {
			m.Timeout = c.Timeout
		}
		if m.Type == TypeHTTP {
			if m.Method == "" {
				m.Method = "GET"
			}
			if len(m.ExpectStatus) == 0 {
				m.ExpectStatus = []int{200, 201, 202, 203, 204, 205, 206}
			}
			if m.CertWarnDays == 0 {
				m.CertWarnDays = 14
			}
		}
	}
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		add("timezone: неизвестный часовой пояс %q", c.Timezone)
	}
	c.Location = loc

	if c.FailureThreshold < 1 || c.RecoveryThreshold < 1 {
		add("failure_threshold и recovery_threshold должны быть не меньше 1")
	}
	if c.RetentionDays < 1 {
		add("retention_days должен быть не меньше 1")
	}
	if tg := c.Notify.Telegram; tg != nil && tg.ChatID == "" {
		add("notify.telegram: не указан chat_id")
	}
	if wh := c.Notify.Webhook; wh != nil {
		if u, err := url.Parse(wh.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			add("notify.webhook: url должен начинаться с http:// или https://")
		}
	}
	if len(c.Monitors) == 0 {
		add("monitors: нужен хотя бы один монитор")
	}

	seen := map[string]bool{}
	for i, m := range c.Monitors {
		where := fmt.Sprintf("monitors[%d]", i)
		if m.ID != "" {
			where = fmt.Sprintf("monitors[%d] (%s)", i, m.ID)
		}
		if !idPattern.MatchString(m.ID) {
			add("%s: id должен состоять из a-z, 0-9 и дефиса", where)
		} else if seen[m.ID] {
			add("%s: id повторяется", where)
		}
		seen[m.ID] = true

		switch m.Type {
		case TypeHTTP:
			u, err := url.Parse(m.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("%s: url должен быть полным адресом http(s)://", where)
			}
			for _, code := range m.ExpectStatus {
				if code < 100 || code > 599 {
					add("%s: код ответа %d вне диапазона 100–599", where, code)
				}
			}
		case TypeTCP:
			if _, port, err := net.SplitHostPort(m.Address); err != nil || port == "" {
				add("%s: address должен быть в виде host:port", where)
			}
		default:
			add("%s: type должен быть http или tcp, получено %q", where, m.Type)
		}

		if m.Interval < 5*time.Second {
			add("%s: interval меньше 5s", where)
		}
		if m.Timeout <= 0 || m.Timeout >= m.Interval {
			add("%s: timeout должен быть больше нуля и меньше interval", where)
		}
	}
	return errors.Join(errs...)
}

// MonitorByID returns the monitor with the given id.
func (c *Config) MonitorByID(id string) (Monitor, bool) {
	for _, m := range c.Monitors {
		if m.ID == id {
			return m, true
		}
	}
	return Monitor{}, false
}
