#!/usr/bin/env bash
# Сквозной прогон настоящего сервиса по HTTP на настоящем Postgres.
set -euo pipefail
: "${DATABASE_URL:?нужен DATABASE_URL}"
go build -o ledgerd ./cmd/ledgerd
./ledgerd --migrate --addr 127.0.0.1:18080 > ledgerd.log 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
for i in $(seq 50); do curl -fsS http://127.0.0.1:18080/healthz >/dev/null 2>&1 && break; sleep 0.2; done

B=http://127.0.0.1:18080
fail() { echo "ПРОВАЛ: $*" >&2; cat ledgerd.log >&2; exit 1; }
post() { curl -sS -o /tmp/body -w '%{http_code}' -X POST "$B$1" -H 'Content-Type: application/json' ${2:+-H "Idempotency-Key: $2"} -d "$3"; }

[ "$(post /v1/accounts '' '{"id":"smoke-bank","currency":"RUB","allow_negative":true}')" = 201 ] || [ "$(post /v1/accounts '' '{"id":"smoke-bank","currency":"RUB","allow_negative":true}')" = 409 ] || fail "счёт bank"
post /v1/accounts '' '{"id":"smoke-alice","currency":"RUB"}' >/dev/null
K="smoke-$$"
T='{"description":"пополнение","entries":[{"account":"smoke-bank","amount":-1000},{"account":"smoke-alice","amount":1000}]}'
[ "$(post /v1/transfers "$K" "$T")" = 201 ] || fail "первый перевод должен дать 201: $(cat /tmp/body)"
[ "$(post /v1/transfers "$K" "$T")" = 200 ] || fail "повтор должен дать 200"
grep -q '"replayed":true' /tmp/body || fail "повтор не помечен replayed"
[ "$(post /v1/transfers "e-$K" '{"entries":[{"account":"smoke-alice","amount":-5000},{"account":"smoke-bank","amount":5000}]}')" = 422 ] || fail "нехватка средств должна давать 422"
grep -q insufficient_funds /tmp/body || fail "код ошибки"
BAL=$(curl -fsS $B/v1/accounts/smoke-alice | sed -n 's/.*"balance":\([0-9-]*\).*/\1/p')
[ "$BAL" = 1000 ] || fail "баланс alice $BAL, ожидали 1000"
curl -fsS $B/v1/verify | grep -q '"ok":true' || fail "сверка нашла расхождения"
echo "smoke: всё хорошо"
