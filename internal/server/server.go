// Package server puts TaskMaster's HTTP service together: Gizmo's JMAP
// server over the accounts' task services, and, if wanted, the server's own
// web client at the root.
//
// Every request must already carry an authenticated user in its context,
// put there by one of the auth package's listeners; the Server itself
// refuses anything else.
package server

import (
	"log/slog"
	"net/http"

	"github.com/rjbs/gizmo/auth"
	"github.com/rjbs/gizmo/httpd"
	"github.com/rjbs/gizmo/jmap"
	"github.com/rjbs/gizmo/push"

	"github.com/rjbs/taskmaster/internal/tasks"
)

type Config struct {
	// BaseURL, if set, is used to build the absolute URLs in the Session
	// object.  Otherwise they are derived from each request.
	BaseURL string

	Directory *auth.Directory
	Accounts  *tasks.Registry
	Processor *jmap.Processor
	Broker    *push.Broker

	// Page, if set, is served at the root as the server's own web client.
	Page   http.Handler
	Logger *slog.Logger
}

type Server struct {
	mux *http.ServeMux
}

func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.Processor.Logger = cfg.Logger
	j := httpd.New(httpd.Config{
		BaseURL:   cfg.BaseURL,
		Processor: cfg.Processor,
		Directory: cfg.Directory,
		Account: func(id string) httpd.Account {
			// A nil *tasks.Service in the interface would not be a
			// nil interface, so the missing case is spelled out.
			if svc := cfg.Accounts.Get(id); svc != nil {
				return svc
			}
			return nil
		},
		Broker:       cfg.Broker,
		Limits:       jmap.Limits{MaxObjectsInGet: tasks.MaxObjectsInGet, MaxObjectsInSet: tasks.MaxObjectsInSet},
		Capabilities: map[string]any{tasks.Capability: map[string]any{}},
		Collations:   []string{"i;ascii-casemap", "i;unicode-casemap"},
		Logger:       cfg.Logger,
	})

	s := &Server{mux: http.NewServeMux()}
	s.mux.Handle("/", j)
	if cfg.Page != nil {
		s.mux.Handle("GET /{$}", cfg.Page)
		s.mux.Handle("GET /index.html", cfg.Page)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserFrom(r.Context()); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	s.mux.ServeHTTP(w, r)
}
