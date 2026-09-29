package tests

import (
	"sync"
	"testing"
	"time"

	"github.com/nikolaykonkin/go-finops-service/internal/models"
)

// Тесты конкурентности запускаются с проверкой гонок:
//
//	go test -race ./tests -v
//
// Они требуют работающий Postgres (docker-compose up -d db), иначе пропускаются

// TestConcurrency: множество горутин одновременно создают транзакции одного
// пользователя и отправляют их в канал процессора; ни одна не должна потеряться
func TestConcurrency(t *testing.T) {
	const n = 100

	env := newEnv(t, 5)
	userID := env.newUser(t, "0")

	ids := runConcurrently(t, n, func(i int) (int, error) {
		return env.createAndSubmit(userID, "10.00", models.TypeDeposit)
	})

	env.waitProcessed(t, ids, 30*time.Second)
	assertBalance(t, env.balance(t, userID), "1000.00")
}

// TestMutexConcurrency: параллельные зачисления и списания одного пользователя
// сериализуются мьютексом пользователя и блокировкой строки в БД, поэтому
// итоговый баланс равен начальному (нет потерянных обновлений)
func TestMutexConcurrency(t *testing.T) {
	const pairs = 40

	env := newEnv(t, 8)
	userID := env.newUser(t, "1000.00")

	ids := runConcurrently(t, 2*pairs, func(i int) (int, error) {
		if i%2 == 0 {
			return env.createAndSubmit(userID, "5.00", models.TypeDeposit)
		}
		return env.createAndSubmit(userID, "5.00", models.TypeWithdraw)
	})

	env.waitProcessed(t, ids, 30*time.Second)
	assertBalance(t, env.balance(t, userID), "1000.00")
}

// TestChannelCommunication: транзакций больше, чем размер буфера очереди (100), а воркер один
// Submit должен блокироваться на переполнении без взаимной блокировки,
// а Close, вернув управление, гарантировать обработку всей очереди
func TestChannelCommunication(t *testing.T) {
	const n = 250

	env := newEnv(t, 1)
	userID := env.newUser(t, "0")

	ids := make([]int, n)
	for i := range ids {
		ids[i] = env.insertPending(t, userID, "1.00", models.TypeDeposit)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, id := range ids {
			env.proc.Submit(models.Transaction{ID: id, UserID: userID})
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Submit не завершился: очередь заблокирована")
	}

	if err := env.proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// После Close все принятые транзакции уже обработаны, ожидание не требуется
	env.waitProcessed(t, ids, time.Second)
	assertBalance(t, env.balance(t, userID), "250.00")
}

// TestRaceCondition: транзакции многих пользователей создаются и обрабатываются параллельно
// Тест нагружает создание мьютексов пользователей (конкурентный доступ к общей карте)
// и вместе с флагом -race выявляет гонки данных
func TestRaceCondition(t *testing.T) {
	const (
		users   = 20
		perUser = 10
	)

	env := newEnv(t, 5)

	userIDs := make([]int, users)
	for i := range userIDs {
		userIDs[i] = env.newUser(t, "0")
	}

	ids := runConcurrently(t, users*perUser, func(i int) (int, error) {
		return env.createAndSubmit(userIDs[i%users], "3.50", models.TypeDeposit)
	})

	env.waitProcessed(t, ids, 30*time.Second)
	for _, id := range userIDs {
		assertBalance(t, env.balance(t, id), "35.00")
	}
}

// TestSubmitCloseRace: конкурентные Submit и Close не приводят ни к panic (отправка в закрытый канал),
// ни к взаимной блокировке; Submit после Close безопасно отклоняется
func TestSubmitCloseRace(t *testing.T) {
	env := newEnv(t, 3)
	userID := env.newUser(t, "0")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Несуществующий ID: процессор пропускает такие записи
			env.proc.Submit(models.Transaction{ID: -1, UserID: userID})
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := env.proc.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("Submit и Close заблокировали друг друга")
	}

	// Повторный вызов и Submit после остановки не должны паниковать
	env.proc.Submit(models.Transaction{ID: -1, UserID: userID})
	if err := env.proc.Close(); err != nil {
		t.Fatalf("повторный Close: %v", err)
	}
}
