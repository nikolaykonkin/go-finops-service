package tests

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nikolaykonkin/go-finops-service/internal/api"
	"github.com/nikolaykonkin/go-finops-service/internal/middleware"
	"github.com/nikolaykonkin/go-finops-service/internal/models"
	"github.com/nikolaykonkin/go-finops-service/internal/services"
	"github.com/shopspring/decimal"
)

// Тесты этого файла не требуют БД: проверяются сериализация, валидация и
// HTTP-контракт для запросов, которые отклоняются до обращения к хранилищу
// Сценарии с БД находятся в integration_test.go

// newValidationHandler собирает маршруты с пустыми зависимостями
// Любое обращение к БД приведет к panic, которую RecoveryMiddleware превратит в 500,
// поэтому тест завершится ошибкой, если запрос дошел до хранилища
func newValidationHandler() http.Handler {
	mux := http.NewServeMux()
	api.RegisterRoutes(mux, services.NewTransactionService(nil, nil, nil), services.NewUserService(nil), nil)
	return middleware.RecoveryMiddleware(mux)
}

func TestTransactionJSON(t *testing.T) {
	ts := time.Date(2026, 1, 12, 10, 30, 0, 0, time.UTC)

	t.Run("сериализация необработанной транзакции", func(t *testing.T) {
		in := models.Transaction{
			ID:        7,
			UserID:    3,
			Amount:    decimal.RequireFromString("100.50"),
			Type:      models.TypeDeposit,
			Timestamp: ts,
		}

		var raw map[string]any
		decodeMarshaled(t, in, &raw)

		if raw["id"] != float64(7) || raw["user_id"] != float64(3) {
			t.Fatalf("неверные id/user_id: %v", raw)
		}
		if raw["type"] != "deposit" {
			t.Fatalf("type = %v, ожидалось deposit", raw["type"])
		}
		amount, ok := raw["amount"].(string)
		if !ok {
			t.Fatalf("amount должен сериализоваться строкой, получено %T", raw["amount"])
		}
		if !decimal.RequireFromString(amount).Equal(in.Amount) {
			t.Fatalf("amount = %s, ожидалось %s", amount, in.Amount)
		}
		if raw["timestamp"] != "2026-01-12T10:30:00Z" {
			t.Fatalf("timestamp = %v", raw["timestamp"])
		}
		// Поведение модели (omitempty): processed = false в ответе отсутствует
		if _, present := raw["processed"]; present {
			t.Fatalf("processed = false не должен попадать в JSON: %v", raw)
		}
	})

	t.Run("обработанная транзакция содержит processed", func(t *testing.T) {
		in := models.Transaction{ID: 1, UserID: 1, Amount: decimal.NewFromInt(5), Type: models.TypeWithdraw, Processed: true}

		var raw map[string]any
		decodeMarshaled(t, in, &raw)

		if raw["processed"] != true {
			t.Fatalf("processed = %v, ожидалось true", raw["processed"])
		}
	})

	t.Run("десериализация принимает сумму строкой и числом", func(t *testing.T) {
		for _, body := range []string{
			`{"id":1,"user_id":2,"amount":"12.34","type":"deposit","processed":true}`,
			`{"id":1,"user_id":2,"amount":12.34,"type":"deposit","processed":true}`,
		} {
			var tx models.Transaction
			if err := json.Unmarshal([]byte(body), &tx); err != nil {
				t.Fatalf("разбор %s: %v", body, err)
			}
			if !tx.Amount.Equal(decimal.RequireFromString("12.34")) || tx.UserID != 2 || !tx.Processed {
				t.Fatalf("неверный результат разбора %s: %+v", body, tx)
			}
		}
	})
}

func TestUserJSON(t *testing.T) {
	in := models.User{ID: 5, Balance: decimal.RequireFromString("250.75")}

	var raw map[string]any
	decodeMarshaled(t, in, &raw)

	if raw["id"] != float64(5) {
		t.Fatalf("id = %v, ожидалось 5", raw["id"])
	}
	balance, ok := raw["balance"].(string)
	if !ok || !decimal.RequireFromString(balance).Equal(in.Balance) {
		t.Fatalf("balance = %v, ожидалось %s", raw["balance"], in.Balance)
	}

	var out models.User
	decodeMarshaled(t, in, &out)
	if out.ID != in.ID || !out.Balance.Equal(in.Balance) {
		t.Fatalf("после круговой сериализации получено %+v, ожидалось %+v", out, in)
	}
}

func TestInvalidTransactionType(t *testing.T) {
	cases := []struct {
		typ     string
		wantErr error
	}{
		{models.TypeDeposit, nil},
		{models.TypeWithdraw, nil},
		{"", services.ErrInvalidType},
		{"transfer", services.ErrInvalidType},
		{"DEPOSIT", services.ErrInvalidType},
		{" deposit", services.ErrInvalidType},
	}

	for _, tc := range cases {
		t.Run("type="+tc.typ, func(t *testing.T) {
			err := services.ValidateTransaction(&models.Transaction{
				UserID: 1,
				Amount: decimal.NewFromInt(10),
				Type:   tc.typ,
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("получена ошибка %v, ожидалась %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAmount(t *testing.T) {
	cases := []struct {
		name    string
		amount  string
		wantErr error
	}{
		{"минимальная сумма", "0.01", nil},
		{"целое число", "100", nil},
		{"две цифры после запятой", "99.99", nil},
		{"максимум NUMERIC(10,2)", "99999999.99", nil},
		{"ноль", "0", services.ErrInvalidAmount},
		{"отрицательная", "-1", services.ErrInvalidAmount},
		{"три знака после запятой", "100.001", services.ErrInvalidAmount},
		{"больше максимума", "100000000", services.ErrInvalidAmount},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := services.ValidateTransaction(&models.Transaction{
				UserID: 1,
				Amount: decimal.RequireFromString(tc.amount),
				Type:   models.TypeDeposit,
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("amount=%s: получена ошибка %v, ожидалась %v", tc.amount, err, tc.wantErr)
			}
		})
	}

	t.Run("сумма не задана", func(t *testing.T) {
		err := services.ValidateTransaction(&models.Transaction{UserID: 1, Type: models.TypeDeposit})
		if !errors.Is(err, services.ErrInvalidAmount) {
			t.Fatalf("получена ошибка %v, ожидалась ErrInvalidAmount", err)
		}
	})

	t.Run("некорректный user_id", func(t *testing.T) {
		for _, id := range []int{0, -1} {
			err := services.ValidateTransaction(&models.Transaction{
				UserID: id,
				Amount: decimal.NewFromInt(1),
				Type:   models.TypeDeposit,
			})
			if !errors.Is(err, services.ErrInvalidUserID) {
				t.Fatalf("user_id=%d: получена ошибка %v, ожидалась ErrInvalidUserID", id, err)
			}
		}
	})
}

func TestCreateTransactionRequest(t *testing.T) {
	h := newValidationHandler()

	cases := []struct {
		name string
		body string
	}{
		{"пустое тело", ``},
		{"невалидный JSON", `{"user_id":`},
		{"неизвестное поле", `{"user_id":1,"amount":"10","type":"deposit","extra":1}`},
		{"данные после JSON", `{"user_id":1,"amount":"10","type":"deposit"} {}`},
		{"сумма не число", `{"user_id":1,"amount":"abc","type":"deposit"}`},
		{"нулевая сумма", `{"user_id":1,"amount":"0","type":"deposit"}`},
		{"отрицательная сумма", `{"user_id":1,"amount":"-5","type":"deposit"}`},
		{"сумма не задана", `{"user_id":1,"type":"deposit"}`},
		{"три знака после запятой", `{"user_id":1,"amount":"1.001","type":"deposit"}`},
		{"неизвестный тип", `{"user_id":1,"amount":"10","type":"transfer"}`},
		{"тип не задан", `{"user_id":1,"amount":"10"}`},
		{"user_id не задан", `{"amount":"10","type":"deposit"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serve(t, h, http.MethodPost, "/transactions", tc.body)
			assertErrorResponse(t, rr, http.StatusBadRequest)
		})
	}

	t.Run("метод не поддерживается", func(t *testing.T) {
		rr := serve(t, h, http.MethodPatch, "/transactions", `{}`)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("код ответа %d, ожидался 405", rr.Code)
		}
	})
}

func TestGetTransactionRequest(t *testing.T) {
	h := newValidationHandler()

	for _, id := range []string{"abc", "0", "-5", "1.5"} {
		t.Run("id="+id, func(t *testing.T) {
			rr := serve(t, h, http.MethodGet, "/transactions/"+id, nil)
			assertErrorResponse(t, rr, http.StatusBadRequest)
		})
	}
}

func TestUpdateTransactionRequest(t *testing.T) {
	h := newValidationHandler()

	t.Run("некорректный id", func(t *testing.T) {
		rr := serve(t, h, http.MethodPut, "/transactions/abc", txBody(1, "10", models.TypeDeposit))
		assertErrorResponse(t, rr, http.StatusBadRequest)
	})

	t.Run("невалидный JSON", func(t *testing.T) {
		rr := serve(t, h, http.MethodPut, "/transactions/1", `{"amount":`)
		assertErrorResponse(t, rr, http.StatusBadRequest)
	})

	t.Run("некорректная сумма", func(t *testing.T) {
		rr := serve(t, h, http.MethodPut, "/transactions/1", txBody(1, "-10", models.TypeDeposit))
		assertErrorResponse(t, rr, http.StatusBadRequest)
	})

	t.Run("неизвестный тип", func(t *testing.T) {
		rr := serve(t, h, http.MethodPut, "/transactions/1", txBody(1, "10", "transfer"))
		assertErrorResponse(t, rr, http.StatusBadRequest)
	})
}

func TestDeleteTransactionRequest(t *testing.T) {
	h := newValidationHandler()

	for _, id := range []string{"abc", "0", "-1"} {
		t.Run("id="+id, func(t *testing.T) {
			rr := serve(t, h, http.MethodDelete, "/transactions/"+id, nil)
			assertErrorResponse(t, rr, http.StatusBadRequest)
		})
	}
}

func TestGetUserBalanceRequest(t *testing.T) {
	h := newValidationHandler()

	for _, id := range []string{"abc", "0", "-3"} {
		t.Run("user_id="+id, func(t *testing.T) {
			rr := serve(t, h, http.MethodGet, "/users/"+id+"/balance", nil)
			assertErrorResponse(t, rr, http.StatusBadRequest)
		})
	}
}

func TestHealthCheck(t *testing.T) {
	rr := serve(t, newValidationHandler(), http.MethodGet, "/health", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("код ответа %d, ожидался 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, ожидался application/json", ct)
	}

	var body map[string]string
	decodeBody(t, rr, &body)
	if body["status"] != "ok" {
		t.Fatalf("тело ответа %v, ожидалось status=ok", body)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("тестовая паника")
	})

	rr := httptest.NewRecorder()
	middleware.RecoveryMiddleware(panicking).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("код ответа %d, ожидался 500", rr.Code)
	}
}

// decodeMarshaled кодирует in в JSON и разбирает результат в out (проверка сериализации в обе стороны)
func decodeMarshaled(t *testing.T, in, out any) {
	t.Helper()

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("сериализация %+v: %v", in, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("разбор %s: %v", raw, err)
	}
}

// assertErrorResponse проверяет код ответа и наличие JSON-описания ошибки
func assertErrorResponse(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()

	if rr.Code != wantStatus {
		t.Fatalf("код ответа %d, ожидался %d; тело: %s", rr.Code, wantStatus, rr.Body.String())
	}

	var body struct {
		Error string `json:"error"`
	}
	decodeBody(t, rr, &body)
	if body.Error == "" {
		t.Fatalf("в ответе нет поля error: %s", rr.Body.String())
	}
}
