package models

import (
	"time"

	"github.com/shopspring/decimal"
)

// Допустимые значения поля Transaction.Type (соответствуют enum transaction_type в БД)
const (
	TypeDeposit  = "deposit"
	TypeWithdraw = "withdraw"
)

type User struct {
	ID      int             `json:"id"`
	Balance decimal.Decimal `json:"balance"`
}

type Transaction struct {
	ID        int             `json:"id"`
	UserID    int             `json:"user_id"`
	Amount    decimal.Decimal `json:"amount"`
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Processed bool            `json:"processed,omitempty"`
}
