package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oleg4311/ledger-double-entry/internal/pgtest"
	"github.com/Oleg4311/ledger-double-entry/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	pool, stop, err := pgtest.Start(ctx, "api_test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "не удалось подготовить Postgres:", err)
		os.Exit(1)
	}
	testPool = pool
	if err := ledger.Migrate(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "миграция:", err)
		stop()
		os.Exit(1)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T) *client {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `truncate ledger_entries, ledger_transactions, ledger_accounts restart identity cascade`); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(ledger.New(testPool)))
	t.Cleanup(srv.Close)
	return &client{t, srv}
}

func (c *client) do(method, path, key, body string) (int, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func (c *client) account(id string, negative bool) {
	c.t.Helper()
	st, out := c.do("POST", "/v1/accounts", "", fmt.Sprintf(`{"id":%q,"currency":"RUB","allow_negative":%v}`, id, negative))
	if st != 201 {
		c.t.Fatalf("открытие счёта %s: %d %v", id, st, out)
	}
}

func TestEndToEndOverHTTP(t *testing.T) {
	c := newClient(t)
	c.account("bank", true)
	c.account("alice", false)

	if st, out := c.do("POST", "/v1/accounts", "", `{"id":"alice","currency":"RUB"}`); st != 409 || errCode(out) != "account_exists" {
		t.Fatalf("повторное открытие: %d %v", st, out)
	}
	st, out := c.do("POST", "/v1/transfers", "fund-1", `{"description":"пополнение","entries":[{"account":"bank","amount":-5000},{"account":"alice","amount":5000}]}`)
	if st != 201 || out["replayed"] != false {
		t.Fatalf("перевод: %d %v", st, out)
	}
	txID := int64(out["tx_id"].(float64))

	st, out = c.do("POST", "/v1/transfers", "fund-1", `{"description":"пополнение","entries":[{"account":"bank","amount":-5000},{"account":"alice","amount":5000}]}`)
	if st != 200 || out["replayed"] != true || int64(out["tx_id"].(float64)) != txID {
		t.Fatalf("повтор: %d %v", st, out)
	}
	if st, out = c.do("POST", "/v1/transfers", "fund-1", `{"description":"пополнение","entries":[{"account":"bank","amount":-5001},{"account":"alice","amount":5001}]}`); st != 409 || errCode(out) != "idempotency_conflict" {
		t.Fatalf("конфликт ключа: %d %v", st, out)
	}

	st, out = c.do("GET", "/v1/accounts/alice", "", "")
	if st != 200 || out["balance"] != float64(5000) {
		t.Fatalf("счёт: %d %v", st, out)
	}
	st, out = c.do("GET", "/v1/accounts/alice/entries?limit=1", "", "")
	entries := out["entries"].([]any)
	if st != 200 || len(entries) != 1 || out["next_before"] == nil {
		t.Fatalf("выписка: %d %v", st, out)
	}

	st, out = c.do("POST", fmt.Sprintf("/v1/transactions/%d/reverse", txID), "rev-1", `{"description":"ошибка оператора"}`)
	if st != 201 {
		t.Fatalf("сторно: %d %v", st, out)
	}
	if st, out = c.do("POST", fmt.Sprintf("/v1/transactions/%d/reverse", txID), "rev-2", ""); st != 409 || errCode(out) != "already_reversed" {
		t.Fatalf("второе сторно: %d %v", st, out)
	}
	if st, out = c.do("GET", "/v1/accounts/alice", "", ""); out["balance"] != float64(0) {
		t.Fatalf("после сторно: %d %v", st, out)
	}
	if st, out = c.do("GET", "/v1/verify", "", ""); st != 200 || out["ok"] != true {
		t.Fatalf("сверка: %d %v", st, out)
	}
}

func TestErrorStatusesAndCodes(t *testing.T) {
	c := newClient(t)
	c.account("bank", true)
	c.account("alice", false)
	pay := func(amt int64) string {
		return fmt.Sprintf(`{"entries":[{"account":"alice","amount":%d},{"account":"bank","amount":%d}]}`, -amt, amt)
	}
	cases := []struct {
		name, method, path, key, body string
		status                        int
		code                          string
	}{
		{"недостаточно средств", "POST", "/v1/transfers", "e1", pay(100), 422, "insufficient_funds"},
		{"нет ключа идемпотентности", "POST", "/v1/transfers", "", pay(100), 400, "invalid_request"},
		{"не сходится", "POST", "/v1/transfers", "e2", `{"entries":[{"account":"alice","amount":-1},{"account":"bank","amount":2}]}`, 400, "unbalanced"},
		{"нет счёта", "POST", "/v1/transfers", "e3", `{"entries":[{"account":"nope","amount":-1},{"account":"bank","amount":1}]}`, 404, "unknown_account"},
		{"битый JSON", "POST", "/v1/transfers", "e4", `{"entries":`, 400, "invalid_request"},
		{"лишнее поле", "POST", "/v1/transfers", "e5", `{"entries":[],"extra":1}`, 400, "invalid_request"},
		{"хвост после объекта", "POST", "/v1/accounts", "", `{"id":"z","currency":"RUB"} {}`, 400, "invalid_request"},
		{"сумма строкой", "POST", "/v1/transfers", "e6", `{"entries":[{"account":"alice","amount":"5"},{"account":"bank","amount":"-5"}]}`, 400, "invalid_request"},
		{"дробная сумма", "POST", "/v1/transfers", "e7", `{"entries":[{"account":"alice","amount":1.5},{"account":"bank","amount":-1.5}]}`, 400, "invalid_request"},
		{"плохая валюта", "POST", "/v1/accounts", "", `{"id":"x","currency":"rub"}`, 400, "invalid_request"},
		{"нет счёта в выписке", "GET", "/v1/accounts/nope/entries", "", "", 404, "unknown_account"},
		{"нет счёта", "GET", "/v1/accounts/nope", "", "", 404, "unknown_account"},
		{"нет транзакции", "POST", "/v1/transactions/999/reverse", "r1", "", 404, "unknown_transaction"},
		{"id транзакции не число", "POST", "/v1/transactions/abc/reverse", "r2", "", 400, "invalid_request"},
		{"сторно без ключа", "POST", "/v1/transactions/1/reverse", "", "", 400, "invalid_request"},
	}
	for _, tc := range cases {
		st, out := c.do(tc.method, tc.path, tc.key, tc.body)
		if st != tc.status || errCode(out) != tc.code {
			t.Errorf("%s: %d %v, ожидали %d %s", tc.name, st, out, tc.status, tc.code)
		}
	}
	if st, _ := c.do("GET", "/healthz", "", ""); st != 200 {
		t.Error("healthz")
	}
	// неподдерживаемый метод → 405, неизвестный путь → 404
	if st, _ := c.do("DELETE", "/v1/accounts/alice", "", ""); st != 405 {
		t.Errorf("DELETE: %d", st)
	}
	if st, _ := c.do("GET", "/v1/nowhere", "", ""); st != 404 {
		t.Errorf("неизвестный путь: %d", st)
	}
}

// Двойной клик по HTTP: 20 одновременных запросов с одним ключом — один платёж.
func TestConcurrentDuplicateHTTPRequestsPostOnce(t *testing.T) {
	c := newClient(t)
	c.account("bank", true)
	c.account("alice", false)
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, _ := c.do("POST", "/v1/transfers", "double-click", `{"entries":[{"account":"bank","amount":-300},{"account":"alice","amount":300}]}`)
			mu.Lock()
			statuses[st]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if statuses[201] != 1 || statuses[200] != 19 {
		t.Fatalf("статусы: %v (ожидали один 201 и 19 × 200)", statuses)
	}
	if _, out := c.do("GET", "/v1/accounts/alice", "", ""); out["balance"] != float64(300) {
		t.Fatalf("баланс %v, ожидали 300", out["balance"])
	}
}

// Описание в docs/openapi.yaml и реальные маршруты не должны расходиться.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	raw, err := os.ReadFile("../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	c.account("a", true)
	if len(doc.Paths) < 6 {
		t.Fatalf("в описании только %d путей", len(doc.Paths))
	}
	for path, methods := range doc.Paths {
		concrete := strings.NewReplacer("{id}", "a").Replace(path)
		if strings.Contains(path, "reverse") {
			concrete = "/v1/transactions/1/reverse"
		}
		for method := range methods {
			var body string
			if method == "post" {
				body = `{}`
			}
			st, _ := c.do(strings.ToUpper(method), concrete, "spec-"+method+concrete, body)
			if st == 404 && !strings.Contains(path, "{id}") || st == 405 {
				t.Errorf("%s %s описан в openapi.yaml, но маршрут его не обслуживает (статус %d)", strings.ToUpper(method), path, st)
			}
		}
	}
	// обратное направление: каждый маршрут сервера описан
	for _, route := range []string{"/healthz", "/v1/accounts", "/v1/accounts/{id}", "/v1/accounts/{id}/entries", "/v1/transfers", "/v1/transactions/{id}/reverse", "/v1/verify"} {
		if _, ok := doc.Paths[route]; !ok {
			t.Errorf("маршрут %s есть в сервере, но не описан в openapi.yaml", route)
		}
	}
}
