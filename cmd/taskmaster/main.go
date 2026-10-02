// Command taskmaster is a single-user JMAP for Task Lists server backed by
// SQLite.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/server"
	"github.com/rjbs/taskmaster/internal/store"
	"github.com/rjbs/taskmaster/internal/tasks"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	dbPath := flag.String("db", "taskmaster.sqlite", "path to the SQLite database")
	account := flag.String("account", "rjbs", "account id and username to serve")
	tzName := flag.String("tz", "America/New_York", "time zone for tasks without one")
	baseURL := flag.String("base-url", "", "public base URL for the Session object (default: derived from each request)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	loc, err := time.LoadLocation(*tzName)
	if err != nil {
		logger.Error("bad -tz", "error", err)
		os.Exit(1)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	svc := tasks.New(db, tasks.Config{AccountID: *account, DefaultTimeZone: loc})
	if err := svc.EnsureDefaultTaskList(context.Background()); err != nil {
		logger.Error("creating default task list", "error", err)
		os.Exit(1)
	}

	proc := jmap.NewProcessor()
	svc.Register(proc)

	broker := server.NewBroker()
	db.OnCommit(broker.Publish)

	srv := &http.Server{
		Addr: *listen,
		Handler: logRequests(logger, server.New(server.Config{
			AccountID: *account,
			BaseURL:   *baseURL,
			Tasks:     svc,
			Processor: proc,
			Broker:    broker,
			Logger:    logger,
		})),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "addr", *listen, "db", *dbPath, "account", *account)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start).Round(time.Millisecond))
	})
}
