package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Oleg4311/ledger-double-entry/internal/pgtest"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	pool, stop, err := pgtest.Start(ctx, "ledger_test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "не удалось подготовить Postgres:", err)
		os.Exit(1)
	}
	testPool = pool
	if err := Migrate(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "миграция:", err)
		stop()
		os.Exit(1)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

func fresh(t testing.TB) (context.Context, *Ledger) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if _, err := testPool.Exec(ctx, `truncate ledger_entries, ledger_transactions, ledger_accounts restart identity cascade`); err != nil {
		t.Fatal(err)
	}
	return ctx, New(testPool)
}

func must(t tb, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func open(t tb, l *Ledger, ctx context.Context, id, cur string, neg bool) {
	t.Helper()
	must(t, l.OpenAccount(ctx, id, cur, neg))
}

func balance(t tb, l *Ledger, ctx context.Context, id string) int64 {
	t.Helper()
	a, err := l.GetAccount(ctx, id)
	must(t, err)
	return a.Balance
}

func noProblems(t tb, l *Ledger, ctx context.Context) {
	t.Helper()
	p, err := l.Verify(ctx)
	must(t, err)
	if len(p) != 0 {
		t.Fatalf("сверка нашла расхождения: %+v", p)
	}
}

func TestMoveAndStatement(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "alice", "RUB", false)
	open(t, l, ctx, "bob", "RUB", false)

	_, err := l.Move(ctx, "fund-1", "bank", "alice", 10_000, "пополнение")
	must(t, err)
	_, err = l.Move(ctx, "pay-1", "alice", "bob", 2_550, "за кофе")
	must(t, err)

	if got := balance(t, l, ctx, "alice"); got != 7_450 {
		t.Fatalf("alice: %d", got)
	}
	if got := balance(t, l, ctx, "bob"); got != 2_550 {
		t.Fatalf("bob: %d", got)
	}
	if got := balance(t, l, ctx, "bank"); got != -10_000 {
		t.Fatalf("bank: %d", got)
	}

	st, err := l.Entries(ctx, "alice", 10, 0)
	must(t, err)
	if len(st) != 2 || st[0].Amount != -2_550 || st[0].BalanceAfter != 7_450 || st[0].Description != "за кофе" || st[1].Amount != 10_000 || st[1].BalanceAfter != 10_000 {
		t.Fatalf("выписка: %+v", st)
	}
	older, err := l.Entries(ctx, "alice", 10, st[0].ID)
	must(t, err)
	if len(older) != 1 || older[0].ID != st[1].ID {
		t.Fatalf("постраничная выписка: %+v", older)
	}
	noProblems(t, l, ctx)
}

func TestInvalidInputIsRejectedBeforeTouchingMoney(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "a", "RUB", true)
	open(t, l, ctx, "b", "RUB", true)
	cases := map[string]Transfer{
		"нет ключа":      {Entries: []Entry{{"a", -1}, {"b", 1}}},
		"одна проводка":  {Key: "k", Entries: []Entry{{"a", 0}}},
		"нулевая сумма":  {Key: "k", Entries: []Entry{{"a", 0}, {"b", 0}}},
		"счёт дважды":    {Key: "k", Entries: []Entry{{"a", -1}, {"a", 1}}},
		"пустой перевод": {Key: "k"},
	}
	for name, tr := range cases {
		if _, err := l.Post(ctx, tr); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ожидали ErrInvalid, получили %v", name, err)
		}
	}
	if _, err := l.Move(ctx, "k", "a", "b", 0, ""); !errors.Is(err, ErrInvalid) {
		t.Error("перевод нулевой суммы должен отвергаться")
	}
	if _, err := l.Move(ctx, "k", "a", "b", -5, ""); !errors.Is(err, ErrInvalid) {
		t.Error("перевод отрицательной суммы должен отвергаться")
	}
	if err := l.OpenAccount(ctx, "плохой id", "RUB", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("некорректный id счёта: %v", err)
	}
	if err := l.OpenAccount(ctx, "ok", "rub", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("некорректная валюта: %v", err)
	}
	if err := l.OpenAccount(ctx, "a", "RUB", false); !errors.Is(err, ErrAccountExists) {
		t.Errorf("повторное открытие: %v", err)
	}
	var n int
	must(t, testPool.QueryRow(ctx, `select count(*) from ledger_transactions`).Scan(&n))
	if n != 0 {
		t.Fatalf("после отвергнутых операций транзакций быть не должно, а их %d", n)
	}
}

func TestUnbalancedAndUnknownAccountLeaveNoTrace(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "a", "RUB", true)
	open(t, l, ctx, "b", "RUB", true)

	if _, err := l.Post(ctx, Transfer{Key: "u1", Entries: []Entry{{"a", -100}, {"b", 99}}}); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("ожидали ErrUnbalanced, получили %v", err)
	}
	if _, err := l.Post(ctx, Transfer{Key: "u2", Entries: []Entry{{"a", -100}, {"ghost", 100}}}); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("ожидали ErrUnknownAccount, получили %v", err)
	}
	if balance(t, l, ctx, "a") != 0 || balance(t, l, ctx, "b") != 0 {
		t.Fatal("балансы не должны были измениться")
	}
	var n int
	must(t, testPool.QueryRow(ctx, `select count(*) from ledger_transactions`).Scan(&n))
	if n != 0 {
		t.Fatalf("отвергнутая операция не должна оставлять транзакцию, а их %d", n)
	}
	// ключ освобождается: после отката его можно использовать для корректной операции
	if _, err := l.Move(ctx, "u1", "a", "b", 100, ""); err != nil {
		t.Fatalf("ключ после отката должен быть свободен: %v", err)
	}
}

func TestInsufficientFundsRollsBackEveryLeg(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "rich", "RUB", false)
	open(t, l, ctx, "poor", "RUB", false)
	open(t, l, ctx, "sink", "RUB", false)
	_, err := l.Move(ctx, "f1", "bank", "rich", 1000, "")
	must(t, err)

	// rich отдаёт 400 (можно), poor отдаёт 50 (нечего): вся операция должна откатиться
	_, err = l.Post(ctx, Transfer{Key: "multi", Entries: []Entry{{"rich", -400}, {"poor", -50}, {"sink", 450}}})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("ожидали ErrInsufficientFunds, получили %v", err)
	}
	if got := balance(t, l, ctx, "rich"); got != 1000 {
		t.Fatalf("rich должен остаться при 1000, а у него %d", got)
	}
	if balance(t, l, ctx, "sink") != 0 {
		t.Fatal("sink не должен получить деньги")
	}
	noProblems(t, l, ctx)
}

func TestMultiCurrency(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "fx-rub", "RUB", true)
	open(t, l, ctx, "fx-usd", "USD", true)
	open(t, l, ctx, "ivan-rub", "RUB", false)
	open(t, l, ctx, "ivan-usd", "USD", false)
	_, err := l.Move(ctx, "seed", "fx-rub", "ivan-rub", 9_000_00, "")
	must(t, err)

	// обмен: четыре проводки, каждая валюта балансируется отдельно
	_, err = l.Post(ctx, Transfer{Key: "exchange-1", Description: "обмен 90 000 ₽ на 1 000 $", Entries: []Entry{
		{"ivan-rub", -90_000}, {"fx-rub", 90_000}, {"fx-usd", -1_000_00}, {"ivan-usd", 1_000_00},
	}})
	must(t, err)
	if balance(t, l, ctx, "ivan-usd") != 1_000_00 || balance(t, l, ctx, "ivan-rub") != 9_000_00-90_000 {
		t.Fatal("обмен проведён неверно")
	}

	// деньги из одной валюты в другую без обменных проводок проводить нельзя
	_, err = l.Post(ctx, Transfer{Key: "bad-fx", Entries: []Entry{{"ivan-rub", -100}, {"ivan-usd", 100}}})
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("ожидали ErrCurrencyMismatch, получили %v", err)
	}
	noProblems(t, l, ctx)
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "a", "RUB", false)

	first, err := l.Move(ctx, "k1", "bank", "a", 500, "x")
	must(t, err)
	if first.Replayed {
		t.Fatal("первая операция не может быть повтором")
	}
	again, err := l.Move(ctx, "k1", "bank", "a", 500, "x")
	must(t, err)
	if !again.Replayed || again.TxID != first.TxID {
		t.Fatalf("повтор должен вернуть ту же транзакцию: %+v / %+v", again, first)
	}
	if balance(t, l, ctx, "a") != 500 {
		t.Fatal("повтор не должен проводить деньги второй раз")
	}
	if _, err := l.Move(ctx, "k1", "bank", "a", 501, "x"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("тот же ключ с другой суммой: %v", err)
	}
	if _, err := l.Move(ctx, "k1", "bank", "a", 500, "другое описание"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("тот же ключ с другим описанием: %v", err)
	}
	if balance(t, l, ctx, "a") != 500 {
		t.Fatal("конфликт не должен менять балансы")
	}
}

// Двойной клик, повтор вебхука: 40 одновременных запросов с одним ключом проводят деньги ровно один раз.
func TestConcurrentDuplicateRequestsPostOnce(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "a", "RUB", false)

	var wg sync.WaitGroup
	ids := make(chan Posted, 40)
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := l.Move(ctx, "same-key", "bank", "a", 700, "")
			if err != nil {
				errs <- err
				return
			}
			ids <- p
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first int64
	fresh, replays := 0, 0
	for p := range ids {
		if first == 0 {
			first = p.TxID
		}
		if p.TxID != first {
			t.Fatalf("разные транзакции для одного ключа: %d и %d", first, p.TxID)
		}
		if p.Replayed {
			replays++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replays != 39 {
		t.Fatalf("новых %d, повторов %d; ожидали 1 и 39", fresh, replays)
	}
	if balance(t, l, ctx, "a") != 700 {
		t.Fatalf("баланс %d, ожидали 700", balance(t, l, ctx, "a"))
	}
	noProblems(t, l, ctx)
}

func TestReverse(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "a", "RUB", false)
	open(t, l, ctx, "b", "RUB", false)
	_, err := l.Move(ctx, "f", "bank", "a", 1000, "")
	must(t, err)
	pay, err := l.Move(ctx, "pay", "a", "b", 300, "заказ")
	must(t, err)

	rev, err := l.Reverse(ctx, pay.TxID, "pay-rev", "возврат")
	must(t, err)
	if balance(t, l, ctx, "a") != 1000 || balance(t, l, ctx, "b") != 0 {
		t.Fatal("сторно должно вернуть остатки")
	}
	again, err := l.Reverse(ctx, pay.TxID, "pay-rev", "возврат")
	must(t, err)
	if !again.Replayed || again.TxID != rev.TxID {
		t.Fatalf("повтор сторно с тем же ключом: %+v", again)
	}
	if _, err := l.Reverse(ctx, pay.TxID, "pay-rev-2", "ещё раз"); !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("второе сторно другим ключом: %v", err)
	}
	if _, err := l.Reverse(ctx, 999999, "ghost", ""); !errors.Is(err, ErrUnknownTransaction) {
		t.Fatalf("сторно несуществующей: %v", err)
	}
	// сторно сторно возвращает исходное состояние
	_, err = l.Reverse(ctx, rev.TxID, "rev-of-rev", "")
	must(t, err)
	if balance(t, l, ctx, "a") != 700 || balance(t, l, ctx, "b") != 300 {
		t.Fatal("сторно сторно должно восстановить платёж")
	}
	noProblems(t, l, ctx)
}

func TestReverseFailsWhenMoneyIsGone(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "a", "RUB", false)
	open(t, l, ctx, "b", "RUB", false)
	open(t, l, ctx, "shop", "RUB", false)
	_, err := l.Move(ctx, "f", "bank", "a", 100, "")
	must(t, err)
	pay, err := l.Move(ctx, "pay", "a", "b", 100, "")
	must(t, err)
	_, err = l.Move(ctx, "spend", "b", "shop", 100, "")
	must(t, err)
	if _, err := l.Reverse(ctx, pay.TxID, "rev", ""); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("сторно при потраченных деньгах: %v", err)
	}
	// неудачное сторно не «сжигает» возможность сторнировать позже
	_, err = l.Move(ctx, "refill", "bank", "b", 100, "")
	must(t, err)
	if _, err := l.Reverse(ctx, pay.TxID, "rev2", ""); err != nil {
		t.Fatalf("после пополнения сторно должно пройти: %v", err)
	}
	noProblems(t, l, ctx)
}

// База сама защищает инварианты, даже если обойти код пакета и писать SQL напрямую.
func TestDatabaseEnforcesInvariantsWithoutTheLibrary(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "a", "RUB", false)
	open(t, l, ctx, "usd", "USD", false)
	_, err := l.Move(ctx, "f", "bank", "a", 100, "")
	must(t, err)

	constraint := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.ConstraintName
		}
		return fmt.Sprint(err)
	}

	t.Run("несбалансированная транзакция не фиксируется", func(t *testing.T) {
		tx, err := testPool.Begin(ctx)
		must(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		var id int64
		must(t, tx.QueryRow(ctx, `insert into ledger_transactions (idempotency_key, fingerprint) values ('raw', 'x') returning id`).Scan(&id))
		_, err = tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, currency, amount, balance_after) values ($1, 'a', 'RUB', 5, 105)`, id)
		must(t, err) // сама вставка проходит: проверка отложена до COMMIT
		if got := constraint(tx.Commit(ctx)); got != "ledger_balanced" {
			t.Fatalf("COMMIT должен упасть на ledger_balanced, а упал на %q", got)
		}
	})
	t.Run("проводки нельзя менять и удалять", func(t *testing.T) {
		if got := constraint(execErr(ctx, `update ledger_entries set amount = amount + 1`)); got != "ledger_immutable" {
			t.Fatalf("UPDATE проводок: %q", got)
		}
		if got := constraint(execErr(ctx, `delete from ledger_entries`)); got != "ledger_immutable" {
			t.Fatalf("DELETE проводок: %q", got)
		}
		if got := constraint(execErr(ctx, `update ledger_transactions set description = 'подмена'`)); got != "ledger_immutable" {
			t.Fatalf("UPDATE транзакций: %q", got)
		}
		if got := constraint(execErr(ctx, `delete from ledger_transactions`)); got != "ledger_immutable" {
			t.Fatalf("DELETE транзакций: %q", got)
		}
	})
	t.Run("баланс без права на минус не может стать отрицательным", func(t *testing.T) {
		if got := constraint(execErr(ctx, `update ledger_accounts set balance = -1 where id = 'a'`)); got != "balance_non_negative" {
			t.Fatalf("баланс в минус: %q", got)
		}
	})
	t.Run("валюта проводки должна совпадать с валютой счёта", func(t *testing.T) {
		tx, err := testPool.Begin(ctx)
		must(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		var id int64
		must(t, tx.QueryRow(ctx, `insert into ledger_transactions (idempotency_key, fingerprint) values ('raw2', 'x') returning id`).Scan(&id))
		_, err = tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, currency, amount, balance_after) values ($1, 'usd', 'RUB', 5, 5)`, id)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23503" {
			t.Fatalf("ожидали нарушение внешнего ключа, получили %v", err)
		}
	})
	noProblems(t, l, ctx)
}

func execErr(ctx context.Context, sql string) error {
	_, err := testPool.Exec(ctx, sql)
	return err
}

// Сверка должна находить порчу данных, сделанную в обход ограничений (например, суперпользователем).
func TestVerifyDetectsTampering(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "a", "RUB", false)
	_, err := l.Move(ctx, "f1", "bank", "a", 100, "")
	must(t, err)
	_, err = l.Move(ctx, "f2", "bank", "a", 50, "")
	must(t, err)
	noProblems(t, l, ctx)

	conn, err := testPool.Acquire(ctx)
	must(t, err)
	defer conn.Release()
	for _, sql := range []string{
		`alter table ledger_entries disable trigger user`,
		`update ledger_entries set amount = amount + 7 where id = (select min(id) from ledger_entries where account_id = 'a')`,
		`alter table ledger_entries enable trigger user`,
	} {
		_, err := conn.Exec(ctx, sql)
		must(t, err)
	}
	probs, err := l.Verify(ctx)
	must(t, err)
	kinds := map[string]bool{}
	for _, p := range probs {
		kinds[p.Kind] = true
	}
	for _, want := range []string{"balance-drift", "unbalanced-transaction", "running-balance"} {
		if !kinds[want] {
			t.Errorf("сверка не нашла %q; нашла %+v", want, probs)
		}
	}
}

// Суммы у границы int64: ни молчаливого переполнения в коде, ни «денег из воздуха» в базе.
func TestHugeAmountsCannotMintMoney(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "x", "RUB", true)
	open(t, l, ctx, "y", "RUB", true)
	open(t, l, ctx, "z", "RUB", true)
	const max = int64(9223372036854775807)

	// MaxInt64 + MaxInt64 + 2 = 2^64: при молчаливом переполнении это «ноль» и операция прошла бы
	_, err := l.Post(ctx, Transfer{Key: "mint", Entries: []Entry{{"x", max}, {"y", max}, {"z", 2}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("ожидали ErrInvalid, получили %v", err)
	}
	for _, id := range []string{"x", "y", "z"} {
		if balance(t, l, ctx, id) != 0 {
			t.Fatalf("счёт %s изменился", id)
		}
	}
	// остаток, который вышел бы за int64, — отказ, а не «упавший» запрос
	_, err = l.Post(ctx, Transfer{Key: "big1", Entries: []Entry{{"x", max}, {"y", -max}}})
	must(t, err)
	_, err = l.Post(ctx, Transfer{Key: "big2", Entries: []Entry{{"x", 1}, {"z", -1}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("переполнение остатка: ожидали ErrInvalid, получили %v", err)
	}
	// вторая линия защиты: база считает сумму точно, поэтому и прямой SQL не проведёт «деньги из воздуха»
	tx, err := testPool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	var id int64
	must(t, tx.QueryRow(ctx, `insert into ledger_transactions (idempotency_key, fingerprint) values ('raw-mint', 'x') returning id`).Scan(&id))
	for _, e := range []struct {
		acc string
		amt int64
	}{{"x", max}, {"y", max}, {"z", 2}} {
		_, err = tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, currency, amount, balance_after) values ($1, $2, 'RUB', $3, 0)`, id, e.acc, e.amt)
		must(t, err)
	}
	var pe *pgconn.PgError
	if err := tx.Commit(ctx); !errors.As(err, &pe) || pe.ConstraintName != "ledger_balanced" {
		t.Fatalf("COMMIT должен упасть на ledger_balanced, получили %v", err)
	}
}

// Параллельные переводы в обе стороны между одними счетами не должны взаимоблокироваться.
func TestOppositeTransfersDoNotDeadlock(t *testing.T) {
	ctx, l := fresh(t)
	open(t, l, ctx, "bank", "RUB", true)
	open(t, l, ctx, "a", "RUB", false)
	open(t, l, ctx, "b", "RUB", false)
	_, err := l.Move(ctx, "fa", "bank", "a", 1_000_000, "")
	must(t, err)
	_, err = l.Move(ctx, "fb", "bank", "b", 1_000_000, "")
	must(t, err)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			from, to := "a", "b"
			if w%2 == 1 {
				from, to = to, from
			}
			for i := 0; i < 150; i++ {
				if _, err := l.Move(ctx, fmt.Sprintf("opp-%d-%d", w, i), from, to, 1, ""); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("встречные переводы дали ошибку (взаимоблокировка?): %v", err)
	}
	if got := balance(t, l, ctx, "a") + balance(t, l, ctx, "b"); got != 2_000_000 {
		t.Fatalf("сумма a+b = %d", got)
	}
	noProblems(t, l, ctx)
}

// Нагрузка: много параллельных переводов между 12 счетами с повторами ключей. Деньги не создаются и не пропадают.
func TestConcurrentStressConservesMoney(t *testing.T) {
	ctx, l := fresh(t)
	const accounts, start = 12, int64(1_000)
	open(t, l, ctx, "bank", "RUB", true)
	for i := 0; i < accounts; i++ {
		open(t, l, ctx, fmt.Sprintf("u%d", i), "RUB", false)
		_, err := l.Move(ctx, fmt.Sprintf("seed-%d", i), "bank", fmt.Sprintf("u%d", i), start, "")
		must(t, err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	declined := 0
	errs := make(chan error, 32)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 99))
			for i := 0; i < 120; i++ {
				a, b := rng.IntN(accounts), rng.IntN(accounts)
				if a == b {
					continue
				}
				// ключ из небольшого пула: часть запросов — настоящие повторы
				key := fmt.Sprintf("tr-%d-%d-%d", a, b, rng.IntN(25))
				amount := int64(1 + rng.IntN(400))
				_, err := l.Move(ctx, key, fmt.Sprintf("u%d", a), fmt.Sprintf("u%d", b), amount, "")
				switch {
				case err == nil:
				case errors.Is(err, ErrInsufficientFunds):
					mu.Lock()
					declined++
					mu.Unlock()
				case errors.Is(err, ErrIdempotencyConflict):
					// тот же ключ, другая сумма — штатный отказ
				default:
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	var total int64
	rows, err := testPool.Query(ctx, `select id, balance from ledger_accounts`)
	must(t, err)
	for rows.Next() {
		var id string
		var b int64
		must(t, rows.Scan(&id, &b))
		total += b
		if id != "bank" && b < 0 {
			t.Fatalf("счёт %s ушёл в минус: %d", id, b)
		}
	}
	rows.Close()
	if total != 0 {
		t.Fatalf("сумма всех остатков %d, должна быть 0: деньги создались или пропали", total)
	}
	if got := balance(t, l, ctx, "bank"); got != -start*accounts {
		t.Fatalf("bank: %d", got)
	}
	noProblems(t, l, ctx)
	t.Logf("отклонено из-за нехватки средств: %d", declined)
}

// tb — минимум, который нужен вспомогательным функциям; его реализуют и *testing.T, и *rapid.T.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}
