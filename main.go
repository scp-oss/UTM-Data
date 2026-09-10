// Command utm-dashboard runs the web UI, background poller and Telegram
// notifier for monitoring УТМ (ЕГАИС transport module) instances.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/scp-oss/utm-data/internal/config"
	"github.com/scp-oss/utm-data/internal/db"
	"github.com/scp-oss/utm-data/internal/scheduler"
	"github.com/scp-oss/utm-data/internal/store"
	"github.com/scp-oss/utm-data/internal/utmclient"
	"github.com/scp-oss/utm-data/internal/web"
)

func main() {
	cfg := config.Load()

	sqlDB, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()

	st := store.New(sqlDB)
	client := utmclient.New(cfg.PollHTTPTimeout)
	sched := scheduler.New(st, client)
	server := web.New(st, sched)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go sched.Run(ctx)

	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: server}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("UTM Дашборд слушает на %s (БД: %s)", cfg.HTTPAddr, cfg.DBPath)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
}
