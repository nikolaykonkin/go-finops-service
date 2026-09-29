package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nikolaykonkin/go-finops-service/internal/api"
	"github.com/nikolaykonkin/go-finops-service/internal/config"
	"github.com/nikolaykonkin/go-finops-service/internal/middleware"
	"github.com/nikolaykonkin/go-finops-service/internal/models"
	"github.com/nikolaykonkin/go-finops-service/internal/processor"
	"github.com/nikolaykonkin/go-finops-service/internal/repositories"
	"github.com/nikolaykonkin/go-finops-service/internal/services"
	"github.com/shopspring/decimal"
)

// Заведомо несуществующий идентификатор: максимальное значение INT в Postgres
const missingID = 2147483647

// testDSN возвращает DSN тестовой БД: TEST_DATABASE_URL, иначе настройки приложения
func testDSN() string {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return config.LoadConfig().DBDSN
}

// openTestPool подключается к БД
// Если БД недоступна, тест пропускается (запуск: docker-compose up -d db)
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(testDSN())
	if err != nil {
		t.Fatalf("разбор DSN тестовой БД: %v", err)
	}
	cfg.MaxConns = 20

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("БД недоступна, тест пропущен (docker-compose up -d db): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("БД недоступна, тест пропущен (docker-compose up -d db): %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// testEnv — полностью собранное приложение поверх тестовой БД
type testEnv struct {
	pool    *pgxpool.Pool
	txSvc   *services.TransactionService
	proc    *processor.Processor
	handler http.Handler

	mu      sync.Mutex
	userIDs []int
}

// newEnv собирает приложение так же, как server/main.go, с заданным числом воркеров
// По завершении теста процессор останавливается, созданные данные удаляются
func newEnv(t *testing.T, workers int) *testEnv {
	t.Helper()

	pool := openTestPool(t)
	userRepo := repositories.NewUserRepo(pool)
	txRepo := repositories.NewTransactionRepo(pool)
	txSvc := services.NewTransactionService(txRepo, userRepo, pool)
	userSvc := services.NewUserService(userRepo)
	proc := processor.NewProcessor(pool, workers)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, txSvc, userSvc, proc)

	env := &testEnv{
		pool:    pool,
		txSvc:   txSvc,
		proc:    proc,
		handler: middleware.RecoveryMiddleware(mux),
	}
	t.Cleanup(env.cleanup)

	return env
}

// cleanup выполняется до закрытия пула (Cleanup вызываются в обратном порядке)
func (e *testEnv) cleanup() {
	_ = e.proc.Close()

	ctx := context.Background()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range e.userIDs {
		_, _ = e.pool.Exec(ctx, `DELETE FROM transactions WHERE user_id = $1`, id)
		_, _ = e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	}
}

// newUser создает пользователя с заданным начальным балансом
func (e *testEnv) newUser(t *testing.T, balance string) int {
	t.Helper()

	var id int
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO users (balance) VALUES ($1) RETURNING id`, balance,
	).Scan(&id)
	if err != nil {
		t.Fatalf("создание пользователя: %v", err)
	}

	e.mu.Lock()
	e.userIDs = append(e.userIDs, id)
	e.mu.Unlock()

	return id
}

// balance читает баланс напрямую из БД
func (e *testEnv) balance(t *testing.T, userID int) decimal.Decimal {
	t.Helper()

	var b decimal.Decimal
	err := e.pool.QueryRow(context.Background(),
		`SELECT balance FROM users WHERE id = $1`, userID,
	).Scan(&b)
	if err != nil {
		t.Fatalf("чтение баланса пользователя %d: %v", userID, err)
	}

	return b
}

// insertPending сохраняет транзакцию через сервис, но не отдает ее процессору:
// она гарантированно остается processed = false
func (e *testEnv) insertPending(t *testing.T, userID int, amount, typ string) int {
	t.Helper()

	tx := &models.Transaction{UserID: userID, Amount: decimal.RequireFromString(amount), Type: typ}
	id, err := e.txSvc.CreateTransaction(context.Background(), tx)
	if err != nil {
		t.Fatalf("создание транзакции: %v", err)
	}

	return id
}

// createAndSubmit создает транзакцию и отдает ее процессору
// Возвращает ошибку вместо t.Fatal, поэтому безопасна для вызова из горутин
func (e *testEnv) createAndSubmit(userID int, amount, typ string) (int, error) {
	tx := &models.Transaction{UserID: userID, Amount: decimal.RequireFromString(amount), Type: typ}
	id, err := e.txSvc.CreateTransaction(context.Background(), tx)
	if err != nil {
		return 0, err
	}

	e.proc.Submit(models.Transaction{ID: id, UserID: userID, Amount: tx.Amount, Type: typ})
	return id, nil
}

// isProcessed возвращает значение флага processed напрямую из БД
func (e *testEnv) isProcessed(t *testing.T, id int) bool {
	t.Helper()

	var processed bool
	err := e.pool.QueryRow(context.Background(),
		`SELECT COALESCE(processed, FALSE) FROM transactions WHERE id = $1`, id,
	).Scan(&processed)
	if err != nil {
		t.Fatalf("чтение processed транзакции %d: %v", id, err)
	}

	return processed
}

// waitProcessed ждет, пока все транзакции из ids получат processed = true
func (e *testEnv) waitProcessed(t *testing.T, ids []int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		var n int
		err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM transactions WHERE id = ANY($1) AND processed`, ids,
		).Scan(&n)
		if err != nil {
			t.Fatalf("подсчёт обработанных транзакций: %v", err)
		}
		if n == len(ids) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("за %s обработано %d транзакций из %d", timeout, n, len(ids))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runConcurrently запускает n горутин и собирает ID созданных ими транзакций
func runConcurrently(t *testing.T, n int, fn func(i int) (int, error)) []int {
	t.Helper()

	ids := make([]int, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = fn(i)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("горутина %d: %v", i, err)
		}
	}

	return ids
}

// assertBalance сравнивает баланс с ожидаемым значением без учета формата записи
func assertBalance(t *testing.T, got decimal.Decimal, want string) {
	t.Helper()

	if !got.Equal(decimal.RequireFromString(want)) {
		t.Fatalf("баланс: получено %s, ожидалось %s", got, want)
	}
}

// serve выполняет запрос напрямую через http.Handler, без сетевого соединения
// body: nil, строка (отправляется как есть) или значение для JSON-кодирования
func serve(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("кодирование тела запроса: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	return rr
}

// decodeBody разбирает JSON-ответ в dst
func decodeBody(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()

	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		t.Fatalf("разбор ответа %q: %v", rr.Body.String(), err)
	}
}

// txBody формирует тело запроса POST/PUT /transactions
func txBody(userID int, amount, typ string) map[string]any {
	return map[string]any{"user_id": userID, "amount": amount, "type": typ}
}
