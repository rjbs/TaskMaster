// Package server exposes the JMAP service over HTTP: the Session resource,
// the API endpoint, an event source for push, and placeholder blob
// endpoints that the Session object is obliged to advertise.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/tasks"
)

const maxSizeRequest = 10_000_000

type Config struct {
	AccountID string

	// BaseURL, if set, is used to build the absolute URLs in the Session
	// object.  Otherwise they are derived from each request.
	BaseURL string

	Tasks     *tasks.Service
	Processor *jmap.Processor
	Broker    *Broker
	Logger    *slog.Logger
}

type Server struct {
	cfg Config
	mux *http.ServeMux
}

func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	cfg.Processor.SessionState = s.sessionState
	s.mux.HandleFunc("GET /.well-known/jmap", s.handleSession)
	s.mux.HandleFunc("GET /jmap/session", s.handleSession)
	s.mux.HandleFunc("POST /jmap", s.handleAPI)
	s.mux.HandleFunc("GET /jmap/eventsource", s.handleEventSource)
	s.mux.HandleFunc("GET /jmap/download/{accountId}/{blobId}/{name}", s.handleNoBlobs)
	s.mux.HandleFunc("POST /jmap/upload/{accountId}/", s.handleNoBlobs)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimSuffix(s.cfg.BaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// session builds the Session object (RFC 8620 §2).
func (s *Server) session(r *http.Request) map[string]any {
	base := s.baseURL(r)
	acct := s.cfg.AccountID
	sess := map[string]any{
		"capabilities": map[string]any{
			jmap.CoreCapability: map[string]any{
				"maxSizeUpload":         0,
				"maxConcurrentUpload":   1,
				"maxSizeRequest":        maxSizeRequest,
				"maxConcurrentRequests": 4,
				"maxCallsInRequest":     s.cfg.Processor.MaxCalls,
				"maxObjectsInGet":       1000,
				"maxObjectsInSet":       1000,
				"collationAlgorithms":   []string{"i;ascii-casemap", "i;unicode-casemap"},
			},
			tasks.Capability: map[string]any{},
		},
		"accounts": map[string]any{
			acct: map[string]any{
				"name":       acct,
				"isPersonal": true,
				"isReadOnly": false,
				"accountCapabilities": map[string]any{
					tasks.Capability: s.cfg.Tasks.AccountCapabilities(),
				},
			},
		},
		"primaryAccounts": map[string]any{
			tasks.Capability: acct,
		},
		"username":       acct,
		"apiUrl":         base + "/jmap",
		"downloadUrl":    base + "/jmap/download/{accountId}/{blobId}/{name}?accept={type}",
		"uploadUrl":      base + "/jmap/upload/{accountId}/",
		"eventSourceUrl": base + "/jmap/eventsource?types={types}&closeafter={closeafter}&ping={ping}",
	}
	sess["state"] = s.sessionState()
	return sess
}

// sessionState only has to change when the session changes.  Apart from
// the URLs, which vary per request but not per session, the only variable
// content is the account capabilities, so a digest of those is an honest
// state string.
func (s *Server) sessionState() string {
	digest, _ := json.Marshal([]any{s.cfg.AccountID, s.cfg.Tasks.AccountCapabilities()})
	sum := sha256.Sum256(digest)
	return hex.EncodeToString(sum[:8])
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.session(r))
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSizeRequest))
	if err != nil {
		s.requestError(w, &jmap.RequestError{Type: jmap.ReqErrLimit, Status: 400, Limit: "maxSizeRequest", Detail: err.Error()})
		return
	}
	var req jmap.Request
	if err := json.Unmarshal(body, &req); err != nil {
		typ := jmap.ReqErrNotRequest
		if !json.Valid(body) {
			typ = jmap.ReqErrNotJSON
		}
		s.requestError(w, &jmap.RequestError{Type: typ, Status: 400, Detail: err.Error()})
		return
	}
	if req.MethodCalls == nil {
		s.requestError(w, &jmap.RequestError{Type: jmap.ReqErrNotRequest, Status: 400, Detail: "methodCalls is required"})
		return
	}

	resp, rerr := s.cfg.Processor.Process(r.Context(), &req)
	if rerr != nil {
		s.requestError(w, rerr)
		return
	}
	for _, inv := range resp.MethodResponses {
		if inv.Name == "error" {
			s.cfg.Logger.Info("method error", "callId", inv.CallID, "error", inv.Args)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) requestError(w http.ResponseWriter, e *jmap.RequestError) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(e)
}

func (s *Server) handleNoBlobs(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "this server does not store blobs", http.StatusNotFound)
}

// handleEventSource implements the push event source (RFC 8620 §7.3).
func (s *Server) handleEventSource(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	wanted := map[string]bool{}
	for _, t := range strings.Split(q.Get("types"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			wanted[t] = true
		}
	}
	all := len(wanted) == 0 || wanted["*"]
	closeAfterState := q.Get("closeafter") == "state"
	pingSeconds, _ := strconv.Atoi(q.Get("ping"))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := s.cfg.Broker.subscribe()
	defer s.cfg.Broker.unsubscribe(sub)

	var ping <-chan time.Time
	if pingSeconds > 0 {
		ticker := time.NewTicker(time.Duration(pingSeconds) * time.Second)
		defer ticker.Stop()
		ping = ticker.C
	}

	eventID := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping:
			fmt.Fprintf(w, "event: ping\ndata: {\"interval\": %d}\n\n", pingSeconds)
			flusher.Flush()
		case <-sub.notify:
			changed := sub.take()
			if !all {
				for typ := range changed {
					if !wanted[typ] {
						delete(changed, typ)
					}
				}
			}
			if len(changed) == 0 {
				continue
			}
			eventID++
			data, _ := json.Marshal(map[string]any{
				"@type":   "StateChange",
				"changed": map[string]any{s.cfg.AccountID: changed},
			})
			fmt.Fprintf(w, "event: state\nid: %d\ndata: %s\n\n", eventID, data)
			flusher.Flush()
			if closeAfterState {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}
