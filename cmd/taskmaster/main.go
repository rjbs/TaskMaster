// Command taskmaster is a small JMAP for Task Lists server backed by SQLite,
// with one database per account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/rjbs/taskmaster/internal/auth"
	"github.com/rjbs/taskmaster/internal/config"
	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/server"
	"github.com/rjbs/taskmaster/internal/store"
	"github.com/rjbs/taskmaster/internal/tailnet"
	"github.com/rjbs/taskmaster/internal/tasks"

	taskblaster "github.com/rjbs/taskmaster/demo-client"
)

func main() {
	configPath := flag.String("config", "taskmaster.json", "path to the configuration file")
	verbose := flag.Bool("v", false, "log at debug level (includes Tailscale's chatter)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [-config FILE] [-v]\n       %s new-token\n", os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	switch {
	case flag.NArg() == 1 && flag.Arg(0) == "new-token":
		if err := newToken(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case flag.NArg() > 0:
		flag.Usage()
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(logger, *configPath); err != nil {
		logger.Error("taskmaster failed", "error", err)
		os.Exit(1)
	}
}

// newToken prints a fresh bearer token, for the client, and the credential
// that accepts it, for the configuration file.
func newToken() error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	fmt.Printf("token (give this to the client; it is not stored anywhere):\n  %s\n\n", token)
	fmt.Printf("credential (add this to a user's credentials):\n  { \"type\": \"bearer\", \"tokenHash\": \"%s\" }\n", auth.HashToken(token))
	return nil
}

func run(logger *slog.Logger, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.Listen == "" && cfg.Tailscale == nil {
		return errors.New("nothing to do: no listen address and no tailscale section")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := tasks.NewRegistry()
	broker := server.NewBroker()
	ids := make([]string, 0, len(cfg.Directory.Accounts))
	for id := range cfg.Directory.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		acct := cfg.Directory.Accounts[id]
		_, statErr := os.Stat(acct.Path)
		fresh := errors.Is(statErr, fs.ErrNotExist)
		db, err := store.Open(acct.Path)
		if err != nil {
			return fmt.Errorf("opening database for account %q: %w", id, err)
		}
		defer db.Close()
		svc := tasks.New(db, tasks.Config{AccountID: id, DefaultTimeZone: cfg.Location})
		if err := svc.EnsureDefaultTaskList(ctx); err != nil {
			return fmt.Errorf("creating default task list for account %q: %w", id, err)
		}
		db.OnCommit(broker.Publisher(id))
		reg.Add(svc)
		if fresh {
			logger.Warn("account database did not exist; created an empty one", "id", id, "owner", acct.Owner, "db", acct.Path)
		} else {
			logger.Info("account", "id", id, "owner", acct.Owner, "db", acct.Path)
		}
	}

	proc := jmap.NewProcessor()
	reg.Register(proc)
	var page http.Handler
	if *cfg.DemoClient {
		page = taskblaster.Handler()
	}
	srv := server.New(server.Config{
		BaseURL:   cfg.BaseURL,
		Directory: cfg.Directory,
		Accounts:  reg,
		Processor: proc,
		Broker:    broker,
		Page:      page,
		Logger:    logger,
	})

	reject := func(listener string) func(*http.Request, string) {
		return func(r *http.Request, why string) {
			logger.Warn("refused request", "via", listener, "from", r.RemoteAddr, "why", why)
		}
	}

	var servers []*http.Server
	errs := make(chan error, 2)
	serve := func(name string, ln net.Listener, h http.Handler) {
		hs := &http.Server{Handler: logRequests(logger, name, h)}
		servers = append(servers, hs)
		go func() {
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("%s listener: %w", name, err)
			}
		}()
	}

	if cfg.Listen != "" {
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		if u := cfg.Directory.LoopbackUser(); u != nil {
			logger.Info("listening", "addr", ln.Addr(), "user", u.ID)
		} else {
			logger.Warn("listening, but no user holds the loopback credential, so only requests with a bearer token will be served", "addr", ln.Addr())
		}
		serve("plain", ln, auth.Bearer(cfg.Directory, reject("plain"), auth.Loopback(cfg.Directory, srv)))
	}

	if ts := cfg.Tailscale; ts != nil {
		node, err := tailnet.Start(ctx, ts.Hostname, ts.Dir, logger)
		if err != nil {
			return fmt.Errorf("joining tailnet: %w", err)
		}
		defer node.Close()
		ln, err := node.Listen(*ts.HTTPS)
		if err != nil {
			return fmt.Errorf("tailnet listener: %w", err)
		}
		logger.Info("listening on tailnet", "url", node.URL(*ts.HTTPS))
		serve("tailnet", ln, auth.Bearer(cfg.Directory, reject("tailnet"), auth.Tailscale(cfg.Directory, node.WhoIs, reject("tailnet"), srv)))
	}

	select {
	case <-ctx.Done():
	case err := <-errs:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, hs := range servers {
		hs.Shutdown(shutdownCtx)
	}
	return nil
}

func logRequests(logger *slog.Logger, listener string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "via", listener, "method", r.Method, "path", r.URL.Path, "took", time.Since(start).Round(time.Millisecond))
	})
}
