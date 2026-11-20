# ledger-double-entry

[![CI](https://github.com/Oleg4311/ledger-double-entry/actions/workflows/ci.yml/badge.svg)](https://github.com/Oleg4311/ledger-double-entry/actions/workflows/ci.yml)

**Денежный журнал с двойной записью на PostgreSQL: Go-библиотека `ledger` и HTTP-сервис `ledgerd`.**
Любая операция — транзакция из проводок на счетах; сумма проводок по каждой валюте равна нулю. Повтор запроса с тем же ключом идемпотентности не проведёт деньги дважды. Главные правила держит **сама база данных**, а не только код.

*English: a double-entry money ledger on PostgreSQL (Go library + HTTP service). Invariants — balanced transactions, no negative balances, immutable history — are enforced by the database itself; idempotency keys, reversals, reconciliation, and a model-based property test that compares the real ledger with an in-memory model. See the [English section](#english).*

## Зачем это нужно

Кошельки, комиссии, холды, внутренние счета маркетплейса — всё это устроено как «перевод между счетами». Ошибки здесь стоят денег: двойное списание из-за повтора запроса, «потерянная» половина операции, баланс в минусе, правка задним числом. Проект собирает минимальное ядро, которое этого не допускает, и доказывает это тестами.

## Модель

- **Счёт** (`ledger_accounts`): идентификатор, валюта, остаток в минимальных единицах (копейки, центы; `bigint`, без плавающей точки) и флаг `allow_negative` (для системных и внешних счетов вроде «банк», которым положено быть в минусе).
- **Транзакция** (`ledger_transactions`): ключ идемпотентности, отпечаток содержимого, описание.
- **Проводка** (`ledger_entries`): счёт и сумма со знаком (плюс — приход, минус — расход), остаток после неё.
- **Обмен валют** — транзакция из четырёх проводок: каждая валюта балансируется отдельно, а смешивать валюты без обменных проводок нельзя.

## Что гарантирует база данных

Эти правила работают, даже если обойти код пакета и писать SQL напрямую (и это проверяется тестом):

| Правило | Как держится |
|---|---|
| Сумма проводок транзакции по каждой валюте равна нулю | отложенный триггер `ledger_balanced`: проверка при `COMMIT`, когда вставлены все строки |
| Остаток не уходит в минус, если счёт этого не разрешает | `CHECK balance_non_negative` |
| История неизменяема | триггеры запрещают `UPDATE` и `DELETE` проводок и транзакций |
| Валюта проводки совпадает с валютой счёта | составной внешний ключ `(account_id, currency)` |
| Один ключ идемпотентности — одна операция; одну транзакцию сторнируют один раз | уникальные ограничения `ledger_idempotency` и `ledger_reverses_once` |

Код пакета добавляет то, что на уровне SQL неудобно: порядок блокировок, идемпотентные повторы, понятные ошибки.

## Использование

```go
pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
ledger.Migrate(ctx, pool)
l := ledger.New(pool)

l.OpenAccount(ctx, "bank", "RUB", true)   // системный счёт: можно в минус
l.OpenAccount(ctx, "alice", "RUB", false)
l.OpenAccount(ctx, "bob", "RUB", false)

l.Move(ctx, "fund-42", "bank", "alice", 10_000, "пополнение")   // 100,00 ₽
p, err := l.Move(ctx, "pay-7", "alice", "bob", 2_550, "за кофе")
// повтор с тем же ключом: p.Replayed == true, деньги не двигаются

// несколько проводок: комиссия и два получателя
l.Post(ctx, ledger.Transfer{Key: "order-9", Entries: []ledger.Entry{
    {Account: "alice", Amount: -1_100}, {Account: "bob", Amount: 1_000}, {Account: "fees", Amount: 100},
}})

l.Reverse(ctx, p.TxID, "pay-7-refund", "возврат")   // сторно: зеркальная транзакция
problems, _ := l.Verify(ctx)                          // независимая сверка остатков с проводками
```

Ошибки — значения для `errors.Is`: `ErrInsufficientFunds`, `ErrUnbalanced`, `ErrCurrencyMismatch`, `ErrUnknownAccount`, `ErrIdempotencyConflict` (тот же ключ, другое содержимое), `ErrAlreadyReversed`, `ErrInvalid`.

## HTTP-сервис

```bash
docker compose up -d        # PostgreSQL (compose.yml в репозитории)
export DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable"
ledgerd --migrate           # слушает 127.0.0.1:8080
```

```bash
curl -X POST localhost:8080/v1/accounts -d '{"id":"bank","currency":"RUB","allow_negative":true}'
curl -X POST localhost:8080/v1/accounts -d '{"id":"alice","currency":"RUB"}'
curl -X POST localhost:8080/v1/transfers -H 'Idempotency-Key: fund-1' \
  -d '{"description":"пополнение","entries":[{"account":"bank","amount":-5000},{"account":"alice","amount":5000}]}'
# повтор того же запроса → 200 {"replayed":true}, а не второе списание
curl localhost:8080/v1/accounts/alice            # остаток
curl localhost:8080/v1/accounts/alice/entries    # выписка, новые первыми
curl localhost:8080/v1/verify                    # сверка журнала
```

Статусы: 201 проведено, 200 повтор, 400 неверный запрос (в том числе не сходится баланс), 404 нет счёта или транзакции, 409 конфликт (ключ с другим содержимым, счёт уже есть, уже сторнировано), 422 недостаточно средств. Полное описание — [docs/openapi.yaml](docs/openapi.yaml); тест следит, чтобы описание и реальные маршруты не расходились. Бинарники для Linux, macOS и Windows — на странице [Releases](https://github.com/Oleg4311/ledger-double-entry/releases).

## Что проверено

- **Модельный тест (property-based, [rapid](https://github.com/flyingmutant/rapid)).** Случайные последовательности операций (переводы, переводы на двоих, несбалансированные, сторно, повторы ключей из маленького пула) выполняются и в настоящем журнале, и в простой модели в памяти. После **каждого** шага остатки и классы ошибок должны совпасть; в конце — сверка `Verify`. 100 сценариев локально, 300 в CI.
- **Параллельная нагрузка:** 16 горутин × 12 счетов с повторами ключей — деньги не создаются и не пропадают (сумма всех остатков равна нулю), никто не ушёл в минус. 40 одновременных запросов с одним ключом проводят деньги **ровно один раз** (один новый результат и 39 повторов); то же по HTTP.
- **Взаимоблокировки:** встречные переводы A→B и B→A (1200 штук параллельно) идут без ошибок: счета блокируются всегда в порядке id.
- **Обход библиотеки:** прямой SQL не проводит несбалансированную транзакцию, не меняет и не удаляет историю, не уводит счёт в минус и не вставляет проводку в чужой валюте.
- **Границы:** суммы у `int64` (`MaxInt64 + MaxInt64 + 2` — это «ноль» при молчаливом переполнении) отвергаются и в коде, и независимо в базе.
- **Мутационная проверка:** убирали упорядочивание блокировок (тест ловит `SQLSTATE 40P01`), проверку конфликта ключа, пометку сторно, `CHECK` на баланс, балансирующий триггер — тесты падают.
- **Сверка (`Verify`)** находит порчу данных в обход ограничений: тест отключает триггеры, правит проводку и убеждается, что найдены расхождения остатков, несбалансированная транзакция и неверный накопленный остаток.
- CI запускает всё с `-race` на PostgreSQL 14, 16 и 17 (service container), сквозной прогон настоящего сервиса по HTTP, бенчмарк, а также тесты на Linux, Windows и macOS со встроенным Postgres.

## Производительность

`go test -bench Move` (переводы между 200 счетами, 12 параллельных клиентов; ноутбук разработчика, Windows, встроенный Postgres без тюнинга, база и клиенты на одной машине): **≈ 4 400 переводов/с**. Каждый перевод — одна транзакция БД с блокировкой счетов, двумя обновлениями остатков, двумя проводками и отложенной проверкой баланса. Тот же бенчмарк на общих раннерах GitHub Actions (4 vCPU, база и клиенты на одной машине, один запуск, без усреднения):

| PostgreSQL | переводов/с |
|---|---|
| 14 | ≈ 1 400 |
| 16 | ≈ 1 550 |
| 17 | ≈ 3 150 |

Разброс между версиями — в основном шум общих раннеров, а не свойство версий Postgres. Это нижняя оценка для неотдельной БД, а не обещание: измерьте на своём железе. Цифры каждого запуска CI сохраняются в артефактах (`bench-pg*`).

## Честные ограничения

- **В `ledgerd` нет аутентификации и авторизации.** Сервис рассчитан на внутреннюю сеть за вашим шлюзом; по умолчанию слушает только `127.0.0.1`.
- **Идемпотентность — по ключу и содержимому.** Тот же ключ с другой суммой или описанием — `409`, а не молчаливая замена. Ключ хранится вечно; чистить его нельзя, не потеряв гарантию.
- **Горячие счета.** Каждый перевод блокирует строки счетов, поэтому тысячи операций в секунду на один и тот же счёт (например, общий счёт комиссий) выстроятся в очередь. Типичное решение — делить такой счёт на несколько подсчётов.
- **Нет холдов, ограничений по времени, комиссий и курсов валют:** только ядро (счета, проводки, сторно, сверка). Обменные курсы вы считаете сами и передаёте готовыми проводками.
- **Отложенная проверка баланса срабатывает на каждую строку** (запрос по транзакции), то есть для транзакции из N проводок это N небольших запросов; для типичных 2–4 проводок это незаметно, для сотен проводок в одной транзакции — нет.
- **Сверка читает всю таблицу проводок.** `Verify` — инструмент для регулярной проверки или диагностики, а не для каждого запроса.

## Разработка

```bash
go test ./...                              # без DATABASE_URL поднимется встроенный Postgres 16
DATABASE_URL=... go test -race ./...       # на своём Postgres
go test -run TestLedgerMatchesModel ./ledger/ -rapid.checks=1000   # больше случайных сценариев
go test -run xxx -bench Move -benchtime 5000x ./ledger/
bash scripts/smoke.sh                      # сквозной прогон сервиса (нужен DATABASE_URL)
```

Релизы (`v*`) собираются из тега и содержат контрольные суммы и подтверждение происхождения: `gh attestation verify ledgerd-v0.1.1-linux-amd64.tar.gz --repo Oleg4311/ledger-double-entry`.

Лицензия: MIT. Сделано с помощью Claude.

---

## English

**ledger-double-entry** is a double-entry ledger for money on PostgreSQL: a Go library (`ledger`) and an HTTP service (`ledgerd`).

- **The database enforces the invariants**: a deferred constraint trigger rejects transactions whose entries do not sum to zero per currency; a `CHECK` prevents negative balances; triggers make history immutable; a composite foreign key ties each entry to its account's currency. A test bypasses the library with raw SQL to prove it.
- **Idempotency keys** with a content fingerprint (same key + different content = `409`), **reversals** (once per transaction), accounts locked in id order so opposite transfers never deadlock.
- **Verification**: a model-based property test (rapid) runs random operation sequences against the real ledger and an in-memory model and compares balances and error classes after every step; concurrency tests (40 duplicate requests post once; money is conserved under 16 concurrent workers); `Verify` reconciles balances from entries and catches tampering. CI runs `-race` on PostgreSQL 14/16/17.
- ≈ 4,400 transfers/s locally, 1,400–3,200 on shared CI runners (see "Производительность"); no auth in `ledgerd` — run it behind your gateway.

MIT licensed. Built with Claude.
