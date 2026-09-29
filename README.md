# go-finops-service

Микросервис для обработки финансовых операций на Go: REST API, PostgreSQL, асинхронная обработка через worker pool, ACID-транзакции, Docker.

Реализован в рамках кейса «Go-Backend Challenge: Построй микросервис» из библиотеки кейсов Нетологии для Go-разработчиков.

## Структура проекта

```
finops-service/
├── server/
│   └── main.go                    — точка входа: конфиг, DI, роутинг, graceful shutdown
├── internal/
│   ├── api/
│   │   └── handlers.go            — HTTP-обработчики (6 эндпоинтов) и RegisterRoutes
│   ├── config/
│   │   └── config.go              — загрузка конфигурации из переменных окружения
│   ├── db/
│   │   └── db.go                  — пул подключений к PostgreSQL, Ping с retry
│   ├── middleware/
│   │   └── middleware.go          — логирование запросов и recovery после паник
│   ├── models/
│   │   └── models.go              — User, Transaction, константы типов
│   ├── processor/
│   │   └── processor.go           — worker pool, per-user mutex, асинхронная обработка
│   ├── repositories/
│   │   ├── errors.go              — sentinel-ошибки слоя данных
│   │   ├── interfaces.go          — контракты UserRepository и TransactionRepository
│   │   ├── transaction_repo.go    — CRUD транзакций
│   │   └── user_repo.go           — GetBalance, UpdateBalance
│   └── services/
│       ├── transaction_service.go — бизнес-логика транзакций, ACID, валидация
│       └── user_service.go        — бизнес-логика пользователей
├── migrations/
│   └── init.sql                   — схема БД: users, transactions, enum transaction_type
├── tests/
│   ├── api_test.go                — unit-тесты без БД: JSON, валидация, HTTP-контракт
│   ├── concurrency_test.go        — тесты конкурентности и race conditions
│   ├── helpers_test.go            — testEnv: сборка приложения для тестов
│   └── integration_test.go        — интеграционные тесты с реальной БД
├── docker-compose.yml             — сервисы db (PostgreSQL) и app
├── Dockerfile                     — multi-stage сборка
├── go.mod                         — модуль и зависимости
└── README.md                      — этот файл
```

## Что было дано

Шаблон микросервиса с готовой архитектурой и каркасом кода. Требовалось реализовать недостающие методы, покрыть тестами и упаковать в Docker.

Структура шаблона: `internal/{api,config,db,middleware,models,processor, repositories,services}`, `migrations/init.sql`, `tests/`, `Dockerfile.template`.

## Что реализовано

### Слой доступа к данным

- `internal/db/db.go` — `NewPool`: создает пул `pgxpool`, настраивает `MaxConns`, повторяет `Ping` до 30 секунд (БД успевает стартовать в Docker), возвращает ошибку при недоступности
- `internal/repositories/user_repo.go` — `GetBalance`, `UpdateBalance`. Баланс обновляется в переданной БД-транзакции, условие `balance + amount >= 0` проверяется в самом UPDATE, поэтому баланс не может уйти в минус
- `internal/repositories/transaction_repo.go` — CRUD транзакций. `UpdateTransaction` атомарно проверяет `processed = false`, `DeleteTransactionTx` удаляет запись в БД-транзакции и возвращает удаленное значение
- `internal/repositories/errors.go` — sentinel-ошибки слоя данных: `ErrUserNotFound`, `ErrTransactionNotFound`, `ErrInsufficientFunds`, `ErrTransactionProcessed`

### Бизнес-логика

- `internal/services/user_service.go` — `GetBalance` с валидацией `userID > 0`
- `internal/services/transaction_service.go`:
  - `ValidateTransaction` — проверяет `user_id`, тип (`deposit` или `withdraw`), сумму (положительная, до 2 знаков после запятой, не больше `NUMERIC(10,2)`)
  - `CreateTransaction` — валидирует, проверяет существование пользователя и достаточность средств для withdraw, сохраняет запись со `processed = false` (баланс здесь не меняется — его применяет процессор ровно один раз)
  - `GetTransaction`, `UpdateTransaction` (только необработанные), `DeleteTransaction` (с реверсом баланса для обработанных транзакций в одной БД-транзакции)

### HTTP API

- `internal/api/handlers.go` — 6 обработчиков:
  - `POST /transactions` → 201;
  - `GET /transactions/{id}` → 200 или 404;
  - `PUT /transactions/{id}` → 204;
  - `DELETE /transactions/{id}` → 204;
  - `GET /users/{user_id}/balance` → 200;
  - `GET /health` → 200.
- `RegisterRoutes` — единая таблица маршрутов, используется в `main` и в тестах
- Строгий разбор JSON: ограничение размера тела, запрет неизвестных полей, проверка отсутствия данных после JSON
- Sentinel-ошибки маппятся в HTTP-статусы: 400 (валидация), 404 (не найдено), 409 (конфликт: недостаточно средств, транзакция уже обработана), 500 (внутреннее)

### Middleware

- `LoggingMiddleware` — логирует метод, путь, код ответа, время выполнения
- `RecoveryMiddleware` — перехватывает панику, логирует стек, возвращает 500 в JSON

### Асинхронная обработка

- `internal/processor/processor.go`:
  - `NewProcessor` запускает N воркеров, буфер канала jobs = 100
  - `Submit` ставит транзакцию в очередь, блокируется при переполнении (обратное давление), после `Close` отправка безопасно отклоняется
  - `process` в одной БД-транзакции читает запись под `FOR UPDATE`, проверяет баланс (`FOR NO KEY UPDATE`), обновляет его, ставит `processed = true`, идемпотентен: повторная постановка в очередь не удваивает баланс, удаленная или уже обработанная транзакция пропускается
  - `Close` останавливает прием, дожидается обработки очереди, завершает воркеры, повторный вызов безопасен
- Per-user mutex сериализует обработку транзакций одного пользователя внутри процесса, корректность при параллельных операциях из других экземпляров обеспечивает блокировка строки в БД

### Точка входа

- `server/main.go`:
  - `signal.NotifyContext` для SIGINT и SIGTERM
  - `http.Server` с таймаутами: `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`
  - Graceful shutdown: сначала `srv.Shutdown`, затем `proc.Close` (дорабатывает очередь), затем закрытие пула

### Тесты

- `tests/api_test.go` — без БД: сериализация JSON, валидация типов и сумм, HTTP-контракт для 400 и 405, recovery middleware, `TestCreateTransactionRequest` покрывает 13 сценариев ошибочного тела запроса
- `tests/integration_test.go` — с реальной БД: CRUD, реверс баланса, нехватка средств, несуществующие сущности
- `tests/concurrency_test.go` — гонки: параллельные deposit и withdraw, обратное давление очереди, конкурентные `Submit` и `Close`, `TestRaceCondition`
- `tests/helpers_test.go` — `testEnv` для сборки приложения в тестах

Запуск тестов:

```bash
go test ./tests -v
go test -race ./tests -v
```

Если БД недоступна, интеграционные и concurrency-тесты пропускаются через `t.Skip`, unit-тесты выполняются всегда.

Покрытие `internal/services` зависит от того, поднята ли БД:

| Состояние БД | Покрытие | Что выполняется |
|---|---|---|
| БД запущена | **88.9%** | unit + integration + concurrency |
| БД недоступна | **27.8%** | только unit-тесты, остальные SKIP |

Интеграционные и concurrency-тесты требуют PostgreSQL. Если БД недоступна, они пропускаются через `t.Skip` — это видно в выводе `go test -v` как `SKIP`, а не `PASS`. Чтобы получить реальное покрытие, сначала поднимите БД:

```bash
docker compose up -d db
go test -coverprofile=cover_services.out -coverpkg=./internal/services/... ./tests
go tool cover -func=cover_services.out | tail -1
```

### Контейнеризация

- `Dockerfile` — multi-stage: build в `golang:1.25-alpine`, runtime в `alpine:3.20`. Бинарник собирается без CGO (`CGO_ENABLED=0`), контейнер запускается под непривилегированным пользователем, есть `HEALTHCHECK` через `/health`
- `docker-compose.yml` — сервисы `db` (PostgreSQL) и `app`, приложение стартует только после `service_healthy` у БД

## Ключевые решения

**Баланс меняет только процессор.** В `CreateTransaction` запись сохраняется со `processed = false` без изменения баланса. Если бы баланс менялся и там, и в процессоре, каждая операция учитывалась бы дважды. Семантика `processed` = «баланс обновлен» становится однозначной.
**Порядок проверок в `CreateTransaction`.** Проверка пользователя и баланса выполняется до `pool.Begin`. Репозитории читают через пул, и при полностью занятом пуле запрос, удерживающий соединение под `Begin` и ожидающий второго соединения для чтения, заблокировал бы сам себя.
**Окончательная проверка средств — в процессоре.** Предварительная проверка в сервисе может устареть к моменту обработки. Процессор перечитывает транзакцию и баланс под `FOR UPDATE` и `FOR NO KEY UPDATE` и отклоняет операцию, если средств не хватает. Транзакция остается `processed = false`: в схеме нет статуса ошибки.
**Sentinel-ошибки.** Слой данных возвращает `ErrUserNotFound` и другие, сервис и обработчики маппят их через `errors.Is` в HTTP-статусы. Это позволяет различать 404 (не найдено), 400 (валидация), 409 (конфликт) и 500 (внутреннее)
**Retry Ping в `NewPool`.** В `docker-compose` приложение может стартовать раньше, чем Postgres начнет принимать TCP-подключения. Ping с интервалом 1 секунда и общим таймаутом 30 секунд убирает гонку старта.
**Healthcheck в `docker-compose` через TCP (`-h 127.0.0.1`).** Во время выполнения `init.sql` временный сервер Postgres слушает только Unix-сокет, и обычная проверка `pg_isready -U user -d finops` сообщила бы о готовности раньше времени, из-за чего приложение могло упасть при старте.
**Строгий разбор JSON.** `MaxBytesReader`, `DisallowUnknownFields`, проверка отсутствия данных после первого JSON-значения. Тесты покрывают все ветки.

## Переменные окружения

Приложение читает конфигурацию из переменных окружения:

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DB_DSN` | — | Строка подключения к PostgreSQL, приоритетнее `DATABASE_URL` |
| `DATABASE_URL` | — | Строка подключения к PostgreSQL, используется, если `DB_DSN` не задан |
| `PORT` | `8080` | Порт HTTP-сервера |

Если ни `DB_DSN`, ни `DATABASE_URL` не заданы, используется строка по умолчанию: `postgres://user:pass@db:5432/finops?sslmode=disable`.

Пример `.env`:

```
DB_DSN=postgres://user:pass@localhost:5432/finops?sslmode=disable
PORT=8080
```

## Архитектура и оптимизации

### Слоистая архитектура

```
HTTP-запрос
    │
    ▼
Middleware (Logging, Recovery)
    │
    ▼
Handlers (internal/api)               — разбор JSON, коды ответов
    │
    ▼
Services (internal/services)          — валидация, бизнес-логика
    │
    ▼
Repositories (internal/repositories)  — SQL-запросы
    │
    ▼
PostgreSQL
```

- **Handlers** зависят от сервисов через конкретные типы
- **Services** зависят от интерфейсов репозиториев — можно подменить моками в тестах
- **Repositories** инкапсулируют SQL и возвращают sentinel-ошибки

### Асинхронная обработка

- **Worker pool**: `processor.NewProcessor(pool, 5)` запускает 5 воркеров
- **Буферизованный канал** jobs (размер 100) обеспечивает обратное давление при переполнении
- **Per-user mutex**: транзакции одного пользователя обрабатываются последовательно, транзакции разных пользователей — параллельно
- **Идемпотентность**: повторная постановка в очередь не удваивает баланс, удаленная или уже обработанная транзакция пропускается

### Работа с БД

- **Connection pooling**: `pgxpool` с `MaxConns = 20`
- **ACID-транзакции**: создание транзакции и обновление баланса — в одной БД-транзакции
- **Блокировки строк**: `SELECT ... FOR UPDATE` для транзакций, `SELECT ... FOR NO KEY UPDATE` для пользователей — предотвращают race conditions
- **Параметризованные запросы** (`$1`, `$2`) — защита от SQL-инъекций
- **Индексы**: `idx_transactions_user_id` ускоряет выборки по пользователю
- **`CHECK (amount > 0)`** и `CHECK` на баланс — защита на уровне схемы

### Устойчивость

- **Retry Ping** при старте (до 30 секунд) — приложение ждет, пока Postgres станет доступен
- **Graceful shutdown**: `srv.Shutdown` → `proc.Close` (доработка очереди) → `pool.Close`
- **Recovery middleware** — перехват паник, логирование стека, возврат 500 в JSON
- **Таймауты HTTP**: `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`

### Валидация и ошибки

- **Строгая валидация JSON**: `MaxBytesReader`, `DisallowUnknownFields`, проверка на данные после JSON
- **Sentinel-ошибки** в слое данных (`ErrUserNotFound`, `ErrTransactionNotFound`, `ErrInsufficientFunds`, `ErrTransactionProcessed`) маппятся в HTTP-статусы через `errors.Is`

## API Эндпоинты

### Получить баланс пользователя

```bash
GET /users/{user_id}/balance
```

Ответ:

```json
{
  "id": 1,
  "balance": "1000.00"
}
```

### Создать транзакцию

```bash
POST /transactions
```

Тело запроса:

```json
{
  "user_id": 1,
  "amount": "100.50",
  "type": "deposit"
}
```

Ответ 201:

```json
{
  "id": 1,
  "user_id": 1,
  "amount": "100.50",
  "type": "deposit",
  "timestamp": "2024-01-04T12:34:56Z",
  "processed": false
}
```

### Получить информацию о транзакции

```bash
GET /transactions/{transaction_id}
```

### Обновить транзакцию

```bash
PUT /transactions/{transaction_id}
```

### Удалить транзакцию

```bash
DELETE /transactions/{transaction_id}
```

### Проверка здоровья

```bash
GET /health
```

## Примеры использования

```bash
# Создать транзакцию пополнения
curl -X POST http://localhost:8080/transactions \
  -H "Content-Type: application/json" \
  -d '{"user_id":1,"amount":"100.00","type":"deposit"}'

# Получить баланс пользователя
curl http://localhost:8080/users/1/balance

# Получить информацию о транзакции
curl http://localhost:8080/transactions/1

# Обновить транзакцию
curl -X PUT http://localhost:8080/transactions/1 \
  -H "Content-Type: application/json" \
  -d '{"user_id":1,"amount":"50.00","type":"withdraw"}'

# Удалить транзакцию
curl -X DELETE http://localhost:8080/transactions/1

# Проверить здоровье сервиса
curl http://localhost:8080/health
```

## Разработка

### Добавление нового эндпоинта

1. Определить модель в `internal/models/`
2. Реализовать бизнес-логику в `internal/services/`
3. Добавить методы в интерфейсы и реализацию в `internal/repositories/`
4. Создать обработчик в `internal/api/handlers.go`
5. Зарегистрировать маршрут в `RegisterRoutes`
6. Добавить тесты в `tests/` — unit и интеграционный

### Работа с БД

Все операции с БД выполняются через контекст и, где требуется атомарность, через БД-транзакции:

```go
dbTx, err := pool.Begin(ctx)
if err != nil {
    return err
}
defer db.Rollback(dbTx)

// операции внутри одной транзакции
if err := dbTx.Commit(ctx); err != nil {
    return err
}
```

### Запуск проверок

```bash
docker compose up -d db

go test ./tests -v
go test -race ./tests -v
go vet ./...
gofmt -l .
```

## Troubleshooting

### Приложение падает при старте: `БД недоступна`

**Причина**: Postgres не успел запуститься или указан неверный DSN.

**Решение**:

```bash
docker compose ps
docker compose logs db
```

`NewPool` повторяет `Ping` до 30 секунд, так что короткие задержки
старта не приводят к падению.

### Healthcheck не проходит

**Причина**: приложение не может подключиться к БД или сервис `app` не запущен.

**Решение**:

```bash
docker compose ps
docker compose logs app
docker compose logs db
```

### Порт уже занят

**Причина**: `8080` или `5432` заняты другим процессом.

**Решение**:

```bash
lsof -i :8080
lsof -i :5432
```

Остановить процесс или изменить порт в `docker-compose.yml` и переменной `PORT`.

### Тесты пропускаются (SKIP)

**Причина**: `docker compose up -d db` не выполнен. Интеграционные и concurrency-тесты пропускаются через `t.Skip`, если БД недоступна.

**Решение**:

```bash
docker compose up -d db
go test ./tests -v
```

### Тесты падают: `connection refused`

**Причина**: контейнер БД еще не готов.

**Решение**:

```bash
docker compose ps
# дождаться статуса "healthy"
go test ./tests -v
```

### `go test` использует кешированные результаты

**Решение**:

```bash
go clean -testcache
go test ./tests -v
```

## Ограничения

- Необработанные транзакции не подхватываются автоматически после перезапуска приложения: для этого нужен стартовый `RecoverPending` (задание этого не требовало)
- Поле `processed` при `false` отсутствует в JSON из-за `omitempty` в модели (поведение зафиксировано тестом)
- `PUT /transactions/{id}` не пересчитывает баланс: изменение суммы или типа необработанной транзакции отразится на балансе при обработке

## Запуск

### Через Docker Compose (рекомендуется)

```bash
docker compose up --build
```

API доступен на `http://localhost:8080`, PostgreSQL — на `localhost:5432` (user: `user`, password: `pass`, database: `finops`). Миграции применяются автоматически.

Проверка:

```bash
curl http://localhost:8080/health
curl http://localhost:8080/users/1/balance
curl -X POST http://localhost:8080/transactions \
  -H "Content-Type: application/json" \
  -d '{"user_id":1,"amount":"100","type":"deposit"}'
curl http://localhost:8080/users/1/balance
```

Остановить:

```bash
docker compose down
```

### Локально (без Docker)

1. Установить зависимости:

```bash
go mod download
```

2. Подготовить PostgreSQL и применить схему:

```bash
createdb finops
psql -d finops -f migrations/init.sql
```

3. Задать переменные окружения:

```bash
export DATABASE_URL="postgres://user:pass@localhost:5432/finops?sslmode=disable"
export PORT=8080
```

4. Запустить сервер:

```bash
go run server/main.go
```

Тесты:

```bash
docker compose up -d db
go test ./tests -v
go test -race ./tests -v
```

## Стек

- Go 1.25
- PostgreSQL 15
- `github.com/jackc/pgx/v5` — драйвер и пул
- `github.com/shopspring/decimal` — деньги без потери точности
- `net/http` — HTTP-сервер, маршрутизация через `ServeMux` с шаблонами путей (Go 1.22+)
- Docker, Docker Compose

## Ссылки

- [PostgreSQL документация](https://www.postgresql.org/docs/)
- [pgx драйвер](https://github.com/jackc/pgx)
- [shopspring/decimal](https://github.com/shopspring/decimal)
- [Go стандартная библиотека](https://golang.org/pkg/)