// ledgerd — HTTP-сервис журнала с двойной записью.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/Oleg4311/ledger-double-entry/api"
	"github.com/Oleg4311/ledger-double-entry/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
)

var version = "0.1.0"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "адрес, который слушает сервис")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "строка подключения к PostgreSQL (или DATABASE_URL)")
	migrate := flag.Bool("migrate", false, "создать/обновить схему при запуске")
	showVersion := flag.Bool("version", false, "показать версию")
	flag.Parse()
	if *showVersion {
		fmt.Println("ledgerd", version)
		return
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "ошибка: не задан --dsn или DATABASE_URL")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
	defer pool.Close()
	if *migrate {
		if err := ledger.Migrate(ctx, pool); err != nil {
			fmt.Fprintln(os.Stderr, "миграция:", err)
			os.Exit(1)
		}
	}

	srv := &http.Server{Addr: *addr, Handler: api.New(ledger.New(pool)), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Printf("ledgerd %s слушает http://%s\n", version, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}
