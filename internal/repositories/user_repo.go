package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type UserRepo struct {
	pool *pgxpool.Pool
}

func NewUserRepo(pool *pgxpool.Pool) *UserRepo {
	return &UserRepo{pool: pool}
}

// GetBalance возвращает баланс пользователя или ErrUserNotFound
func (r *UserRepo) GetBalance(ctx context.Context, userID int) (decimal.Decimal, error) {
	var balance decimal.Decimal

	err := r.pool.QueryRow(ctx,
		`SELECT COALESCE(balance, 0) FROM users WHERE id = $1`,
		userID,
	).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, ErrUserNotFound
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("получение баланса пользователя %d: %w", userID, err)
	}

	return balance, nil
}

// UpdateBalance прибавляет amount (положительное или отрицательное) к балансу
// пользователя в рамках переданной БД-транзакции tx
//
// Условие balance + amount >= 0 проверяется в самом UPDATE, поэтому баланс
// не может уйти в минус даже при параллельных изменениях
// Если строка не обновлена, возвращается ErrUserNotFound либо ErrInsufficientFunds
func (r *UserRepo) UpdateBalance(ctx context.Context, userID int, amount decimal.Decimal, tx pgx.Tx) error {
	tag, err := tx.Exec(ctx,
		`UPDATE users
		    SET balance = COALESCE(balance, 0) + $1
		  WHERE id = $2
		    AND COALESCE(balance, 0) + $1 >= 0`,
		amount, userID,
	)
	if err != nil {
		return fmt.Errorf("обновление баланса пользователя %d: %w", userID, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	var exists bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`,
		userID,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("проверка существования пользователя %d: %w", userID, err)
	}
	if !exists {
		return ErrUserNotFound
	}

	return ErrInsufficientFunds
}
