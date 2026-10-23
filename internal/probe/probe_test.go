package probe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whysafe/dozor/internal/config"
)

func httpMonitor(url string) config.Monitor {
	return config.Monitor{
		ID: "t", Type: config.TypeHTTP, URL: url, Method: "GET",
		ExpectStatus: []int{200}, Timeout: 2 * time.Second, CertWarnDays: 14,
	}
}

func TestHTTPUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<h1>Каталог</h1>")
	}))
	defer srv.Close()

	m := httpMonitor(srv.URL)
	m.Keyword = "Каталог"
	r := New().Check(context.Background(), m)
	if r.Status != Up || r.Message != "" || r.MonitorID != "t" {
		t.Fatalf("got %+v", r)
	}
}

func TestHTTPUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	m := httpMonitor(srv.URL)
	m.ExpectStatus = []int{200, 201, 202, 204}
	r := New().Check(context.Background(), m)
	if r.Status != Down || r.Message != "код ответа 502, ожидался 200–202, 204" {
		t.Fatalf("got %+v", r)
	}
}

func TestHTTPMissingKeyword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Ведутся технические работы")
	}))
	defer srv.Close()

	m := httpMonitor(srv.URL)
	m.Keyword = "Каталог"
	r := New().Check(context.Background(), m)
	if r.Status != Down || !strings.Contains(r.Message, "Каталог") {
		t.Fatalf("got %+v", r)
	}
}

func TestHTTPTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	m := httpMonitor(srv.URL)
	m.Timeout = 100 * time.Millisecond
	r := New().Check(context.Background(), m)
	if r.Status != Down || !strings.HasPrefix(r.Message, "нет ответа за") {
		t.Fatalf("got %+v", r)
	}
}

func TestHTTPSlowIsDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
	}))
	defer srv.Close()

	m := httpMonitor(srv.URL)
	m.SlowMS = 20
	r := New().Check(context.Background(), m)
	if r.Status != Degraded || !strings.Contains(r.Message, "медленный ответ") {
		t.Fatalf("got %+v", r)
	}
	if !r.Status.Available() {
		t.Error("a slow answer must count as available")
	}
}

func TestHTTPSCertificateExpiringSoon(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	p := New()
	p.Transport = srv.Client().Transport
	m := httpMonitor(srv.URL)

	r := p.Check(context.Background(), m)
	if r.CertExpiry.IsZero() {
		t.Fatal("certificate expiry not captured")
	}
	// Pretend we are a week before the test certificate expires.
	p.Now = func() time.Time { return r.CertExpiry.Add(-7*24*time.Hour - time.Hour) }
	r = p.Check(context.Background(), m)
	if r.Status != Degraded || r.Message != "сертификат истекает через 7 дней" {
		t.Fatalf("got %+v", r)
	}
}

func TestHTTPSUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	r := New().Check(context.Background(), httpMonitor(srv.URL))
	if r.Status != Down || !strings.HasPrefix(r.Message, "TLS:") {
		t.Fatalf("got %+v", r)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	m := config.Monitor{ID: "db", Type: config.TypeTCP, Address: addr, Timeout: time.Second}
	if r := New().Check(context.Background(), m); r.Status != Up {
		t.Fatalf("open port: %+v", r)
	}

	ln.Close()
	if r := New().Check(context.Background(), m); r.Status != Down || r.Message != "соединение отклонено" {
		t.Fatalf("closed port: %+v", r)
	}
}
