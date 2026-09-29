package repositories

import "errors"

// Ошибки слоя доступа к данным
// Сервисы и обработчики сопоставляют их через errors.Is и переводят в HTTP-статусы
var (
	ErrUserNotFound         = errors.New("user not found")
	ErrTransactionNotFound  = errors.New("transaction not found")
	ErrTransactionProcessed = errors.New("transaction is already processed")
	ErrInsufficientFunds    = errors.New("insufficient funds")
)
