package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
)

const satsPerBTC = 100_000_000

// config holds the parsed CLI flags for the accounting report command.
type config struct {
	backend          string
	sqliteDBFile     string
	postgresHost     string
	postgresPort     int
	postgresUser     string
	postgresPassword string
	postgresDBName   string
	postgresSSL      bool
	applyMigrations  bool
	format           string
	priceSource      string
	fiatCurrency     string
	timeout          time.Duration
}

// accountBalance is the shape emitted for one chart-of-accounts row.
type accountBalance struct {
	AccountID   string   `json:"account_id"`
	AccountName string   `json:"account_name"`
	AccountType string   `json:"account_type"`
	BalanceSat  int64    `json:"balance_sat"`
	BalanceFiat *float64 `json:"balance_fiat,omitempty"`
}

// eventTotal is the shape emitted for one ledger event type total.
type eventTotal struct {
	EventType  string `json:"event_type"`
	EntryCount int64  `json:"entry_count"`
	TotalSat   int64  `json:"total_sat"`
}

// report is the top-level accounting report.
type report struct {
	GeneratedAtUnix int64            `json:"generated_at_unix"`
	Backend         string           `json:"backend"`
	EntryCount      int64            `json:"entry_count"`
	FirstEntryUnix  int64            `json:"first_entry_unix"`
	LastEntryUnix   int64            `json:"last_entry_unix"`
	FiatCurrency    string           `json:"fiat_currency,omitempty"`
	BTCFiatPrice    *float64         `json:"btc_fiat_price,omitempty"`
	Accounts        []accountBalance `json:"accounts"`
	Events          []eventTotal     `json:"events"`
}

// PriceSource resolves a BTC fiat price without coupling this tool to
// Faraday's accounting interfaces.
type PriceSource interface {
	Price(ctx context.Context, currency string) (float64, error)
}

// CoinGeckoPriceSource resolves current BTC prices from CoinGecko's
// public simple-price endpoint.
type CoinGeckoPriceSource struct {
	Client *http.Client
}

// Price returns the current BTC price in the requested fiat currency.
func (s CoinGeckoPriceSource) Price(ctx context.Context, currency string) (
	float64, error) {

	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}

	currency = strings.ToLower(currency)
	query := url.Values{}
	query.Set("ids", "bitcoin")
	query.Set("vs_currencies", currency)
	reqURL := "https://api.coingecko.com/api/v3/simple/price?" +
		query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, err
	}

	//nolint:gosec // Fixed CoinGecko endpoint; currency is query-encoded.
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("coingecko returned %s", resp.Status)
	}

	// Cap the decoded body so a compromised or misbehaving endpoint
	// cannot stream an unbounded response and exhaust memory. The
	// simple-price payload is a few hundred bytes; 1 MiB is generous.
	const maxPriceBody = 1 << 20
	limited := io.LimitReader(resp.Body, maxPriceBody)

	var body struct {
		Bitcoin map[string]float64 `json:"bitcoin"`
	}
	if err := json.NewDecoder(limited).Decode(&body); err != nil {
		return 0, err
	}

	// Reject non-finite or non-positive prices so a NaN/Inf value cannot
	// poison the downstream fiat conversion.
	price, ok := body.Bitcoin[currency]
	if !ok || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("missing BTC/%s price", currency)
	}

	return price, nil
}

// main runs the accounting report command.
func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "accounting: %v\n", err)
		os.Exit(1)
	}
}

// run parses flags, reads the ledger DB, and writes the report.
func run(ctx context.Context, args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	// SkipMigrations is set by default, so the store constructor performs
	// no migration IO and manages its own context internally.
	//nolint:contextcheck
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() {
		_ = store.Close()
	}()

	var price *float64
	if cfg.priceSource != "none" {
		src, err := newPriceSource(cfg.priceSource)
		if err != nil {
			return err
		}

		got, err := src.Price(ctx, cfg.fiatCurrency)
		if err != nil {
			return err
		}
		price = &got
	}

	rep, err := buildReport(ctx, store, cfg, price)
	if err != nil {
		return err
	}

	switch cfg.format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(rep)

	case "csv":
		return writeCSVReport(os.Stdout, rep)

	case "text":
		printTextReport(rep)

		return nil

	default:
		return fmt.Errorf("unknown --format %q", cfg.format)
	}
}

// parseFlags parses command-line flags into a config struct.
func parseFlags(args []string) (*config, error) {
	cfg := &config{
		backend:        "sqlite",
		postgresHost:   "localhost",
		postgresPort:   5432,
		postgresUser:   "postgres",
		postgresDBName: "arkd",
	}

	fs := flag.NewFlagSet("accounting", flag.ContinueOnError)
	fs.StringVar(
		&cfg.backend, "backend", cfg.backend,
		"database backend: sqlite or postgres",
	)
	fs.StringVar(
		&cfg.sqliteDBFile, "sqlite.dbfile", "",
		"path to the waved sqlite database file",
	)
	fs.StringVar(
		&cfg.postgresHost, "postgres.host", cfg.postgresHost,
		"postgres host",
	)
	fs.IntVar(
		&cfg.postgresPort, "postgres.port", cfg.postgresPort,
		"postgres port",
	)
	fs.StringVar(
		&cfg.postgresUser, "postgres.user", cfg.postgresUser,
		"postgres user",
	)
	fs.StringVar(
		&cfg.postgresPassword, "postgres.password", "",
		"postgres password",
	)
	fs.StringVar(
		&cfg.postgresDBName, "postgres.dbname", cfg.postgresDBName,
		"postgres database name",
	)
	fs.BoolVar(
		&cfg.postgresSSL, "postgres.ssl", false,
		"require SSL for postgres",
	)
	fs.BoolVar(
		&cfg.applyMigrations, "apply-migrations", false, "apply "+
			"database migrations before reporting (off by "+
			"default so the report never mutates the daemon "+
			"schema)",
	)
	fs.StringVar(
		&cfg.format, "format", "text",
		"output format: text, json, or csv",
	)
	fs.StringVar(
		&cfg.priceSource, "price-source", "none",
		"fiat price source: none or coingecko",
	)
	fs.StringVar(
		&cfg.fiatCurrency, "fiat", "usd", "fiat currency code",
	)
	fs.DurationVar(
		&cfg.timeout, "timeout", 15*time.Second, "report timeout",
	)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	return cfg, nil
}

// openStore opens the configured backend without running migrations unless the
// operator explicitly opts in, reusing the same db package the daemon uses so
// the report works against either a sqlite or a postgres deployment.
func openStore(cfg *config) (*db.Store, error) {
	switch cfg.backend {
	case "sqlite":
		if cfg.sqliteDBFile == "" {
			return nil, fmt.Errorf("--sqlite.dbfile is required " +
				"for the sqlite backend")
		}

		// Refuse to create a fresh database: a missing path almost
		// always means a typo, and reporting against an empty DB the
		// tool just created would be misleading. The shared opener
		// would otherwise create the file on first use.
		//
		//nolint:gosec // G703: operator-supplied path to their own DB.
		info, err := os.Stat(cfg.sqliteDBFile)
		if err != nil {
			return nil, fmt.Errorf("stat sqlite db: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("sqlite path %q is a directory",
				cfg.sqliteDBFile)
		}

	case "postgres":
	default:
		return nil, fmt.Errorf("unsupported backend %q (want sqlite "+
			"or postgres)", cfg.backend)
	}

	dbCfg := &db.Config{
		Backend: cfg.backend,
		Sqlite: &db.SqliteConfig{
			DatabaseFileName:      cfg.sqliteDBFile,
			SkipMigrations:        !cfg.applyMigrations,
			SkipMigrationDBBackup: !cfg.applyMigrations,
		},
		Postgres: &db.PostgresConfig{
			Host:               cfg.postgresHost,
			Port:               cfg.postgresPort,
			User:               cfg.postgresUser,
			Password:           cfg.postgresPassword,
			DBName:             cfg.postgresDBName,
			RequireSSL:         cfg.postgresSSL,
			SkipMigrations:     !cfg.applyMigrations,
			MaxOpenConnections: 1,
		},
	}

	return db.NewStoreFromConfig(dbCfg, btclog.Disabled)
}

// newPriceSource returns the requested fiat price source.
func newPriceSource(name string) (PriceSource, error) {
	switch name {
	case "coingecko":
		return CoinGeckoPriceSource{
			Client: &http.Client{
				Timeout: 10 * time.Second,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown --price-source %q", name)
	}
}

// buildReport reads account balances and event totals from the DB inside a
// single read-only transaction.
func buildReport(ctx context.Context, store *db.Store, cfg *config,
	price *float64) (*report, error) {

	// A read-only transaction expresses intent and, on backends that
	// enforce it (e.g. postgres), rejects an accidental write. Combined
	// with SkipMigrations and report queries that only SELECT, the tool
	// never mutates the daemon's database.
	tx, err := store.BaseDB().BeginTx(ctx, db.ReadTxOption())
	if err != nil {
		return nil, fmt.Errorf("begin read tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	q := store.Queries().WithTx(tx)

	stats, err := q.GetClientLedgerStats(ctx)
	if err != nil {
		return nil, err
	}

	accountRows, err := q.ListClientAccountBalances(ctx)
	if err != nil {
		return nil, err
	}

	eventRows, err := q.ListClientLedgerEventTotals(ctx)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit read tx: %w", err)
	}

	rep := &report{
		GeneratedAtUnix: time.Now().Unix(),
		Backend:         cfg.backend,
		EntryCount:      stats.EntryCount,
		FirstEntryUnix:  stats.FirstCreatedAt,
		LastEntryUnix:   stats.LastCreatedAt,
		FiatCurrency:    strings.ToUpper(cfg.fiatCurrency),
		BTCFiatPrice:    price,
		Accounts:        make([]accountBalance, 0, len(accountRows)),
		Events:          make([]eventTotal, 0, len(eventRows)),
	}
	if price == nil {
		rep.FiatCurrency = ""
	}

	for _, row := range accountRows {
		account := accountBalance{
			AccountID:   row.AccountID,
			AccountName: row.AccountName,
			AccountType: row.AccountType,
			BalanceSat:  row.BalanceSat,
		}
		if price != nil {
			value := satsToFiat(row.BalanceSat, *price)
			account.BalanceFiat = &value
		}

		rep.Accounts = append(rep.Accounts, account)
	}

	for _, row := range eventRows {
		rep.Events = append(rep.Events, eventTotal{
			EventType:  row.EventType,
			EntryCount: row.EntryCount,
			TotalSat:   row.TotalSat,
		})
	}

	return rep, nil
}

// satsToFiat converts satoshis to fiat using a BTC fiat price.
func satsToFiat(sats int64, btcPrice float64) float64 {
	return (float64(sats) / satsPerBTC) * btcPrice
}

// writeCSVReport emits the report as a single flat CSV table. Each row is a
// line item discriminated by the leading "category" column ("account" or
// "event"), so balance-sheet rows and event totals share one parseable
// stream.
func writeCSVReport(w io.Writer, rep *report) error {
	cw := csv.NewWriter(w)

	header := []string{
		"category", "id", "name", "account_type", "entry_count",
		"amount_sat", "fiat",
	}
	if err := cw.Write(header); err != nil {
		return err
	}

	for _, account := range rep.Accounts {
		fiat := ""
		if account.BalanceFiat != nil {
			fiat = strconv.FormatFloat(
				*account.BalanceFiat, 'f', 2, 64,
			)
		}

		row := []string{
			"account", account.AccountID, account.AccountName,
			account.AccountType, "",
			strconv.FormatInt(account.BalanceSat, 10), fiat,
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}

	for _, event := range rep.Events {
		row := []string{
			"event", event.EventType, "", "",
			strconv.FormatInt(event.EntryCount, 10),
			strconv.FormatInt(event.TotalSat, 10), "",
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}

// printTextReport writes a compact operator-readable report.
func printTextReport(rep *report) {
	fmt.Printf("Accounting report (%s backend)\n", rep.Backend)
	fmt.Printf("Generated: %s\n", time.Unix(rep.GeneratedAtUnix, 0).Format(
		time.RFC3339,
	))
	fmt.Printf("Ledger entries: %d\n", rep.EntryCount)
	if rep.EntryCount > 0 {
		fmt.Printf("Entry range: %s to %s\n",
			time.Unix(rep.FirstEntryUnix, 0).Format(time.RFC3339),
			time.Unix(rep.LastEntryUnix, 0).Format(time.RFC3339))
	}
	if rep.BTCFiatPrice != nil {
		fmt.Printf("BTC/%s price: %.2f\n", rep.FiatCurrency,
			*rep.BTCFiatPrice)
	}

	fmt.Println()
	fmt.Println("Accounts")
	for _, account := range rep.Accounts {
		if account.BalanceFiat == nil {
			fmt.Printf("  %-22s %14d sat  %s\n", account.AccountID,
				account.BalanceSat, account.AccountType)
			continue
		}

		fmt.Printf("  %-22s %14d sat  %12.2f %s  %s\n",
			account.AccountID, account.BalanceSat,
			*account.BalanceFiat, rep.FiatCurrency,
			account.AccountType)
	}

	fmt.Println()
	fmt.Println("Events")
	for _, event := range rep.Events {
		fmt.Printf("  %-24s %8d entries %14d sat\n", event.EventType,
			event.EntryCount, event.TotalSat)
	}
}
