package tests

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/nikolaykonkin/go-finops-service/internal/models"
	"github.com/shopspring/decimal"
)

// Интеграционные тесты работают с реальным Postgres (docker-compose up -d db)
// Если БД недоступна, тесты пропускаются
// Каждый тест создает собственных пользователей и удаляет их по завершении, поэтому тесты независимы

const waitTimeout = 10 * time.Second

func txPath(id int) string {
	return "/transactions/" + strconv.Itoa(id)
}

// countTransactions возвращает число транзакций пользователя в БД
func countTransactions(t *testing.T, env *testEnv, userID int) int {
	t.Helper()

	var n int
	err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM transactions WHERE user_id = $1`, userID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("подсчёт транзакций пользователя %d: %v", userID, err)
	}

	return n
}

func TestCreateTransaction(t *testing.T) {
	env := newEnv(t, 3)

	t.Run("успешное создание и обработка", func(t *testing.T) {
		userID := env.newUser(t, "0")

		rr := serve(t, env.handler, http.MethodPost, "/transactions", txBody(userID, "100.00", models.TypeDeposit))
		if rr.Code != http.StatusCreated {
			t.Fatalf("код ответа %d, ожидался 201; тело: %s", rr.Code, rr.Body.String())
		}

		var resp struct {
			ID int `json:"id"`
		}
		decodeBody(t, rr, &resp)
		if resp.ID <= 0 {
			t.Fatalf("в ответе некорректный id: %s", rr.Body.String())
		}

		env.waitProcessed(t, []int{resp.ID}, waitTimeout)
		assertBalance(t, env.balance(t, userID), "100.00")
	})

	t.Run("несуществующий пользователь", func(t *testing.T) {
		rr := serve(t, env.handler, http.MethodPost, "/transactions", txBody(missingID, "10.00", models.TypeDeposit))
		assertErrorResponse(t, rr, http.StatusNotFound)
	})

	t.Run("списание больше баланса отклоняется", func(t *testing.T) {
		userID := env.newUser(t, "10.00")

		rr := serve(t, env.handler, http.MethodPost, "/transactions", txBody(userID, "50.00", models.TypeWithdraw))
		assertErrorResponse(t, rr, http.StatusConflict)

		if n := countTransactions(t, env, userID); n != 0 {
			t.Fatalf("отклонённая транзакция сохранена в БД (записей: %d)", n)
		}
		assertBalance(t, env.balance(t, userID), "10.00")
	})
}

func TestGetTransaction(t *testing.T) {
	env := newEnv(t, 1)
	userID := env.newUser(t, "0")
	id := env.insertPending(t, userID, "42.50", models.TypeDeposit)

	t.Run("существующая транзакция", func(t *testing.T) {
		rr := serve(t, env.handler, http.MethodGet, txPath(id), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("код ответа %d, ожидался 200; тело: %s", rr.Code, rr.Body.String())
		}

		var got models.Transaction
		decodeBody(t, rr, &got)
		if got.ID != id || got.UserID != userID || got.Type != models.TypeDeposit {
			t.Fatalf("получена транзакция %+v", got)
		}
		if !got.Amount.Equal(decimal.RequireFromString("42.50")) {
			t.Fatalf("amount = %s, ожидалось 42.50", got.Amount)
		}
		if got.Processed {
			t.Fatal("необработанная транзакция вернулась с processed = true")
		}
		if got.Timestamp.IsZero() {
			t.Fatal("timestamp не заполнен")
		}
	})

	t.Run("несуществующая транзакция", func(t *testing.T) {
		rr := serve(t, env.handler, http.MethodGet, txPath(missingID), nil)
		assertErrorResponse(t, rr, http.StatusNotFound)
	})
}

func TestUpdateTransaction(t *testing.T) {
	env := newEnv(t, 1)

	t.Run("обновление необработанной транзакции", func(t *testing.T) {
		userID := env.newUser(t, "0")
		id := env.insertPending(t, userID, "10.00", models.TypeDeposit)

		rr := serve(t, env.handler, http.MethodPut, txPath(id), txBody(userID, "75.25", models.TypeDeposit))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("код ответа %d, ожидался 204; тело: %s", rr.Code, rr.Body.String())
		}

		var got models.Transaction
		decodeBody(t, serve(t, env.handler, http.MethodGet, txPath(id), nil), &got)
		if !got.Amount.Equal(decimal.RequireFromString("75.25")) {
			t.Fatalf("после обновления amount = %s, ожидалось 75.25", got.Amount)
		}

		// Процессору передана устаревшая сумма: он обязан применить актуальную из БД
		env.proc.Submit(models.Transaction{ID: id, UserID: userID, Amount: decimal.NewFromInt(10), Type: models.TypeDeposit})
		env.waitProcessed(t, []int{id}, waitTimeout)
		assertBalance(t, env.balance(t, userID), "75.25")
	})

	t.Run("обработанную транзакцию изменить нельзя", func(t *testing.T) {
		userID := env.newUser(t, "0")
		id, err := env.createAndSubmit(userID, "20.00", models.TypeDeposit)
		if err != nil {
			t.Fatalf("создание транзакции: %v", err)
		}
		env.waitProcessed(t, []int{id}, waitTimeout)

		rr := serve(t, env.handler, http.MethodPut, txPath(id), txBody(userID, "99.00", models.TypeDeposit))
		assertErrorResponse(t, rr, http.StatusConflict)

		var got models.Transaction
		decodeBody(t, serve(t, env.handler, http.MethodGet, txPath(id), nil), &got)
		if !got.Amount.Equal(decimal.RequireFromString("20.00")) {
			t.Fatalf("amount изменился: %s", got.Amount)
		}
	})

	t.Run("несуществующая транзакция", func(t *testing.T) {
		userID := env.newUser(t, "0")

		rr := serve(t, env.handler, http.MethodPut, txPath(missingID), txBody(userID, "10.00", models.TypeDeposit))
		assertErrorResponse(t, rr, http.StatusNotFound)
	})

	t.Run("несуществующий пользователь", func(t *testing.T) {
		userID := env.newUser(t, "0")
		id := env.insertPending(t, userID, "10.00", models.TypeDeposit)

		rr := serve(t, env.handler, http.MethodPut, txPath(id), txBody(missingID, "10.00", models.TypeDeposit))
		assertErrorResponse(t, rr, http.StatusNotFound)
	})
}

func TestDeleteTransaction(t *testing.T) {
	env := newEnv(t, 3)

	t.Run("удаление необработанной транзакции", func(t *testing.T) {
		userID := env.newUser(t, "50.00")
		id := env.insertPending(t, userID, "10.00", models.TypeDeposit)

		rr := serve(t, env.handler, http.MethodDelete, txPath(id), nil)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("код ответа %d, ожидался 204; тело: %s", rr.Code, rr.Body.String())
		}

		assertErrorResponse(t, serve(t, env.handler, http.MethodGet, txPath(id), nil), http.StatusNotFound)
		assertBalance(t, env.balance(t, userID), "50.00")
	})

	t.Run("несуществующая транзакция", func(t *testing.T) {
		rr := serve(t, env.handler, http.MethodDelete, txPath(missingID), nil)
		assertErrorResponse(t, rr, http.StatusNotFound)
	})

	t.Run("реверс баланса для обработанной транзакции", func(t *testing.T) {
		userID := env.newUser(t, "0")

		deposit, err := env.createAndSubmit(userID, "100.00", models.TypeDeposit)
		if err != nil {
			t.Fatalf("создание deposit: %v", err)
		}
		env.waitProcessed(t, []int{deposit}, waitTimeout)

		withdraw, err := env.createAndSubmit(userID, "60.00", models.TypeWithdraw)
		if err != nil {
			t.Fatalf("создание withdraw: %v", err)
		}
		env.waitProcessed(t, []int{withdraw}, waitTimeout)
		assertBalance(t, env.balance(t, userID), "40.00")

		// Реверс deposit оставил бы баланс -60: удаление должно быть отклонено целиком
		rr := serve(t, env.handler, http.MethodDelete, txPath(deposit), nil)
		assertErrorResponse(t, rr, http.StatusConflict)
		if got := serve(t, env.handler, http.MethodGet, txPath(deposit), nil); got.Code != http.StatusOK {
			t.Fatalf("транзакция исчезла после отклонённого удаления, код %d", got.Code)
		}
		assertBalance(t, env.balance(t, userID), "40.00")

		// Реверс withdraw возвращает средства
		if rr := serve(t, env.handler, http.MethodDelete, txPath(withdraw), nil); rr.Code != http.StatusNoContent {
			t.Fatalf("удаление withdraw: код %d, тело: %s", rr.Code, rr.Body.String())
		}
		assertBalance(t, env.balance(t, userID), "100.00")

		// Теперь реверс deposit допустим
		if rr := serve(t, env.handler, http.MethodDelete, txPath(deposit), nil); rr.Code != http.StatusNoContent {
			t.Fatalf("удаление deposit: код %d, тело: %s", rr.Code, rr.Body.String())
		}
		assertBalance(t, env.balance(t, userID), "0.00")
	})
}

func TestGetUserBalance(t *testing.T) {
	env := newEnv(t, 1)

	t.Run("существующий пользователь", func(t *testing.T) {
		userID := env.newUser(t, "123.45")

		rr := serve(t, env.handler, http.MethodGet, "/users/"+strconv.Itoa(userID)+"/balance", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("код ответа %d, ожидался 200; тело: %s", rr.Code, rr.Body.String())
		}

		var got models.User
		decodeBody(t, rr, &got)
		if got.ID != userID {
			t.Fatalf("id = %d, ожидалось %d", got.ID, userID)
		}
		assertBalance(t, got.Balance, "123.45")
	})

	t.Run("несуществующий пользователь", func(t *testing.T) {
		rr := serve(t, env.handler, http.MethodGet, "/users/"+strconv.Itoa(missingID)+"/balance", nil)
		assertErrorResponse(t, rr, http.StatusNotFound)
	})
}

func TestHealthCheckEndpoint(t *testing.T) {
	env := newEnv(t, 1)

	rr := serve(t, env.handler, http.MethodGet, "/health", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("код ответа %d, ожидался 200", rr.Code)
	}
}

func TestTransactionProcessing(t *testing.T) {
	t.Run("deposit применяется ровно один раз", func(t *testing.T) {
		env := newEnv(t, 1)
		userID := env.newUser(t, "10.00")

		rr := serve(t, env.handler, http.MethodPost, "/transactions", txBody(userID, "25.50", models.TypeDeposit))
		if rr.Code != http.StatusCreated {
			t.Fatalf("код ответа %d; тело: %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			ID int `json:"id"`
		}
		decodeBody(t, rr, &resp)

		env.waitProcessed(t, []int{resp.ID}, waitTimeout)

		var got models.Transaction
		decodeBody(t, serve(t, env.handler, http.MethodGet, txPath(resp.ID), nil), &got)
		if !got.Processed {
			t.Fatal("после обработки API вернул processed = false")
		}
		assertBalance(t, env.balance(t, userID), "35.50")
	})

	t.Run("withdraw уменьшает баланс", func(t *testing.T) {
		env := newEnv(t, 1)
		userID := env.newUser(t, "100.00")

		id, err := env.createAndSubmit(userID, "40.00", models.TypeWithdraw)
		if err != nil {
			t.Fatalf("создание транзакции: %v", err)
		}
		env.waitProcessed(t, []int{id}, waitTimeout)
		assertBalance(t, env.balance(t, userID), "60.00")
	})

	// Один воркер обрабатывает очередь строго по порядку, поэтому обработка
	// завершающей контрольной транзакции означает, что предыдущие уже разобраны
	t.Run("нехватка средств при обработке оставляет транзакцию необработанной", func(t *testing.T) {
		env := newEnv(t, 1)
		userID := env.newUser(t, "10.00")

		// Записи в обход сервиса: предварительная проверка при создании ее бы отклонила
		var badID int
		err := env.pool.QueryRow(context.Background(),
			`INSERT INTO transactions (user_id, amount, type) VALUES ($1, 50.00, 'withdraw') RETURNING id`,
			userID,
		).Scan(&badID)
		if err != nil {
			t.Fatalf("вставка транзакции: %v", err)
		}
		env.proc.Submit(models.Transaction{ID: badID, UserID: userID})

		sentinel, err := env.createAndSubmit(userID, "5.00", models.TypeDeposit)
		if err != nil {
			t.Fatalf("создание контрольной транзакции: %v", err)
		}
		env.waitProcessed(t, []int{sentinel}, waitTimeout)

		if env.isProcessed(t, badID) {
			t.Fatal("транзакция без достаточных средств помечена обработанной")
		}
		assertBalance(t, env.balance(t, userID), "15.00")
	})

	t.Run("повторная постановка в очередь не удваивает баланс", func(t *testing.T) {
		env := newEnv(t, 1)
		userID := env.newUser(t, "0")

		id, err := env.createAndSubmit(userID, "10.00", models.TypeDeposit)
		if err != nil {
			t.Fatalf("создание транзакции: %v", err)
		}
		env.proc.Submit(models.Transaction{ID: id, UserID: userID})
		env.proc.Submit(models.Transaction{ID: id, UserID: userID})

		sentinel, err := env.createAndSubmit(userID, "1.00", models.TypeDeposit)
		if err != nil {
			t.Fatalf("создание контрольной транзакции: %v", err)
		}
		env.waitProcessed(t, []int{id, sentinel}, waitTimeout)
		assertBalance(t, env.balance(t, userID), "11.00")
	})

	t.Run("удалённая до обработки транзакция пропускается", func(t *testing.T) {
		env := newEnv(t, 1)
		userID := env.newUser(t, "0")

		id := env.insertPending(t, userID, "10.00", models.TypeDeposit)
		if rr := serve(t, env.handler, http.MethodDelete, txPath(id), nil); rr.Code != http.StatusNoContent {
			t.Fatalf("удаление: код %d, тело: %s", rr.Code, rr.Body.String())
		}
		env.proc.Submit(models.Transaction{ID: id, UserID: userID})

		sentinel, err := env.createAndSubmit(userID, "1.00", models.TypeDeposit)
		if err != nil {
			t.Fatalf("создание контрольной транзакции: %v", err)
		}
		env.waitProcessed(t, []int{sentinel}, waitTimeout)
		assertBalance(t, env.balance(t, userID), "1.00")
	})
}
