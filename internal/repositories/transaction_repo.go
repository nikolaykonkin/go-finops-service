package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nikolaykonkin/go-finops-service/internal/models"
)

// transactionColumns — порядок столбцов должен совпадать с scanTransaction
const transactionColumns = `id, user_id, amount, type, "timestamp", processed`

type TransactionRepo struct {
	pool *pgxpool.Pool
}

func NewTransactionRepo(pool *pgxpool.Pool) *TransactionRepo {
	return &TransactionRepo{pool: pool}
}

// scanTransaction читает одну запись; pgx.ErrNoRows превращается в ErrTransactionNotFound
func scanTransaction(row pgx.Row) (*models.Transaction, error) {
	var t models.Transaction

	err := row.Scan(&t.ID, &t.UserID, &t.Amount, &t.Type, &t.Timestamp, &t.Processed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTransactionNotFound
	}
	if err != nil {
		return nil, err
	}

	return &t, nil
}

// CreateTransaction вставляет новую транзакцию в рамках dbTx и возвращает ее ID
// Поля timestamp и processed заполняются значениями по умолчанию из схемы
func (r *TransactionRepo) CreateTransaction(ctx context.Context, tx *models.Transaction, dbTx pgx.Tx) (int, error) {
	var id int

	err := dbTx.QueryRow(ctx,
		`INSERT INTO transactions (user_id, amount, type)
		 VALUES ($1, $2, $3::transaction_type)
		 RETURNING id`,
		tx.UserID, tx.Amount, tx.Type,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("создание транзакции: %w", err)
	}

	return id, nil
}

// GetTransaction возвращает транзакцию по ID или ErrTransactionNotFound
func (r *TransactionRepo) GetTransaction(ctx context.Context, id int) (*models.Transaction, error) {
	t, err := scanTransaction(r.pool.QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM transactions WHERE id = $1`,
		id,
	))
	if err != nil {
		if errors.Is(err, ErrTransactionNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("получение транзакции %d: %w", id, err)
	}

	return t, nil
}

// UpdateTransaction обновляет user_id, amount и type необработанной транзакции
//
// Условие processed = FALSE проверяется в самом UPDATE, поэтому гонка с процессором невозможна:
// строка, которую процессор уже заблокировал и обработал, не будет изменена
func (r *TransactionRepo) UpdateTransaction(ctx context.Context, id int, newTx *models.Transaction) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE transactions
		    SET user_id = $1, amount = $2, type = $3::transaction_type
		  WHERE id = $4
		    AND COALESCE(processed, FALSE) = FALSE`,
		newTx.UserID, newTx.Amount, newTx.Type, id,
	)
	if err != nil {
		return fmt.Errorf("обновление транзакции %d: %w", id, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	// Ни одной строки не обновлено: записи нет либо она уже обработана
	var exists bool
	err = r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM transactions WHERE id = $1)`,
		id,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("проверка существования транзакции %d: %w", id, err)
	}
	if !exists {
		return ErrTransactionNotFound
	}

	return ErrTransactionProcessed
}

// DeleteTransaction удаляет транзакцию по ID
// Баланс пользователя не затрагивается: для удаления с реверсом баланса
// сервис использует DeleteTransactionTx
func (r *TransactionRepo) DeleteTransaction(ctx context.Context, id int) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM transactions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("удаление транзакции %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTransactionNotFound
	}

	return nil
}

// DeleteTransactionTx удаляет транзакцию в рамках dbTx и возвращает удаленную запись
// Если строка в этот момент обрабатывается, DELETE дожидается
// снятия блокировки и вернет актуальное значение processed
func (r *TransactionRepo) DeleteTransactionTx(ctx context.Context, id int, dbTx pgx.Tx) (*models.Transaction, error) {
	t, err := scanTransaction(dbTx.QueryRow(ctx,
		`DELETE FROM transactions WHERE id = $1 RETURNING `+transactionColumns,
		id,
	))
	if err != nil {
		if errors.Is(err, ErrTransactionNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("удаление транзакции %d: %w", id, err)
	}

	return t, nil
}
