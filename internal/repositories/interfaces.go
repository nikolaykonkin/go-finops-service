package repositories

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/nikolaykonkin/go-finops-service/internal/models"
	"github.com/shopspring/decimal"
)

type UserRepository interface {
	GetBalance(ctx context.Context, userID int) (decimal.Decimal, error)
	// UpdateBalance прибавляет amount (со знаком) к балансу пользователя
	// внутри переданной БД-транзакции
	UpdateBalance(ctx context.Context, userID int, amount decimal.Decimal, tx pgx.Tx) error
}

type TransactionRepository interface {
	CreateTransaction(ctx context.Context, tx *models.Transaction, dbTx pgx.Tx) (int, error)
	GetTransaction(ctx context.Context, id int) (*models.Transaction, error)
	UpdateTransaction(ctx context.Context, id int, newTx *models.Transaction) error
	DeleteTransaction(ctx context.Context, id int) error
	// DeleteTransactionTx удаляет транзакцию внутри переданной БД-транзакции и возвращает удаленную запись
	// Нужен сервису, чтобы атомарно удалить запись и откатить ее влияние на баланс
	DeleteTransactionTx(ctx context.Context, id int, dbTx pgx.Tx) (*models.Transaction, error)
}
