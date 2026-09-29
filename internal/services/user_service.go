package services

import (
	"context"

	"github.com/nikolaykonkin/go-finops-service/internal/repositories"
	"github.com/shopspring/decimal"
)

type UserService struct {
	userRepo repositories.UserRepository
}

func NewUserService(userRepo repositories.UserRepository) *UserService {
	return &UserService{userRepo: userRepo}
}

// GetBalance возвращает баланс пользователя
// Для несуществующего пользователя вернется repositories.ErrUserNotFound
func (s *UserService) GetBalance(ctx context.Context, userID int) (decimal.Decimal, error) {
	if userID <= 0 {
		return decimal.Zero, ErrInvalidUserID
	}

	return s.userRepo.GetBalance(ctx, userID)
}
