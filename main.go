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

	"github.com/naumenis-code/UTM-Monotoring/internal/auth"
	"github.com/naumenis-code/UTM-Monotoring/internal/config"
	"github.com/naumenis-code/UTM-Monotoring/internal/db"
	"github.com/naumenis-code/UTM-Monotoring/internal/scheduler"
	"github.com/naumenis-code/UTM-Monotoring/internal/store"
	"github.com/naumenis-code/UTM-Monotoring/internal/utmclient"
	"github.com/naumenis-code/UTM-Monotoring/internal/web"
)

func main() {
	cfg := config.Load()

	if cfg.AdminPassword == "" {
		log.Print("ВНИМАНИЕ: переменная ADMIN_PASSWORD не задана — управление УТМ и настройки открыты без пароля")
	}

	sqlDB, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()

	st := store.New(sqlDB)
	client := utmclient.New(cfg.PollHTTPTimeout)
	sched := scheduler.New(st, client)
	authMgr := auth.New(cfg.AdminPassword)
	server := web.New(st, sched, authMgr)

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
