package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"seqlabels/internal/api"
	"seqlabels/internal/labels"
	"seqlabels/internal/storage"

	_ "github.com/lib/pq"
)

func main() {
	addr := env("ADDR", ":8080")
	databaseURL := env("DATABASE_URL", "postgres://seqlabels:seqlabels@db:5432/seqlabels?sslmode=disable")

	db, err := openDatabase(databaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	store := storage.NewStore(db)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("migrate database: %v", err)
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           api.NewServer(store, labels.Compute).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
}

func openDatabase(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := db.PingContext(ctx); err == nil {
			return db, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("database did not become ready")
		case <-ticker.C:
		}
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
