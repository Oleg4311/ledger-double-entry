package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Модельный тест: случайные последовательности операций выполняются и в настоящем журнале (PostgreSQL),
// и в простой модели в памяти. После каждого шага остатки и классы ошибок обязаны совпадать, а в конце
// независимая сверка Verify не должна находить расхождений.
//
// Модель намеренно проще кода: она не знает про блокировки, ограничения и SQL — только правила предметной области.

type mTx struct {
	entries  map[string]int64
	reversed bool
}

// mKey — что именно было проведено под ключом: повтор с тем же содержимым безопасен, с другим — конфликт.
type mKey struct {
	sig string
	tx  int // индекс в model.txs
}

type model struct {
	balance map[string]int64
	allowed map[string]bool // можно ли уходить в минус
	txs     []mTx
	ids     []int64 // реальные id транзакций журнала, по индексу в txs
	keys    map[string]mKey
}

func (m *model) canApply(entries map[string]int64) bool {
	for acc, d := range entries {
		if !m.allowed[acc] && m.balance[acc]+d < 0 {
			return false
		}
	}
	return true
}

func (m *model) apply(entries map[string]int64) {
	for acc, d := range entries {
		m.balance[acc] += d
	}
}

func sig(entries map[string]int64, desc string, reverses int) string {
	accs := make([]string, 0, len(entries))
	for a := range entries {
		accs = append(accs, a)
	}
	sort.Strings(accs)
	s := fmt.Sprintf("d=%s;r=%d;", desc, reverses)
	for _, a := range accs {
		s += fmt.Sprintf("%s:%d;", a, entries[a])
	}
	return s
}

var accountNames = []string{"bank", "a", "b", "c"}

func TestLedgerMatchesModel(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := testPool.Exec(ctx, `truncate ledger_entries, ledger_transactions, ledger_accounts restart identity cascade`); err != nil {
			rt.Fatal(err)
		}
		l := New(testPool)
		m := &model{balance: map[string]int64{}, allowed: map[string]bool{"bank": true}, keys: map[string]mKey{}}
		for _, a := range accountNames {
			if err := l.OpenAccount(ctx, a, "RUB", m.allowed[a]); err != nil {
				rt.Fatal(err)
			}
		}

		account := rapid.SampledFrom(accountNames)
		amount := rapid.Int64Range(1, 300)
		desc := rapid.SampledFrom([]string{"", "оплата"})
		moveKey := rapid.SampledFrom([]string{"k0", "k1", "k2", "k3", "k4", "k5"})
		revKey := rapid.SampledFrom([]string{"r0", "r1", "r2"})

		checkBalances := func() {
			for _, a := range accountNames {
				if got := balance(rt, l, ctx, a); got != m.balance[a] {
					rt.Fatalf("счёт %s: журнал %d, модель %d", a, got, m.balance[a])
				}
			}
		}

		// post выполняет операцию в журнале и в модели и сверяет результат.
		post := func(key string, entries []Entry, descr string) {
			byAcc := map[string]int64{}
			for _, e := range entries {
				byAcc[e.Account] += e.Amount
			}
			distinct := len(byAcc) == len(entries)
			s := sig(byAcc, descr, 0)
			p, err := l.Post(ctx, Transfer{Key: key, Description: descr, Entries: entries})

			switch old, used := m.keys[key]; {
			case !distinct:
				if !errors.Is(err, ErrInvalid) {
					rt.Fatalf("счёт дважды: ожидали ErrInvalid, получили %v", err)
				}
			case used && old.sig == s:
				if err != nil || !p.Replayed || p.TxID != m.ids[old.tx] {
					rt.Fatalf("повтор: ожидали Replayed и id %d, получили %+v, %v", m.ids[old.tx], p, err)
				}
			case used:
				if !errors.Is(err, ErrIdempotencyConflict) {
					rt.Fatalf("конфликт ключа: получили %v", err)
				}
			case !m.canApply(byAcc):
				if !errors.Is(err, ErrInsufficientFunds) {
					rt.Fatalf("нехватка средств: получили %v", err)
				}
			default:
				if err != nil || p.Replayed {
					rt.Fatalf("новая операция: %+v, %v", p, err)
				}
				m.apply(byAcc)
				m.txs = append(m.txs, mTx{entries: byAcc})
				m.ids = append(m.ids, p.TxID)
				m.keys[key] = mKey{sig: s, tx: len(m.txs) - 1}
			}
			checkBalances()
		}

		rt.Repeat(map[string]func(*rapid.T){
			"перевод": func(rt *rapid.T) {
				from, to := account.Draw(rt, "from"), account.Draw(rt, "to")
				amt := amount.Draw(rt, "amount")
				post(moveKey.Draw(rt, "key"), []Entry{{from, -amt}, {to, amt}}, desc.Draw(rt, "desc"))
			},
			"перевод на двоих": func(rt *rapid.T) {
				from, to1, to2 := account.Draw(rt, "from"), account.Draw(rt, "to1"), account.Draw(rt, "to2")
				x, y := amount.Draw(rt, "x"), amount.Draw(rt, "y")
				post(moveKey.Draw(rt, "key"), []Entry{{from, -(x + y)}, {to1, x}, {to2, y}}, desc.Draw(rt, "desc"))
			},
			"несбалансированная": func(rt *rapid.T) {
				from, to := account.Draw(rt, "from"), account.Draw(rt, "to")
				if from == to {
					rt.Skip("один счёт")
				}
				amt := amount.Draw(rt, "amount")
				_, err := l.Post(ctx, Transfer{Key: "unbalanced-" + from + to, Entries: []Entry{{from, -amt}, {to, amt + 1}}})
				if !errors.Is(err, ErrUnbalanced) {
					rt.Fatalf("ожидали ErrUnbalanced, получили %v", err)
				}
				checkBalances()
			},
			"сторно": func(rt *rapid.T) {
				if len(m.txs) == 0 {
					rt.Skip("нечего сторнировать")
				}
				idx := rapid.IntRange(0, len(m.txs)-1).Draw(rt, "tx")
				key := revKey.Draw(rt, "key")
				neg := map[string]int64{}
				for a, d := range m.txs[idx].entries {
					neg[a] = -d
				}
				s := sig(neg, "сторно", idx)
				p, err := l.Reverse(ctx, m.ids[idx], key, "сторно")

				switch old, used := m.keys[key]; {
				case used && old.sig == s:
					if err != nil || !p.Replayed || p.TxID != m.ids[old.tx] {
						rt.Fatalf("повтор сторно: %+v, %v", p, err)
					}
				case used:
					if !errors.Is(err, ErrIdempotencyConflict) {
						rt.Fatalf("конфликт ключа сторно: %v", err)
					}
				case m.txs[idx].reversed:
					if !errors.Is(err, ErrAlreadyReversed) {
						rt.Fatalf("повторное сторно: %v", err)
					}
				case !m.canApply(neg):
					if !errors.Is(err, ErrInsufficientFunds) {
						rt.Fatalf("сторно без средств: %v", err)
					}
				default:
					if err != nil || p.Replayed {
						rt.Fatalf("новое сторно: %+v, %v", p, err)
					}
					m.apply(neg)
					m.txs[idx].reversed = true
					m.txs = append(m.txs, mTx{entries: neg})
					m.ids = append(m.ids, p.TxID)
					m.keys[key] = mKey{sig: s, tx: len(m.txs) - 1}
				}
				checkBalances()
			},
			"": func(rt *rapid.T) { // инварианты после каждого шага
				checkBalances()
				var total int64
				for _, a := range accountNames {
					total += m.balance[a]
					if !m.allowed[a] && m.balance[a] < 0 {
						rt.Fatalf("модель: счёт %s в минусе", a)
					}
				}
				if total != 0 {
					rt.Fatalf("сумма остатков %d, должна быть 0", total)
				}
			},
		})

		if p, err := l.Verify(ctx); err != nil || len(p) != 0 {
			rt.Fatalf("сверка в конце сценария: %+v, %v", p, err)
		}
	})
}
