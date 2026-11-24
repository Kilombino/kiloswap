// kiloswap-guard is lnd RPC middleware that bounds what the Kiloswap swap
// backend may make the main lnd do (see internal/policy). Configuration by
// environment:
//
//	LND_GRPC        host:port of lnd's gRPC (e.g. lnd:10009)
//	LND_TLSCERT     lnd's tls.cert
//	LND_MACAROON    the guard's macaroon (RegisterRPCMiddleware, WalletBalance,
//	                LookupInvoice)
//	CHAIN_RPC_URL   the node's JSON-RPC (http://host:8332/), with
//	CHAIN_RPC_USER / CHAIN_RPC_PASSWORD (or _FILE): getrawtransaction only
//	DATABASE_URL    postgres://kiloswap_guard:…@postgres/boltz (read-only role)
//	LSP_STATE       the LSP's state.json (read-only mount)
//	LEDGER          where the guard keeps its ledger
//	ALERT_URL       optional: each refusal is POSTed here as JSON (the bot)
//	MAX_PER_OP_SAT, DAILY_SAT, RESERVE_SAT, MAX_PENDING_REVERSE,
//	FEE_RATIO, MIN_FEE_SAT, MAX_FEE_RATE, REQUIRED_CONFS  limits (defaults below)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/middleware"
	"github.com/Kilombino/kiloswap/guard/internal/policy"
	"github.com/Kilombino/kiloswap/guard/internal/sources"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

// secret reads K, or the file named by K_FILE.
func secret(k string) string {
	if f := os.Getenv(k + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("%s_FILE: %v", k, err)
		}
		return strings.TrimSpace(string(b))
	}
	return must(k)
}

func num(k string, def int64) int64 {
	v, err := strconv.ParseInt(env(k, strconv.FormatInt(def, 10)), 10, 64)
	if err != nil || v < 0 {
		log.Fatalf("%s: not a number", k)
	}
	return v
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := middleware.Dial(must("LND_GRPC"), must("LND_TLSCERT"), must("LND_MACAROON"))
	if err != nil {
		logger.Fatal(err)
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = secret("DATABASE_URL")
	}
	db, err := sources.OpenDB(ctx, dbURL)
	if err != nil {
		logger.Fatal(err)
	}
	ledger, err := policy.OpenLedger(must("LEDGER"), nil)
	if err != nil {
		logger.Fatal(err)
	}
	ratio, err := strconv.ParseFloat(env("FEE_RATIO", "0.0001"), 64)
	if err != nil || ratio < 0 || ratio > 0.01 {
		logger.Fatal("FEE_RATIO: between 0 and 0.01")
	}
	p := &policy.Policy{
		MaxPerOpSat:       num("MAX_PER_OP_SAT", 100_000_000),
		DailySat:          num("DAILY_SAT", 200_000_000),
		ReserveSat:        num("RESERVE_SAT", 50_000_000),
		MaxPendingReverse: int(num("MAX_PENDING_REVERSE", 5)),
		FeeRatio:          ratio,
		MinFeeSat:         num("MIN_FEE_SAT", 121),
		MaxFeeRate:        num("MAX_FEE_RATE", 25),
		RequiredConfs:     num("REQUIRED_CONFS", 3),
		Swaps:             db,
		Invoices:          sources.Invoices{Conn: conn},
		Chain: sources.Chain{URL: must("CHAIN_RPC_URL"), User: must("CHAIN_RPC_USER"),
			Password: secret("CHAIN_RPC_PASSWORD")},
		Wallet: sources.Wallet{Conn: conn},
		LSP:    sources.LSP{Path: must("LSP_STATE")},
		Ledger: ledger,
	}
	if url := os.Getenv("ALERT_URL"); url != "" {
		p.Alert = func(method, reason string) {
			go func() {
				b, _ := json.Marshal(map[string]string{"event": "guard_refusal", "method": method, "reason": reason})
				c := http.Client{Timeout: 5 * time.Second}
				if r, err := c.Post(url, "application/json", bytes.NewReader(b)); err == nil {
					r.Body.Close()
				}
			}()
		}
	}
	logger.Printf("kiloswap-guard: per op %d sat, daily %d sat, LSP reserve %d sat, %d pending reverse swaps",
		p.MaxPerOpSat, p.DailySat, p.ReserveSat, p.MaxPendingReverse)
	middleware.Run(ctx, conn, p, logger)
}
