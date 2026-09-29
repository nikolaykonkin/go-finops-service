package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Общее время ожидания готовности БД при старте и пауза между попытками
	connectTimeout = 30 * time.Second
	retryInterval  = time.Second

	// Ограничение на откат транзакции
	rollbackTimeout = 5 * time.Second

	// Размер пула с запасом под воркеры процессора и параллельные HTTP-запросы
	maxConns = 20
)

// NewPool создает пул соединений и проверяет доступность БД через Ping
//
// pgxpool.New не открывает соединение сразу, поэтому доступность проверяется явно
// Ping повторяется до connectTimeout: в docker-compose приложение может
// стартовать раньше, чем Postgres начнет принимать TCP-подключения
func NewPool(dsn string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("разбор DSN: %w", err)
	}
	cfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("создание пула соединений: %w", err)
	}

	for {
		pingErr := pool.Ping(ctx)
		if pingErr == nil {
			return pool, nil
		}

		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("БД недоступна: %w", pingErr)
		case <-time.After(retryInterval):
			log.Printf("db: БД пока недоступна, повторная попытка: %v", pingErr)
		}
	}
}

// Rollback откатывает БД-транзакцию, не завися от отмены исходного контекста
// Предназначен для defer: после успешного Commit pgx возвращает ErrTxClosed, он игнорируется
func Rollback(dbTx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()

	if err := dbTx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		log.Printf("db: откат транзакции: %v", err)
	}
}
