// Пакет ledger — денежный журнал с двойной записью поверх PostgreSQL.
//
// Любая операция — транзакция из проводок на счетах; сумма проводок по каждой валюте равна нулю.
// Повтор с тем же ключом идемпотентности возвращает прежний результат, а не проводит деньги второй раз.
// Деньги — целые числа в минимальных единицах (копейки, центы): никакой плавающей точки.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUnbalanced          = errors.New("ledger: сумма проводок не равна нулю")
	ErrInsufficientFunds   = errors.New("ledger: недостаточно средств")
	ErrCurrencyMismatch    = errors.New("ledger: валюта проводок не сходится (проводки в разных валютах должны балансироваться каждая отдельно)")
	ErrUnknownAccount      = errors.New("ledger: счёт не найден")
	ErrAccountExists       = errors.New("ledger: счёт уже существует")
	ErrIdempotencyConflict = errors.New("ledger: ключ идемпотентности уже использован для другой операции")
	ErrAlreadyReversed     = errors.New("ledger: транзакция уже сторнирована")
	ErrUnknownTransaction  = errors.New("ledger: транзакция не найдена")
	ErrInvalid             = errors.New("ledger: некорректные данные")
)

// Account — счёт. Balance — текущий остаток в минимальных единицах.
type Account struct {
	ID            string
	Currency      string
	AllowNegative bool
	Balance       int64
	CreatedAt     time.Time
}

// Entry — проводка. Amount со знаком: плюс увеличивает остаток счёта, минус уменьшает.
type Entry struct {
	Account string
	Amount  int64
}

// Transfer — транзакция к проведению. Key обязателен: это ключ идемпотентности.
type Transfer struct {
	Key         string
	Description string
	Entries     []Entry
}

// Posted — результат проведения.
type Posted struct {
	TxID     int64
	Replayed bool // true, если операция с этим ключом уже была проведена раньше
}

// EntryRecord — проводка в выписке.
type EntryRecord struct {
	ID           int64
	TxID         int64
	Amount       int64
	BalanceAfter int64
	Description  string
	CreatedAt    time.Time
}

// Ledger — журнал.
type Ledger struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Ledger { return &Ledger{pool: pool} }

// OpenAccount открывает счёт. allowNegative разрешает уходить в минус (внешние/системные счета).
func (l *Ledger) OpenAccount(ctx context.Context, id, currency string, allowNegative bool) error {
	_, err := l.pool.Exec(ctx, `insert into ledger_accounts (id, currency, allow_negative) values ($1, $2, $3)`, id, currency, allowNegative)
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe) && pe.Code == "23505":
		return ErrAccountExists
	case errors.As(err, &pe) && pe.Code == "23514":
		return fmt.Errorf("%w: %s", ErrInvalid, "идентификатор счёта ([A-Za-z0-9:_.-], до 64 знаков) или код валюты (три заглавные буквы)")
	}
	return err
}

// GetAccount возвращает счёт с текущим остатком.
func (l *Ledger) GetAccount(ctx context.Context, id string) (Account, error) {
	var a Account
	err := l.pool.QueryRow(ctx, `select id, currency, allow_negative, balance, created_at from ledger_accounts where id = $1`, id).
		Scan(&a.ID, &a.Currency, &a.AllowNegative, &a.Balance, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrUnknownAccount
	}
	return a, err
}

// Move — перевод суммы amount (>0) со счёта from на счёт to.
func (l *Ledger) Move(ctx context.Context, key, from, to string, amount int64, description string) (Posted, error) {
	if amount <= 0 {
		return Posted{}, fmt.Errorf("%w: сумма перевода должна быть больше нуля", ErrInvalid)
	}
	return l.Post(ctx, Transfer{Key: key, Description: description, Entries: []Entry{{from, -amount}, {to, amount}}})
}

// Post проводит транзакцию атомарно: либо все проводки и обновления остатков, либо ничего.
func (l *Ledger) Post(ctx context.Context, t Transfer) (Posted, error) {
	return l.post(ctx, t, nil)
}

// Reverse сторнирует транзакцию: проводит зеркальную транзакцию с обратными знаками.
// Одну транзакцию можно сторнировать только один раз.
func (l *Ledger) Reverse(ctx context.Context, txID int64, key, description string) (Posted, error) {
	rows, err := l.pool.Query(ctx, `select account_id, amount from ledger_entries where tx_id = $1 order by id`, txID)
	if err != nil {
		return Posted{}, err
	}
	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Account, &e.Amount); err != nil {
			rows.Close()
			return Posted{}, err
		}
		e.Amount = -e.Amount
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Posted{}, err
	}
	if len(entries) == 0 {
		return Posted{}, ErrUnknownTransaction
	}
	return l.post(ctx, Transfer{Key: key, Description: description, Entries: entries}, &txID)
}

// fingerprint — отпечаток содержимого операции: повтор с тем же ключом, но другим содержимым — ошибка.
func fingerprint(t Transfer, reverses *int64) string {
	es := append([]Entry(nil), t.Entries...)
	sort.Slice(es, func(i, j int) bool { return es[i].Account < es[j].Account })
	var b strings.Builder
	for _, e := range es {
		b.WriteString(e.Account + ":" + strconv.FormatInt(e.Amount, 10) + ";")
	}
	b.WriteString("|" + t.Description)
	if reverses != nil {
		b.WriteString("|rev:" + strconv.FormatInt(*reverses, 10))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func validate(t Transfer) error {
	if strings.TrimSpace(t.Key) == "" {
		return fmt.Errorf("%w: нужен ключ идемпотентности", ErrInvalid)
	}
	if len(t.Entries) < 2 {
		return fmt.Errorf("%w: нужно минимум две проводки", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, e := range t.Entries {
		if e.Amount == 0 {
			return fmt.Errorf("%w: проводка на нулевую сумму", ErrInvalid)
		}
		if seen[e.Account] {
			return fmt.Errorf("%w: счёт %q встречается дважды (объедините проводки)", ErrInvalid, e.Account)
		}
		seen[e.Account] = true
	}
	return nil
}

func (l *Ledger) post(ctx context.Context, t Transfer, reverses *int64) (Posted, error) {
	if err := validate(t); err != nil {
		return Posted{}, err
	}
	fp := fingerprint(t, reverses)

	// Счета блокируются всегда в одном порядке (по id): параллельные переводы A→B и B→A не взаимоблокируются.
	entries := append([]Entry(nil), t.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Account < entries[j].Account })

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return Posted{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 1. Ключ идемпотентности. Параллельный дубль ждёт здесь фиксации первого и затем видит его результат.
	var txID int64
	err = tx.QueryRow(ctx, `
		insert into ledger_transactions (idempotency_key, fingerprint, description, reverses)
		values ($1, $2, $3, $4)
		on conflict (idempotency_key) do nothing
		returning id`, t.Key, fp, t.Description, reverses).Scan(&txID)
	if errors.Is(err, pgx.ErrNoRows) {
		var oldID int64
		var oldFP string
		if err := tx.QueryRow(ctx, `select id, fingerprint from ledger_transactions where idempotency_key = $1`, t.Key).Scan(&oldID, &oldFP); err != nil {
			return Posted{}, err
		}
		if oldFP != fp {
			return Posted{}, ErrIdempotencyConflict
		}
		return Posted{TxID: oldID, Replayed: true}, nil
	}
	if err != nil {
		return Posted{}, mapErr(err)
	}

	// 2. Блокируем счета в порядке id и проверяем валюты.
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.Account
	}
	rows, err := tx.Query(ctx, `select id, currency from ledger_accounts where id = any($1) order by id for update`, ids)
	if err != nil {
		return Posted{}, err
	}
	currency := map[string]string{}
	for rows.Next() {
		var id, cur string
		if err := rows.Scan(&id, &cur); err != nil {
			rows.Close()
			return Posted{}, err
		}
		currency[id] = cur
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Posted{}, err
	}
	sums := map[string]int64{}
	for _, e := range entries {
		cur, ok := currency[e.Account]
		if !ok {
			return Posted{}, fmt.Errorf("%w: %q", ErrUnknownAccount, e.Account)
		}
		next, ok := addChecked(sums[cur], e.Amount)
		if !ok {
			return Posted{}, fmt.Errorf("%w: сумма проводок выходит за пределы int64", ErrInvalid)
		}
		sums[cur] = next
	}
	for _, s := range sums {
		if s != 0 {
			if len(sums) > 1 {
				return Posted{}, ErrCurrencyMismatch // валюты смешаны без обменных проводок
			}
			return Posted{}, ErrUnbalanced
		}
	}

	// 3. Остатки и проводки. Недостаток средств сработает как CHECK в самой базе.
	for _, e := range entries {
		var after int64
		if err := tx.QueryRow(ctx, `update ledger_accounts set balance = balance + $2 where id = $1 returning balance`, e.Account, e.Amount).Scan(&after); err != nil {
			return Posted{}, mapErr(err)
		}
		if _, err := tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, currency, amount, balance_after) values ($1, $2, $3, $4, $5)`,
			txID, e.Account, currency[e.Account], e.Amount, after); err != nil {
			return Posted{}, mapErr(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Posted{}, mapErr(err)
	}
	return Posted{TxID: txID}, nil
}

// addChecked складывает без молчаливого переполнения.
func addChecked(a, b int64) (int64, bool) {
	c := a + b
	if (b > 0 && c < a) || (b < 0 && c > a) {
		return 0, false
	}
	return c, true
}

// mapErr переводит нарушения ограничений базы в ошибки пакета.
func mapErr(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.ConstraintName {
	case "balance_non_negative":
		return ErrInsufficientFunds
	case "ledger_balanced":
		return ErrUnbalanced
	case "ledger_reverses_once":
		return ErrAlreadyReversed
	case "ledger_idempotency":
		return ErrIdempotencyConflict
	}
	if pe.Code == "22003" { // bigint вне диапазона: остаток счёта вышел бы за int64
		return fmt.Errorf("%w: остаток счёта выходит за пределы int64", ErrInvalid)
	}
	if pe.Code == "23503" { // нарушение внешнего ключа: нет счёта или валюта не совпала
		return ErrCurrencyMismatch
	}
	return err
}

// Entries возвращает выписку по счёту: новые проводки первыми. beforeID=0 — с самой последней.
func (l *Ledger) Entries(ctx context.Context, account string, limit int, beforeID int64) ([]EntryRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if _, err := l.GetAccount(ctx, account); err != nil {
		return nil, err
	}
	rows, err := l.pool.Query(ctx, `
		select e.id, e.tx_id, e.amount, e.balance_after, t.description, t.created_at
		from ledger_entries e join ledger_transactions t on t.id = e.tx_id
		where e.account_id = $1 and ($3 = 0 or e.id < $3)
		order by e.id desc limit $2`, account, limit, beforeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryRecord
	for rows.Next() {
		var r EntryRecord
		if err := rows.Scan(&r.ID, &r.TxID, &r.Amount, &r.BalanceAfter, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Problem — найденное расхождение при сверке.
type Problem struct {
	Kind   string // balance-drift | unbalanced-transaction | running-balance
	Detail string
}

// Verify пересчитывает всё из проводок и сравнивает с хранимыми остатками. Пустой результат — журнал согласован.
// Это независимая проверка поверх ограничений базы: она ловит и обход ограничений (например, правки суперпользователем).
func (l *Ledger) Verify(ctx context.Context) ([]Problem, error) {
	var out []Problem
	collect := func(kind, sql string, scan func(pgx.Rows) (string, error)) error {
		rows, err := l.pool.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scan(rows)
			if err != nil {
				return err
			}
			out = append(out, Problem{Kind: kind, Detail: d})
		}
		return rows.Err()
	}

	if err := collect("balance-drift", `
		select a.id, a.balance, coalesce(sum(e.amount), 0)::bigint
		from ledger_accounts a left join ledger_entries e on e.account_id = a.id
		group by a.id, a.balance having a.balance <> coalesce(sum(e.amount), 0)
		order by a.id`, func(r pgx.Rows) (string, error) {
		var id string
		var stored, computed int64
		err := r.Scan(&id, &stored, &computed)
		return fmt.Sprintf("счёт %s: хранится %d, по проводкам %d", id, stored, computed), err
	}); err != nil {
		return nil, err
	}
	if err := collect("unbalanced-transaction", `
		select tx_id, currency, sum(amount)::bigint from ledger_entries
		group by tx_id, currency having sum(amount) <> 0 order by tx_id`, func(r pgx.Rows) (string, error) {
		var tx, sum int64
		var cur string
		err := r.Scan(&tx, &cur, &sum)
		return fmt.Sprintf("транзакция %d: сумма %s = %d", tx, cur, sum), err
	}); err != nil {
		return nil, err
	}
	if err := collect("running-balance", `
		select id, account_id, balance_after, running from (
			select id, account_id, balance_after, sum(amount) over (partition by account_id order by id)::bigint as running
			from ledger_entries) s
		where balance_after <> running order by id limit 100`, func(r pgx.Rows) (string, error) {
		var id, after, running int64
		var acc string
		err := r.Scan(&id, &acc, &after, &running)
		return fmt.Sprintf("проводка %d (счёт %s): остаток после %d, по накопленной сумме %d", id, acc, after, running), err
	}); err != nil {
		return nil, err
	}
	return out, nil
}
