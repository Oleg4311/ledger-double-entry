package ledger

import (
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"
)

// BenchmarkMove — пропускная способность переводов между 200 счетами при параллельной нагрузке.
// Запуск: go test -run xxx -bench Move -benchtime 5000x ./ledger/
func BenchmarkMove(b *testing.B) {
	ctx, l := fresh(b)
	const accounts = 200
	if err := l.OpenAccount(ctx, "bank", "RUB", true); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < accounts; i++ {
		id := fmt.Sprintf("u%d", i)
		if err := l.OpenAccount(ctx, id, "RUB", false); err != nil {
			b.Fatal(err)
		}
		if _, err := l.Move(ctx, "seed-"+id, "bank", id, 1_000_000_000, ""); err != nil {
			b.Fatal(err)
		}
	}
	var seq atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewPCG(uint64(seq.Add(1)), 7))
		for pb.Next() {
			x, y := rng.IntN(accounts), rng.IntN(accounts)
			if x == y {
				y = (y + 1) % accounts
			}
			if _, err := l.Move(ctx, fmt.Sprintf("b-%d", seq.Add(1)), fmt.Sprintf("u%d", x), fmt.Sprintf("u%d", y), 1, ""); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "переводов/с")
}
