package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrations применяются по порядку; номер версии = индекс+1.
//
// Главное: правила журнала держит сама база, а не только код приложения.
//   - сумма проводок транзакции по каждой валюте равна нулю (отложенный триггер: проверяется при COMMIT);
//   - баланс счёта не уходит в минус, если счёт этого не разрешает (CHECK);
//   - проводки и транзакции неизменяемы (триггеры запрещают UPDATE и DELETE);
//   - проводка ссылается на счёт и его валюту, поэтому «рубли на долларовый счёт» не вставить.
var migrations = []string{
	`create table ledger_accounts (
		id             text    primary key check (id ~ '^[A-Za-z0-9:_.-]{1,64}$'),
		currency       text    not null check (currency ~ '^[A-Z]{3}$'),
		allow_negative boolean not null default false,
		balance        bigint  not null default 0,
		created_at     timestamptz not null default now(),
		unique (id, currency),
		constraint balance_non_negative check (allow_negative or balance >= 0)
	);

	create table ledger_transactions (
		id              bigserial primary key,
		idempotency_key text   not null,
		fingerprint     text   not null,
		description     text   not null default '',
		reverses        bigint references ledger_transactions (id),
		created_at      timestamptz not null default now(),
		constraint ledger_idempotency unique (idempotency_key),
		constraint ledger_reverses_once unique (reverses)
	);

	create table ledger_entries (
		id            bigserial primary key,
		tx_id         bigint not null references ledger_transactions (id),
		account_id    text   not null,
		currency      text   not null,
		amount        bigint not null check (amount <> 0),
		balance_after bigint not null,
		foreign key (account_id, currency) references ledger_accounts (id, currency),
		unique (tx_id, account_id)
	);
	create index ledger_entries_account on ledger_entries (account_id, id desc);

	create function ledger_forbid_change() returns trigger language plpgsql as $$
	begin
		raise exception 'ledger rows are immutable (%.%)', tg_table_name, tg_op using errcode = '23000', constraint = 'ledger_immutable';
	end $$;
	create trigger ledger_entries_immutable before update or delete on ledger_entries
		for each row execute function ledger_forbid_change();
	create trigger ledger_transactions_immutable before update or delete on ledger_transactions
		for each row execute function ledger_forbid_change();

	-- Баланс проводки проверяется один раз при фиксации, когда уже вставлены все строки транзакции.
	create function ledger_check_balanced() returns trigger language plpgsql as $$
	declare bad record;
	begin
		select currency, sum(amount) as total into bad
		from ledger_entries where tx_id = new.tx_id
		group by currency having sum(amount) <> 0 limit 1;
		if found then
			raise exception 'transaction % is not balanced in %: sum %', new.tx_id, bad.currency, bad.total
				using errcode = '23514', constraint = 'ledger_balanced';
		end if;
		return null;
	end $$;
	create constraint trigger ledger_entries_balanced after insert on ledger_entries
		deferrable initially deferred for each row execute function ledger_check_balanced();`,
}

// Migrate создаёт или обновляет схему. Безопасно вызывать одновременно из нескольких процессов.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	const lockID = 7_251_903_227 // произвольная константа проекта
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, lockID); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, lockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `create table if not exists ledger_migrations (version int primary key, applied_at timestamptz not null default now())`); err != nil {
		return err
	}
	var current int
	if err := conn.QueryRow(ctx, `select coalesce(max(version), 0) from ledger_migrations`).Scan(&current); err != nil {
		return err
	}
	for v := current; v < len(migrations); v++ {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, migrations[v]); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("миграция %d: %w", v+1, err)
		}
		if _, err := tx.Exec(ctx, `insert into ledger_migrations (version) values ($1)`, v+1); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
