package processor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nikolaykonkin/go-finops-service/internal/db"
	"github.com/nikolaykonkin/go-finops-service/internal/models"
	"github.com/nikolaykonkin/go-finops-service/internal/repositories"
	"github.com/shopspring/decimal"
)

const (
	// Размер буфера очереди: при переполнении Submit блокируется,
	// пока воркеры не освободят место (обратное давление)
	queueSize = 100

	// Максимальное время обработки одной транзакции
	processTimeout = 10 * time.Second
)

// Processor асинхронно применяет транзакции к балансам пользователей
//
// Корректность обеспечивают два уровня блокировок:
//   - мьютекс пользователя сериализует обработку его транзакций внутри
//     процесса, чтобы воркеры не занимали соединения, ожидая друг друга;
//   - блокировка строки в БД (FOR UPDATE) гарантирует корректность при
//     любых конкурентных операциях, в том числе из других экземпляров
type Processor struct {
	pool        *pgxpool.Pool
	userRepo    repositories.UserRepository
	jobs        chan models.Transaction
	userMutexes map[int]*sync.Mutex
	mu          sync.RWMutex // защищает userMutexes

	wg      sync.WaitGroup
	closeMu sync.RWMutex // защищает closed и закрытие канала jobs
	closed  bool
}

// NewProcessor создает процессор и запускает numWorkers воркеров
func NewProcessor(pool *pgxpool.Pool, numWorkers int) *Processor {
	if numWorkers < 1 {
		numWorkers = 1
	}

	p := &Processor{
		pool:        pool,
		userRepo:    repositories.NewUserRepo(pool),
		jobs:        make(chan models.Transaction, queueSize),
		userMutexes: make(map[int]*sync.Mutex),
	}

	p.wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go p.worker()
	}

	return p
}

// Submit ставит транзакцию в очередь обработки - блокируется, если очередь заполнена
// После Close транзакция отклоняется и остается необработанной
//
// RLock удерживается на время отправки в канал: это исключает отправку в уже
// закрытый канал (panic) при конкурентном вызове Close
func (p *Processor) Submit(tx models.Transaction) {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()

	if p.closed {
		log.Printf("processor: транзакция %d отклонена, процессор остановлен", tx.ID)
		return
	}

	p.jobs <- tx
}

// worker обрабатывает транзакции из канала, пока канал не будет закрыт
func (p *Processor) worker() {
	defer p.wg.Done()

	for tx := range p.jobs {
		p.safeProcess(tx)
	}
}

// safeProcess изолирует panic внутри обработки одной транзакции, чтобы
// она не завершила весь процесс
func (p *Processor) safeProcess(tx models.Transaction) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("processor: panic при обработке транзакции %d: %v", tx.ID, r)
		}
	}()

	if err := p.process(tx); err != nil {
		log.Printf("processor: транзакция %d не обработана: %v", tx.ID, err)
	}
}

// userMutex возвращает мьютекс пользователя, создавая его при первом обращении
func (p *Processor) userMutex(userID int) *sync.Mutex {
	p.mu.RLock()
	m, ok := p.userMutexes[userID]
	p.mu.RUnlock()
	if ok {
		return m
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Повторная проверка: пока ожидали Lock, мьютекс мог создать другой воркер
	if m, ok = p.userMutexes[userID]; ok {
		return m
	}
	m = &sync.Mutex{}
	p.userMutexes[userID] = m

	return m
}

// process применяет одну транзакцию к балансу и устанавливает processed = true
// в единой БД-транзакции
//
// Из переданной структуры используются ID и UserID (последний только для выбора мьютекса)
// Остальные поля читаются из БД под блокировкой строки, поэтому изменения, сделанные через PUT
// до начала обработки, не теряются, а удаленная или уже обработанная транзакция пропускается
// Корректность при смене user_id через PUT обеспечивает блокировка в БД, а не мьютекс
//
// Транзакция, для которой не хватает средств, остается необработанной
// (processed = false); причина возвращается вызывающему для логирования
func (p *Processor) process(tx models.Transaction) error {
	mu := p.userMutex(tx.UserID)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), processTimeout)
	defer cancel()

	dbTx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало транзакции БД: %w", err)
	}
	defer db.Rollback(dbTx)

	var (
		userID    int
		amount    decimal.Decimal
		txType    string
		processed bool
	)
	err = dbTx.QueryRow(ctx,
		`SELECT user_id, amount, type, COALESCE(processed, FALSE)
		   FROM transactions
		  WHERE id = $1
		    FOR UPDATE`,
		tx.ID,
	).Scan(&userID, &amount, &txType, &processed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // удалена до начала обработки
	}
	if err != nil {
		return fmt.Errorf("чтение транзакции: %w", err)
	}
	if processed {
		return nil // уже обработана
	}

	// NO KEY UPDATE блокирует изменение баланса другими, но не мешает
	// вставке новых транзакций пользователя (ссылочная проверка внешнего ключа)
	var balance decimal.Decimal
	err = dbTx.QueryRow(ctx,
		`SELECT COALESCE(balance, 0) FROM users WHERE id = $1 FOR NO KEY UPDATE`,
		userID,
	).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return repositories.ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("чтение баланса пользователя %d: %w", userID, err)
	}

	delta := amount
	if txType == models.TypeWithdraw {
		if balance.LessThan(amount) {
			return fmt.Errorf("%w: баланс %s, сумма списания %s",
				repositories.ErrInsufficientFunds, balance, amount)
		}
		delta = amount.Neg()
	}

	if err := p.userRepo.UpdateBalance(ctx, userID, delta, dbTx); err != nil {
		return err
	}

	if _, err := dbTx.Exec(ctx,
		`UPDATE transactions SET processed = TRUE WHERE id = $1`,
		tx.ID,
	); err != nil {
		return fmt.Errorf("установка processed: %w", err)
	}

	if err := dbTx.Commit(ctx); err != nil {
		return fmt.Errorf("коммит транзакции БД: %w", err)
	}

	return nil
}

// Close останавливает прием новых транзакций, дожидается обработки всего, что уже находится
// в очереди, и завершает воркеры - повторный вызов безопасен
func (p *Processor) Close() error {
	p.closeMu.Lock()
	if p.closed {
		p.closeMu.Unlock()
		return nil
	}
	p.closed = true
	close(p.jobs)
	p.closeMu.Unlock()

	p.wg.Wait()
	return nil
}
