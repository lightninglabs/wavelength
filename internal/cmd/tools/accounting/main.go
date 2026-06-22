package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lightninglabs/darepo-client/db/sqlc"
	_ "modernc.org/sqlite"
)

const satsPerBTC = 100_000_000

// accountBalance is the JSON shape emitted for one chart-of-accounts row.
type accountBalance struct {
	AccountID   string   `json:"account_id"`
	AccountName string   `json:"account_name"`
	AccountType string   `json:"account_type"`
	BalanceSat  int64    `json:"balance_sat"`
	BalanceFiat *float64 `json:"balance_fiat,omitempty"`
}

// eventTotal is the JSON shape emitted for one ledger event type total.
type eventTotal struct {
	EventType  string `json:"event_type"`
	EntryCount int64  `json:"entry_count"`
	TotalSat   int64  `json:"total_sat"`
}

// report is the top-level accounting report.
type report struct {
	GeneratedAtUnix int64            `json:"generated_at_unix"`
	SQLitePath      string           `json:"sqlite_path"`
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

	var body struct {
		Bitcoin map[string]float64 `json:"bitcoin"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}

	price, ok := body.Bitcoin[currency]
	if !ok || price <= 0 {
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
	fs := flag.NewFlagSet("accounting", flag.ContinueOnError)
	sqlitePath := fs.String("sqlite", "", "path to darepod SQLite DB")
	format := fs.String("format", "text", "output format: text or json")
	priceSource := fs.String(
		"price-source", "none", "fiat price source: none or coingecko",
	)
	fiatCurrency := fs.String("fiat", "usd", "fiat currency code")
	timeout := fs.Duration("timeout", 15*time.Second, "report timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sqlitePath == "" {
		return fmt.Errorf("--sqlite is required")
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	rawDB, err := openReadOnlySQLite(ctx, *sqlitePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = rawDB.Close()
	}()
	queries := sqlc.NewSqlite(rawDB)

	var price *float64
	if *priceSource != "none" {
		src, err := newPriceSource(*priceSource)
		if err != nil {
			return err
		}

		got, err := src.Price(ctx, *fiatCurrency)
		if err != nil {
			return err
		}
		price = &got
	}

	rep, err := buildReport(
		ctx, queries, *sqlitePath, *fiatCurrency, price,
	)
	if err != nil {
		return err
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(rep)

	case "text":
		printTextReport(rep)

		return nil

	default:
		return fmt.Errorf("unknown --format %q", *format)
	}
}

// openReadOnlySQLite opens an existing SQLite database in read-only mode.
func openReadOnlySQLite(ctx context.Context, path string) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat sqlite db: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("sqlite path %q is a directory", path)
	}

	rawDB, err := sql.Open("sqlite", readOnlySQLiteDSN(path))
	if err != nil {
		return nil, err
	}
	rawDB.SetMaxOpenConns(1)
	rawDB.SetMaxIdleConns(1)

	if _, err := rawDB.ExecContext(
		ctx, "PRAGMA query_only = ON",
	); err != nil {

		_ = rawDB.Close()

		return nil, fmt.Errorf("enable sqlite query_only: %w", err)
	}

	if err := rawDB.PingContext(ctx); err != nil {
		_ = rawDB.Close()

		return nil, err
	}

	return rawDB, nil
}

// readOnlySQLiteDSN returns a SQLite URI that refuses to create or mutate the
// target database file.
func readOnlySQLiteDSN(path string) string {
	uri := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=ro",
	}

	return uri.String()
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

// buildReport reads account balances and event totals from the DB.
func buildReport(ctx context.Context, queries *sqlc.Queries, sqlitePath,
	fiatCurrency string, price *float64) (*report, error) {

	stats, err := queries.GetClientLedgerStats(ctx)
	if err != nil {
		return nil, err
	}

	accountRows, err := queries.ListClientAccountBalances(ctx)
	if err != nil {
		return nil, err
	}

	eventRows, err := queries.ListClientLedgerEventTotals(ctx)
	if err != nil {
		return nil, err
	}

	rep := &report{
		GeneratedAtUnix: time.Now().Unix(),
		SQLitePath:      sqlitePath,
		EntryCount:      stats.EntryCount,
		FirstEntryUnix:  stats.FirstCreatedAt,
		LastEntryUnix:   stats.LastCreatedAt,
		FiatCurrency:    strings.ToUpper(fiatCurrency),
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

// printTextReport writes a compact operator-readable report.
func printTextReport(rep *report) {
	fmt.Printf("Accounting report: %s\n", rep.SQLitePath)
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
