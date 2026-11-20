// Пакет api — HTTP-интерфейс журнала. Описание: docs/openapi.yaml.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Oleg4311/ledger-double-entry/ledger"
)

type server struct{ l *ledger.Ledger }

// New возвращает обработчик всех маршрутов.
func New(l *ledger.Ledger) http.Handler {
	s := &server{l: l}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("POST /v1/accounts", s.openAccount)
	mux.HandleFunc("GET /v1/accounts/{id}", s.getAccount)
	mux.HandleFunc("GET /v1/accounts/{id}/entries", s.entries)
	mux.HandleFunc("POST /v1/transfers", s.transfer)
	mux.HandleFunc("POST /v1/transactions/{id}/reverse", s.reverse)
	mux.HandleFunc("GET /v1/verify", s.verify)
	return mux
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message = code, msg
	writeJSON(w, status, e)
}

// fromErr переводит ошибки журнала в статус и стабильный код ошибки.
func fromErr(w http.ResponseWriter, err error) {
	table := []struct {
		err    error
		status int
		code   string
	}{
		{ledger.ErrInvalid, 400, "invalid_request"},
		{ledger.ErrUnbalanced, 400, "unbalanced"},
		{ledger.ErrCurrencyMismatch, 400, "currency_mismatch"},
		{ledger.ErrUnknownAccount, 404, "unknown_account"},
		{ledger.ErrUnknownTransaction, 404, "unknown_transaction"},
		{ledger.ErrAccountExists, 409, "account_exists"},
		{ledger.ErrIdempotencyConflict, 409, "idempotency_conflict"},
		{ledger.ErrAlreadyReversed, 409, "already_reversed"},
		{ledger.ErrInsufficientFunds, 422, "insufficient_funds"},
	}
	for _, e := range table {
		if errors.Is(err, e.err) {
			fail(w, e.status, e.code, err.Error())
			return
		}
	}
	fail(w, 500, "internal", "внутренняя ошибка")
}

// decode читает тело строго: лишние поля и хвост после объекта — ошибка.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, 400, "invalid_request", "тело запроса: "+err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		fail(w, 400, "invalid_request", "тело запроса: после объекта есть лишние данные")
		return false
	}
	return true
}

type accountJSON struct {
	ID            string    `json:"id"`
	Currency      string    `json:"currency"`
	AllowNegative bool      `json:"allow_negative"`
	Balance       int64     `json:"balance"`
	CreatedAt     time.Time `json:"created_at"`
}

func toAccount(a ledger.Account) accountJSON {
	return accountJSON{a.ID, a.Currency, a.AllowNegative, a.Balance, a.CreatedAt}
}

func (s *server) openAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID            string `json:"id"`
		Currency      string `json:"currency"`
		AllowNegative bool   `json:"allow_negative"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.l.OpenAccount(r.Context(), in.ID, in.Currency, in.AllowNegative); err != nil {
		fromErr(w, err)
		return
	}
	a, err := s.l.GetAccount(r.Context(), in.ID)
	if err != nil {
		fromErr(w, err)
		return
	}
	writeJSON(w, 201, toAccount(a))
}

func (s *server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.l.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		fromErr(w, err)
		return
	}
	writeJSON(w, 200, toAccount(a))
}

func (s *server) entries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	recs, err := s.l.Entries(r.Context(), r.PathValue("id"), limit, before)
	if err != nil {
		fromErr(w, err)
		return
	}
	type rec struct {
		ID           int64     `json:"id"`
		TxID         int64     `json:"tx_id"`
		Amount       int64     `json:"amount"`
		BalanceAfter int64     `json:"balance_after"`
		Description  string    `json:"description"`
		CreatedAt    time.Time `json:"created_at"`
	}
	out := struct {
		Entries []rec  `json:"entries"`
		Next    *int64 `json:"next_before,omitempty"`
	}{Entries: []rec{}}
	for _, e := range recs {
		out.Entries = append(out.Entries, rec{e.ID, e.TxID, e.Amount, e.BalanceAfter, e.Description, e.CreatedAt})
	}
	if n := len(recs); n > 0 && (limit <= 0 || n == min(limit, 1000)) {
		last := recs[n-1].ID
		out.Next = &last
	}
	writeJSON(w, 200, out)
}

type postedJSON struct {
	TxID     int64 `json:"tx_id"`
	Replayed bool  `json:"replayed"`
}

func respondPosted(w http.ResponseWriter, p ledger.Posted) {
	status := 201
	if p.Replayed {
		status = 200 // повтор: ничего нового не создано
	}
	writeJSON(w, status, postedJSON{p.TxID, p.Replayed})
}

func (s *server) transfer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Description string `json:"description"`
		Entries     []struct {
			Account string `json:"account"`
			Amount  int64  `json:"amount"`
		} `json:"entries"`
	}
	if !decode(w, r, &in) {
		return
	}
	t := ledger.Transfer{Key: r.Header.Get("Idempotency-Key"), Description: in.Description}
	for _, e := range in.Entries {
		t.Entries = append(t.Entries, ledger.Entry{Account: e.Account, Amount: e.Amount})
	}
	p, err := s.l.Post(r.Context(), t)
	if err != nil {
		fromErr(w, err)
		return
	}
	respondPosted(w, p)
}

func (s *server) reverse(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "invalid_request", "id транзакции должен быть положительным числом")
		return
	}
	var in struct {
		Description string `json:"description"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		fail(w, 400, "invalid_request", "нужен заголовок Idempotency-Key")
		return
	}
	p, err := s.l.Reverse(r.Context(), id, key, in.Description)
	if err != nil {
		fromErr(w, err)
		return
	}
	respondPosted(w, p)
}

func (s *server) verify(w http.ResponseWriter, r *http.Request) {
	probs, err := s.l.Verify(r.Context())
	if err != nil {
		fromErr(w, err)
		return
	}
	type prob struct {
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	}
	out := struct {
		OK       bool   `json:"ok"`
		Problems []prob `json:"problems"`
	}{OK: len(probs) == 0, Problems: []prob{}}
	for _, p := range probs {
		out.Problems = append(out.Problems, prob{p.Kind, p.Detail})
	}
	writeJSON(w, 200, out)
}
