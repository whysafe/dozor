// Package probe performs a single HTTP or TCP check and classifies the outcome.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/textru"
)

type Status string

const (
	Up       Status = "up"
	Degraded Status = "degraded"
	Down     Status = "down"
)

// Available reports whether the service answered correctly; a slow answer
// still counts as available for uptime.
func (s Status) Available() bool { return s == Up || s == Degraded }

type Result struct {
	MonitorID  string
	At         time.Time
	Status     Status
	Latency    time.Duration
	Message    string
	CertExpiry time.Time
}

// keywordReadLimit caps how much of a body is scanned for the keyword.
const keywordReadLimit = 2 << 20

type Prober struct {
	Transport http.RoundTripper
	Dialer    *net.Dialer
	Now       func() time.Time
}

func New() *Prober {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Every check must open a fresh connection, otherwise a dead backend
	// behind a kept-alive socket is noticed only after the socket times out.
	transport.DisableKeepAlives = true
	return &Prober{Transport: transport, Dialer: &net.Dialer{}, Now: time.Now}
}

func (p *Prober) Check(ctx context.Context, m config.Monitor) Result {
	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()

	started := p.Now()
	var r Result
	switch m.Type {
	case config.TypeTCP:
		r = p.checkTCP(ctx, m)
	default:
		r = p.checkHTTP(ctx, m)
	}
	r.MonitorID = m.ID
	r.At = started
	if r.Latency == 0 {
		r.Latency = p.Now().Sub(started)
	}
	if r.Status == Down && r.Message == "" {
		r.Message = "ошибка проверки"
	}
	if r.Status == Up && m.SlowMS > 0 && r.Latency > time.Duration(m.SlowMS)*time.Millisecond {
		r.Status = Degraded
		r.Message = fmt.Sprintf("медленный ответ: %d мс при пороге %d мс", r.Latency.Milliseconds(), m.SlowMS)
	}
	return r
}

func (p *Prober) checkTCP(ctx context.Context, m config.Monitor) Result {
	start := p.Now()
	conn, err := p.Dialer.DialContext(ctx, "tcp", m.Address)
	if err != nil {
		return Result{Status: Down, Message: describe(err, m.Timeout)}
	}
	latency := p.Now().Sub(start)
	conn.Close()
	return Result{Status: Up, Latency: latency}
}

func (p *Prober) checkHTTP(ctx context.Context, m config.Monitor) Result {
	req, err := http.NewRequestWithContext(ctx, m.Method, m.URL, nil)
	if err != nil {
		return Result{Status: Down, Message: err.Error()}
	}
	req.Header.Set("User-Agent", "dozor-uptime/1.0")

	client := &http.Client{Transport: p.Transport}
	start := p.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: Down, Message: describe(err, m.Timeout)}
	}
	defer resp.Body.Close()

	found := true
	if m.Keyword != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, keywordReadLimit))
		if err != nil {
			return Result{Status: Down, Message: "обрыв при чтении ответа: " + describe(err, m.Timeout)}
		}
		found = strings.Contains(string(body), m.Keyword)
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, keywordReadLimit))
	}
	latency := p.Now().Sub(start)

	r := Result{Status: Up, Latency: latency}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.CertExpiry = resp.TLS.PeerCertificates[0].NotAfter
	}

	switch {
	case !slices.Contains(m.ExpectStatus, resp.StatusCode):
		r.Status = Down
		r.Message = fmt.Sprintf("код ответа %d, ожидался %s", resp.StatusCode, joinCodes(m.ExpectStatus))
	case !found:
		r.Status = Down
		r.Message = fmt.Sprintf("в ответе нет текста «%s»", m.Keyword)
	case !r.CertExpiry.IsZero() && m.CertWarnDays > 0:
		left := r.CertExpiry.Sub(p.Now())
		if left < time.Duration(m.CertWarnDays)*24*time.Hour {
			days := int64(left.Hours() / 24)
			r.Status = Degraded
			r.Message = fmt.Sprintf("сертификат истекает через %d %s", days, textru.Plural(days, "день", "дня", "дней"))
		}
	}
	return r
}

// joinCodes collapses runs of consecutive codes: [200 201 202 301] → "200–202, 301".
func joinCodes(codes []int) string {
	sorted := slices.Clone(codes)
	slices.Sort(sorted)
	var parts []string
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d–%d", sorted[i], sorted[j]))
		} else {
			parts = append(parts, fmt.Sprint(sorted[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// describe turns low-level network errors into short messages for the status page.
func describe(err error, timeout time.Duration) string {
	var (
		dnsErr   *net.DNSError
		certErr  *tls.CertificateVerificationError
		invalid  x509.CertificateInvalidError
		hostErr  x509.HostnameError
		netErr   net.Error
		unknownA x509.UnknownAuthorityError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "нет ответа за " + textru.Duration(timeout)
	case errors.As(err, &dnsErr):
		return "DNS: не удалось найти " + dnsErr.Name
	// Windows reports WSAECONNREFUSED, which does not match syscall.ECONNREFUSED.
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(err.Error(), "actively refused"):
		return "соединение отклонено"
	case errors.Is(err, syscall.ECONNRESET):
		return "соединение сброшено"
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return "TLS: сертификат просрочен"
	case errors.As(err, &hostErr):
		return "TLS: сертификат выдан на другое имя"
	case errors.As(err, &unknownA):
		return "TLS: сертификат подписан неизвестным центром"
	case errors.As(err, &certErr):
		return "TLS: " + certErr.Err.Error()
	default:
		return err.Error()
	}
}
