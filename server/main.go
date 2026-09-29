package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nikolaykonkin/go-finops-service/internal/api"
	"github.com/nikolaykonkin/go-finops-service/internal/config"
	"github.com/nikolaykonkin/go-finops-service/internal/db"
	"github.com/nikolaykonkin/go-finops-service/internal/middleware"
	"github.com/nikolaykonkin/go-finops-service/internal/processor"
	"github.com/nikolaykonkin/go-finops-service/internal/repositories"
	"github.com/nikolaykonkin/go-finops-service/internal/services"
)

const (
	workerCount     = 5
	shutdownTimeout = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("finops-service: %v", err)
	}
}

// run вынесен из main, чтобы отложенные вызовы (закрытие процессора и пула)
// выполнялись и при завершении с ошибкой: log.Fatal их пропускает
func run() error {
	cfg := config.LoadConfig()

	pool, err := db.NewPool(cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("подключение к БД: %w", err)
	}
	defer pool.Close()

	userRepo := repositories.NewUserRepo(pool)
	txRepo := repositories.NewTransactionRepo(pool)

	userService := services.NewUserService(userRepo)
	txService := services.NewTransactionService(txRepo, userRepo, pool)

	// Defer'ы выполняются в обратном порядке: сначала Close процессора
	// (дожидается обработки очереди), затем закрытие пула
	proc := processor.NewProcessor(pool, workerCount)
	defer func() {
		if err := proc.Close(); err != nil {
			log.Printf("остановка процессора: %v", err)
		}
	}()

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, txService, userService, proc)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           middleware.LoggingMiddleware(middleware.RecoveryMiddleware(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("сервер запущен на %s", srv.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("HTTP-сервер: %w", err)
	case <-ctx.Done():
		log.Println("получен сигнал завершения, останавливаем сервер")
	}

	// Сначала перестаем принимать запросы (и, как следствие, Submit),
	// затем отложенный proc.Close() доработает очередь
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("остановка HTTP-сервера: %w", err)
	}

	log.Println("сервер остановлен")
	return nil
}
