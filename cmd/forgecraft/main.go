// Command forgecraft runs the lightweight dashboard for itzg/minecraft-server.
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

	"forgecraft/internal/config"
	"forgecraft/internal/docker"
	"forgecraft/internal/host"
	"forgecraft/internal/manager"
	"forgecraft/internal/server"
	"forgecraft/internal/sources"
	"forgecraft/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("forgecraft: ")

	defData := "./forgecraft-data"
	if os.Geteuid() == 0 {
		defData = "/srv/forgecraft"
	}
	dataDir, err := filepath.Abs(env("FORGECRAFT_DATA", defData))
	if err != nil {
		log.Fatal(err)
	}
	listen := env("FORGECRAFT_LISTEN", ":8420")

	cfg, err := config.Load(dataDir)
	if err != nil {
		log.Fatalf("Einstellungen laden: %v", err)
	}
	store, err := server.NewStore(dataDir)
	if err != nil {
		log.Fatalf("Datenverzeichnis: %v", err)
	}
	dc, err := docker.New()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := dc.Ping(ctx); err != nil {
		log.Printf("Warnung: Docker nicht erreichbar über %s: %v", dc.Host, err)
	}
	cancel()

	src := sources.New(func() string { return cfg.Get().CFAPIKey })
	m := manager.New(dc, store, cfg, src)
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
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		log.Printf("Dashboard auf %s · Daten in %s · Docker über %s", listen, dataDir, dc.Host)
		if cfg.Get().PasswordHash == "" {
			log.Printf("Erster Start: Passwort im Browser unter /setup festlegen")
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
