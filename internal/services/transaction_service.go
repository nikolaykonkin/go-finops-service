package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nikolaykonkin/go-finops-service/internal/db"
	"github.com/nikolaykonkin/go-finops-service/internal/models"
	"github.com/nikolaykonkin/go-finops-service/internal/repositories"
	"github.com/shopspring/decimal"
)

// Ошибки валидации входных данных
var (
	ErrInvalidUserID = errors.New("user_id must be a positive integer")
	ErrInvalidAmount = errors.New("amount must be positive, have at most 2 decimal places and not exceed 99999999.99")
	ErrInvalidType   = errors.New("type must be either deposit or withdraw")
)

// maxAmount — верхняя граница суммы, определяемая типом NUMERIC(10,2)
var maxAmount = decimal.New(9999999999, -2)

// ValidateTransaction проверяет user_id, тип и сумму транзакции
// Не обращается к БД, поэтому пригодна для unit-тестов
func ValidateTransaction(tx *models.Transaction) error {
	if tx == nil || tx.UserID <= 0 {
		return ErrInvalidUserID
	}
	if tx.Type != models.TypeDeposit && tx.Type != models.TypeWithdraw {
		return ErrInvalidType
	}
	if !tx.Amount.IsPositive() || tx.Amount.GreaterThan(maxAmount) {
		return ErrInvalidAmount
	}
	// Не более двух знаков после запятой: иначе Postgres молча округлит сумму
	if !tx.Amount.Equal(tx.Amount.Round(2)) {
		return ErrInvalidAmount
	}

	return nil
}

type TransactionService struct {
	txRepo   repositories.TransactionRepository
	userRepo repositories.UserRepository
	pool     *pgxpool.Pool
}

func NewTransactionService(
	txRepo repositories.TransactionRepository,
	userRepo repositories.UserRepository,
	pool *pgxpool.Pool,
) *TransactionService {
	return &TransactionService{
		txRepo:   txRepo,
		userRepo: userRepo,
		pool:     pool,
	}
}

// CreateTransaction валидирует и сохраняет транзакцию со статусом processed = false и возвращает ее ID
//
// Баланс здесь намеренно не меняется - он изменяется ровно один раз, при асинхронной обработке транзакции
// процессором: если применять изменение в обоих местах, каждая операция будет учтена дважды
//
// Проверка существования пользователя и достаточности средств для withdraw выполняется до Begin
// и носит предварительный характер (баланс может измениться до обработки)
// Окончательная проверка делается процессором под блокировкой строки пользователя
// Читать баланс после Begin нельзя: репозиторий использует пул, и при полностью занятом пуле каждый запрос,
// удерживающий соединение под Begin, ждал бы второго соединения, то есть возникла бы взаимная блокировка
func (s *TransactionService) CreateTransaction(ctx context.Context, tx *models.Transaction) (int, error) {
	if err := ValidateTransaction(tx); err != nil {
		return 0, err
	}

	balance, err := s.userRepo.GetBalance(ctx, tx.UserID)
	if err != nil {
		return 0, err
	}
	if tx.Type == models.TypeWithdraw && balance.LessThan(tx.Amount) {
		return 0, repositories.ErrInsufficientFunds
	}

	dbTx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("начало транзакции БД: %w", err)
	}
	defer db.Rollback(dbTx)

	id, err := s.txRepo.CreateTransaction(ctx, tx, dbTx)
	if err != nil {
		return 0, err
	}

	if err := dbTx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("коммит транзакции БД: %w", err)
	}

	tx.ID = id
	return id, nil
}

// GetTransaction возвращает транзакцию по ID
func (s *TransactionService) GetTransaction(ctx context.Context, id int) (*models.Transaction, error) {
	return s.txRepo.GetTransaction(ctx, id)
}

// UpdateTransaction обновляет user_id, amount и type необработанной транзакции
// Для обработанной транзакции вернется repositories.ErrTransactionProcessed:
// проверка processed = false выполняется атомарно внутри UPDATE в репозитории
func (s *TransactionService) UpdateTransaction(ctx context.Context, id int, newTx *models.Transaction) error {
	if err := ValidateTransaction(newTx); err != nil {
		return err
	}
	if _, err := s.userRepo.GetBalance(ctx, newTx.UserID); err != nil {
		return err
	}

	return s.txRepo.UpdateTransaction(ctx, id, newTx)
}

// DeleteTransaction удаляет транзакцию
// Для обработанной транзакции в той же БД-транзакции откатывается ее влияние на баланс:
// deposit списывается, withdraw возвращается
// Если откат невозможен (например, зачисленные средства уже потрачены),
// удаление отменяется с ErrInsufficientFunds
func (s *TransactionService) DeleteTransaction(ctx context.Context, id int) error {
	dbTx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало транзакции БД: %w", err)
	}
	defer db.Rollback(dbTx)

	deleted, err := s.txRepo.DeleteTransactionTx(ctx, id, dbTx)
	if err != nil {
		return err
	}

	if deleted.Processed {
		delta := deleted.Amount
		if deleted.Type == models.TypeDeposit {
			delta = delta.Neg()
		}
		if err := s.userRepo.UpdateBalance(ctx, deleted.UserID, delta, dbTx); err != nil {
			return err
		}
	}

	if err := dbTx.Commit(ctx); err != nil {
		return fmt.Errorf("коммит транзакции БД: %w", err)
	}

	return nil
}
