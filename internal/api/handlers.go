package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/nikolaykonkin/go-finops-service/internal/models"
	"github.com/nikolaykonkin/go-finops-service/internal/processor"
	"github.com/nikolaykonkin/go-finops-service/internal/repositories"
	"github.com/nikolaykonkin/go-finops-service/internal/services"
	"github.com/shopspring/decimal"
)

// maxBodyBytes ограничивает размер тела запроса
const maxBodyBytes = 1 << 20

type TransactionRequest struct {
	UserID int             `json:"user_id"`
	Amount decimal.Decimal `json:"amount"`
	Type   string          `json:"type"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// RegisterRoutes регистрирует все маршруты сервиса
// Используется и в main, и в тестах, чтобы маршрутизация была описана в одном месте
func RegisterRoutes(
	mux *http.ServeMux,
	txService *services.TransactionService,
	userService *services.UserService,
	p *processor.Processor,
) {
	mux.Handle("GET /health", HealthCheckHandler())
	mux.Handle("POST /transactions", CreateTransactionHandler(txService, p))
	mux.Handle("GET /transactions/{id}", GetTransactionHandler(txService))
	mux.Handle("PUT /transactions/{id}", UpdateTransactionHandler(txService, p))
	mux.Handle("DELETE /transactions/{id}", DeleteTransactionHandler(txService, p))
	mux.Handle("GET /users/{user_id}/balance", GetUserBalanceHandler(userService))
}

// CreateTransactionHandler: POST /transactions -> 201 {"id": N}
// После сохранения транзакция передается процессору на асинхронную обработку
func CreateTransactionHandler(txService *services.TransactionService, p *processor.Processor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req TransactionRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}

		tx := &models.Transaction{
			UserID: req.UserID,
			Amount: req.Amount,
			Type:   req.Type,
		}

		id, err := txService.CreateTransaction(r.Context(), tx)
		if err != nil {
			writeServiceError(w, err)
			return
		}

		p.Submit(models.Transaction{
			ID:     id,
			UserID: tx.UserID,
			Amount: tx.Amount,
			Type:   tx.Type,
		})

		writeJSON(w, http.StatusCreated, map[string]int{"id": id})
	}
}

// GetTransactionHandler: GET /transactions/{id} -> 200 или 404
func GetTransactionHandler(txService *services.TransactionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}

		tx, err := txService.GetTransaction(r.Context(), id)
		if err != nil {
			writeServiceError(w, err)
			return
		}

		writeJSON(w, http.StatusOK, tx)
	}
}

// UpdateTransactionHandler: PUT /transactions/{id} -> 204
// Процессор не используется: обработчик читает актуальные данные из БД,
// поэтому повторная постановка в очередь не требуется
func UpdateTransactionHandler(txService *services.TransactionService, _ *processor.Processor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}

		var req TransactionRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}

		newTx := &models.Transaction{
			UserID: req.UserID,
			Amount: req.Amount,
			Type:   req.Type,
		}
		if err := txService.UpdateTransaction(r.Context(), id, newTx); err != nil {
			writeServiceError(w, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// DeleteTransactionHandler: DELETE /transactions/{id} -> 204
// Реверс баланса для обработанных транзакций выполняет сервис
func DeleteTransactionHandler(txService *services.TransactionService, _ *processor.Processor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}

		if err := txService.DeleteTransaction(r.Context(), id); err != nil {
			writeServiceError(w, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// GetUserBalanceHandler: GET /users/{user_id}/balance -> 200 {"id": N, "balance": "..."}
func GetUserBalanceHandler(userService *services.UserService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := pathID(w, r, "user_id")
		if !ok {
			return
		}

		balance, err := userService.GetBalance(r.Context(), userID)
		if err != nil {
			writeServiceError(w, err)
			return
		}

		writeJSON(w, http.StatusOK, models.User{ID: userID, Balance: balance})
	}
}

// HealthCheckHandler: GET /health -> 200
func HealthCheckHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// pathID извлекает положительный целочисленный параметр пути
// При ошибке сам отвечает 400 и возвращает ok = false
func pathID(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	id, err := strconv.Atoi(r.PathValue(name))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, name+" must be a positive integer")
		return 0, false
	}

	return id, true
}

// decodeJSON строго разбирает тело запроса: ограничивает размер, запрещает
// неизвестные поля и данные после первого JSON-значения
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON body")
	}

	return nil
}

// writeServiceError переводит ошибки сервисного слоя в HTTP-статусы
// Внутренние ошибки логируются, клиенту их детали не раскрываются
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, services.ErrInvalidUserID),
		errors.Is(err, services.ErrInvalidAmount),
		errors.Is(err, services.ErrInvalidType):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, repositories.ErrUserNotFound),
		errors.Is(err, repositories.ErrTransactionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, repositories.ErrInsufficientFunds),
		errors.Is(err, repositories.ErrTransactionProcessed):
		writeError(w, http.StatusConflict, err.Error())
	default:
		log.Printf("api: внутренняя ошибка: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: ошибка записи ответа: %v", err)
	}
}
