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
	m.Lang = func() string {
		if l := cfg.Get().Language; i18n.Valid(l) {
			return l
		}
		return i18n.Source
	}
	schedCtx, stopSched := context.WithCancel(context.Background())
	defer stopSched()
	go m.RunScheduler(schedCtx)
	app, err := web.New(m, cfg, &host.Reader{Docker: dc, DataDir: dataDir}, dataDir, listen)
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
