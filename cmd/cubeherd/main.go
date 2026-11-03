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
	"strings"
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
	"cubeherd/internal/node"
	"cubeherd/internal/nodes"
	"cubeherd/internal/server"
	"cubeherd/internal/sources"
	"cubeherd/internal/web"
)

// envBool reads a switch such as CUBEHERD_LOCAL; unset or unrecognised keeps def.
func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "agent":
			runAgent(dataDir)
			return
		case "user", "db":
			if err := runCLI(dataDir, os.Args[1:]); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	listen := env("CUBEHERD_LISTEN", ":8420")

	cfg, err := config.Load(dataDir)
	if err != nil {
		log.Fatalf("loading settings: %v", err)
	}
	if envBool("CUBEHERD_ACCOUNTS", false) && !cfg.Get().Accounts {
		if err := cfg.Update(func(s *config.Settings) { s.Accounts = true }); err != nil {
			log.Fatalf("saving settings: %v", err)
		}
	}
	src := sources.New(func() string { return cfg.Get().CFAPIKey })

	// The embedded local node manages the Docker host this process runs on. CUBEHERD_LOCAL=false turns it off
	// for a dashboard that only manages remote agents and needs no Docker socket.
	var local node.Node
	dockerHost := "none (local node off)"
	if envBool("CUBEHERD_LOCAL", true) {
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
		local, dockerHost = m, dc.Host
	}
	reg := nodes.New(cfg, local, web.Version)
	go reg.Refresh(context.Background(), false, 0) // hands the settings to the agents right away
	app, err := web.New(reg, src, cfg, dataDir, listen)
	if err != nil {
		log.Fatalf("Templates: %v", err)
	}

	if envBool("CUBEHERD_AUDIT_STDOUT", false) {
		app.SetAuditSink(func(b []byte) { _, _ = os.Stdout.Write(append(b, '\n')) })
	}
	go app.RunMaintenance(context.Background())
	srv := &http.Server{
		Addr:              listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    128 << 10,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		log.Printf("dashboard on %s · data in %s · Docker via %s · %d remote node(s)", listen, dataDir, dockerHost, len(cfg.Get().Nodes))
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
