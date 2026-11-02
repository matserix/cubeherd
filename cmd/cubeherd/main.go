// Command cubeherd runs the lightweight dashboard for itzg/minecraft-server.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in minimal images without zoneinfo

	"cubeherd/internal/agent"
	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/i18n"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
	"cubeherd/internal/sources"
	"cubeherd/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("cubeherd: ")

	defData := "./cubeherd-data"
	if os.Geteuid() == 0 {
		defData = "/srv/cubeherd"
	}
	dataDir, err := filepath.Abs(env("CUBEHERD_DATA", defData))
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		runAgent(dataDir)
		return
	}
	listen := env("CUBEHERD_LISTEN", ":8420")

	cfg, err := config.Load(dataDir)
	if err != nil {
		log.Fatalf("loading settings: %v", err)
	}
	store, err := server.NewStore(dataDir)
	if err != nil {
		log.Fatalf("data directory: %v", err)
	}
	dc, err := docker.New()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := dc.Ping(ctx); err != nil {
		log.Printf("warning: Docker not reachable via %s: %v", dc.Host, err)
	}
	cancel()

	src := sources.New(func() string { return cfg.Get().CFAPIKey })
	m := manager.New(dc, store, cfg, src)
	m.Hoster = &host.Reader{Docker: dc, DataDir: dataDir}
	m.Lang = func() string {
		if l := cfg.Get().Language; i18n.Valid(l) {
			return l
		}
		return i18n.Source
	}
	schedCtx, stopSched := context.WithCancel(context.Background())
	defer stopSched()
	go m.RunScheduler(schedCtx)
	app, err := web.New(m, src, cfg, dataDir, listen)
	if err != nil {
		log.Fatalf("Templates: %v", err)
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    128 << 10,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		log.Printf("dashboard on %s · data in %s · Docker via %s", listen, dataDir, dc.Host)
		if cfg.Get().PasswordHash == "" {
			log.Printf("first start: set the password in the browser at /setup")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// runAgent serves the node API of this host to a dashboard: Docker access, server folders, backups
// and the scheduler stay here, the web UI does not. Clients must present the bearer token and pin the
// certificate fingerprint printed at startup.
func runAgent(dataDir string) {
	listen := env("CUBEHERD_AGENT_LISTEN", ":8421")
	cfg, err := config.Load(dataDir)
	if err != nil {
		log.Fatalf("loading settings: %v", err)
	}
	store, err := server.NewStore(dataDir)
	if err != nil {
		log.Fatalf("data directory: %v", err)
	}
	dc, err := docker.New()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := dc.Ping(ctx); err != nil {
		log.Printf("warning: Docker not reachable via %s: %v", dc.Host, err)
	}
	cancel()

	m := manager.New(dc, store, cfg, sources.New(func() string { return cfg.Get().CFAPIKey }))
	m.Hoster = &host.Reader{Docker: dc, DataDir: dataDir}
	m.Lang = func() string {
		if l := cfg.Get().Language; i18n.Valid(l) {
			return l
		}
		return i18n.Source
	}
	schedCtx, stopSched := context.WithCancel(context.Background())
	defer stopSched()
	go m.RunScheduler(schedCtx)

	stateDir := filepath.Join(dataDir, "agent")
	cert, err := agent.LoadOrCreateCert(stateDir)
	if err != nil {
		log.Fatalf("certificate: %v", err)
	}
	tok, generated, err := agent.LoadOrCreateToken(stateDir, os.Getenv("CUBEHERD_AGENT_TOKEN"))
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	srv := agent.New(m, tok, web.Version, dataDir).Server(listen, cert)
	log.Printf("agent on %s · data in %s · Docker via %s", listen, dataDir, dc.Host)
	log.Printf("certificate fingerprint (pin this in the dashboard): %s", api.Fingerprint(cert.Certificate[0]))
	if generated != "" {
		log.Printf("generated token (shown once, only its hash is stored): %s", generated)
	}
	go func() {
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
