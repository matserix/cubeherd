// Package agent serves the node API of one host to a dashboard over HTTPS.
package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/auth"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
)

// Backend is the part of a node the agent exposes so far.
type Backend interface {
	Views(ctx context.Context, withStats bool) ([]api.View, error)
	Start(id string) error
	Stop(id string) error
	Restart(id string) error
	HostStats(ctx context.Context) host.Stats
	DockerInfo(ctx context.Context) (docker.Info, error)
	DockerHost() string
}

var _ Backend = (*manager.Manager)(nil)

// Agent is the HTTP handler for one node.
type Agent struct {
	B       Backend
	Token   Token
	Version string
	DataDir string

	lim *auth.Limiter
}

func New(b Backend, tok Token, version, dataDir string) *Agent {
	return &Agent{B: b, Token: tok, Version: version, DataDir: dataDir, lim: auth.NewLimiter()}
}

// Handler routes /v1 requests behind the token check.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", a.info)
	mux.HandleFunc("GET /v1/host", a.host)
	mux.HandleFunc("GET /v1/servers", a.servers)
	mux.HandleFunc("POST /v1/servers/{id}/{action}", a.lifecycle)
	return a.guard(mux)
}

// Server returns an HTTPS server for the handler using the agent certificate.
func (a *Agent) Server(addr string, cert tls.Certificate) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
		IdleTimeout:       90 * time.Second,
	}
}

// guard checks the API major and the bearer token; repeated failures per client are slowed down.
func (a *Agent) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := a.lim.Wait(r); d > 0 {
			w.Header().Set("Retry-After", itoa(int(d.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "", "too many failed attempts")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !a.Token.Check(tok) {
			a.lim.Fail(r)
			writeErr(w, http.StatusUnauthorized, "", "invalid token")
			return
		}
		a.lim.Reset(r)
		if v := r.Header.Get(api.Header); v != itoa(api.Version) {
			writeErr(w, http.StatusConflict, "apiversion", "agent speaks API "+itoa(api.Version)+", caller sent "+quote(v))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Agent) info(w http.ResponseWriter, r *http.Request) {
	hn, _ := os.Hostname()
	in := api.Info{Version: a.Version, API: api.Version, Hostname: hn, DockerHost: a.B.DockerHost(), DataDir: a.DataDir}
	if _, err := a.B.DockerInfo(r.Context()); err != nil {
		in.DockerErr = err.Error()
	} else {
		in.DockerOK = true
	}
	writeJSON(w, http.StatusOK, in)
}

func (a *Agent) host(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.B.HostStats(r.Context()))
}

func (a *Agent) servers(w http.ResponseWriter, r *http.Request) {
	views, err := a.B.Views(r.Context(), r.URL.Query().Get("stats") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	if views == nil {
		views = []api.View{}
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *Agent) lifecycle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var fn func(string) error
	switch r.PathValue("action") {
	case "start":
		fn = a.B.Start
	case "stop":
		fn = a.B.Stop
	case "restart":
		fn = a.B.Restart
	default:
		writeErr(w, http.StatusNotFound, "", "unknown action")
		return
	}
	switch err := fn(id); {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, manager.ErrBusy):
		writeErr(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, server.ErrNotFound):
		writeErr(w, http.StatusNotFound, "notfound", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, api.Error{Error: msg, Code: kind})
}
