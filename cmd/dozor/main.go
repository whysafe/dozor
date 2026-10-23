// Command dozor is a self-hosted uptime monitor with a public status page.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // timezones must resolve inside scratch containers and on Windows

	"github.com/whysafe/dozor/internal/config"
	"github.com/whysafe/dozor/internal/demo"
	"github.com/whysafe/dozor/internal/monitor"
	"github.com/whysafe/dozor/internal/notify"
	"github.com/whysafe/dozor/internal/probe"
	"github.com/whysafe/dozor/internal/store"
	"github.com/whysafe/dozor/internal/web"
)

var version = "dev"

const usage = `dozor — мониторинг доступности сайтов и серверов со страницей статуса.

Команды:
  dozor serve  [-config dozor.yaml] [-db dozor.db] [-addr :8080]   проверять по расписанию и показывать статус
  dozor check  [-config dozor.yaml]                                 проверить всё один раз (код 1, если есть сбой)
  dozor validate [-config dozor.yaml]                               проверить конфигурацию
  dozor demo   [-addr :8080] [-render page.html]                    страница статуса на сгенерированных данных
  dozor version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = runServe(ctx, args)
	case "check":
		err = runCheck(ctx, args, os.Stdout)
	case "validate":
		err = runValidate(args, os.Stdout)
	case "demo":
		err = runDemo(ctx, args)
	case "version", "-v", "--version":
		fmt.Println("dozor", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	var exit exitError
	switch {
	case errors.As(err, &exit):
		os.Exit(int(exit))
	case err != nil:
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

// exitError ends the process with a status code and no extra message.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("DOZOR_DEBUG") != "" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "dozor.yaml", "файл конфигурации")
	dbPath := fs.String("db", "dozor.db", "файл базы SQLite")
	addr := fs.String("addr", ":8080", "адрес HTTP-сервера")
	fs.Parse(args)

	log := newLogger()
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(*dbPath, cfg.Location)
	if err != nil {
		return err
	}
	defer st.Close()

	notifier, err := buildNotifier(cfg)
	if err != nil {
		return err
	}
	engine := monitor.New(cfg, st, probe.New(), notifier, log)
	if err := engine.Restore(ctx); err != nil {
		return err
	}
	srv, err := web.New(cfg, st, nil, log)
	if err != nil {
		return err
	}

	// Bind before the first check, so a monitor pointed at dozor itself
	// (or at a proxy in front of it) never sees a refused connection on startup.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	go engine.Run(ctx)
	log.Info("dozor started", "addr", ln.Addr().String(), "monitors", len(cfg.Monitors), "version", version)
	return serve(ctx, ln, srv.Handler())
}

func buildNotifier(cfg *config.Config) (monitor.Notifier, error) {
	var list notify.Multi
	if tg := cfg.Notify.Telegram; tg != nil {
		token := os.Getenv(tg.TokenEnv)
		if token == "" {
			return nil, fmt.Errorf("уведомления в Telegram включены, но переменная %s пуста", tg.TokenEnv)
		}
		list = append(list, &notify.Telegram{Token: token, ChatID: tg.ChatID, Loc: cfg.Location})
	}
	if wh := cfg.Notify.Webhook; wh != nil {
		list = append(list, &notify.Webhook{URL: wh.URL})
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, nil
}

func serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func runCheck(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", "dozor.yaml", "файл конфигурации")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	results := make([]probe.Result, len(cfg.Monitors))
	done := make(chan struct{})
	prober := probe.New()
	for i, m := range cfg.Monitors {
		go func() {
			results[i] = prober.Check(ctx, m)
			done <- struct{}{}
		}()
	}
	for range cfg.Monitors {
		<-done
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "МОНИТОР\tСТАТУС\tОТКЛИК\tКОММЕНТАРИЙ")
	failed := 0
	for i, m := range cfg.Monitors {
		r := results[i]
		if r.Status == probe.Down {
			failed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%d мс\t%s\n", m.ID, statusLabel(r.Status), r.Latency.Milliseconds(), r.Message)
	}
	tw.Flush()
	if failed > 0 {
		return exitError(1)
	}
	return nil
}

func statusLabel(s probe.Status) string {
	switch s {
	case probe.Up:
		return "ok"
	case probe.Degraded:
		return "warn"
	default:
		return "DOWN"
	}
}

func runValidate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	cfgPath := fs.String("config", "dozor.yaml", "файл конфигурации")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: всё в порядке, мониторов: %d\n", *cfgPath, len(cfg.Monitors))
	return nil
}

func runDemo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "адрес HTTP-сервера")
	render := fs.String("render", "", "сохранить страницу в HTML-файл и выйти")
	fs.Parse(args)

	cfg := demo.Config()
	st, err := store.Open(":memory:", cfg.Location)
	if err != nil {
		return err
	}
	defer st.Close()

	now := time.Now()
	engine := monitor.New(cfg, st, nil, nil, nil)
	if err := demo.Seed(ctx, cfg, engine, now); err != nil {
		return err
	}
	srv, err := web.New(cfg, st, func() time.Time { return now }, newLogger())
	if err != nil {
		return err
	}

	if *render != "" {
		srv.Refresh = 0
		var buf bytes.Buffer
		if err := srv.Render(ctx, &buf); err != nil {
			return err
		}
		return os.WriteFile(*render, buf.Bytes(), 0o644)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "демо-страница: http://%s\n", ln.Addr())
	return serve(ctx, ln, srv.Handler())
}
