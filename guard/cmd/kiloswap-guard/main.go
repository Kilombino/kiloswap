// kiloswap-guard is lnd RPC middleware that bounds what the Kiloswap swap
// backend may make the main lnd do (see internal/policy). Configuration by
// environment:
//
//	LND_GRPC        host:port of lnd's gRPC (e.g. lnd:10009)
//	LND_TLSCERT     lnd's tls.cert
//	LND_MACAROON    the guard's macaroon (RegisterRPCMiddleware, WalletBalance,
//	                LookupInvoice, DecodePayReq, SettleInvoice, TrackPaymentV2,
//	                NewAddress)
//	CHAIN_RPC_URL   the node's JSON-RPC (http://host:8332/), with
//	CHAIN_RPC_USER / CHAIN_RPC_PASSWORD (or _FILE): the guard's own user
//	                (getrawtransaction, gettxout, getblockcount, getblockhash,
//	                getblock, gettxspendingprevout, estimatesmartfee,
//	                sendrawtransaction)
//	NETWORK         mainnet or regtest
//	SWAP_MNEMONIC_FILE  the swaps' server key (the backend's former seed.dat);
//	SWAP_KEY_PATH   its derivation path (Boltz: m/0/0)
//	DATABASE_URL    postgres://kiloswap_guard:…@postgres/boltz (read-only role)
//	LSP_STATE       the LSP's state.json (read-only mount)
//	LEDGER          where the guard keeps its ledger
//	API_ADDR        where the backend asks for claims and refunds (:8091), with
//	API_TOKEN_FILE  the shared bearer token
//	ALERT_URL       optional: each refusal and movement is POSTed here as JSON
//	MAX_PER_OP_SAT, DAILY_SAT, RESERVE_SAT, MAX_PENDING_REVERSE, FEE_RATIO,
//	MIN_FEE_SAT, MAX_FEE_RATE, REQUIRED_CONFS, MIN_LOCK_BLOCKS,
//	MAX_LOCK_BLOCKS, HOLD_EXPIRY_DELTA, SAFETY_BLOCKS, CLAIM_MARGIN_SAT  limits
//
//	kiloswap-guard xpub     prints the extended public key the backend gets
//	kiloswap-guard newseed  prints a new mnemonic (a new install)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/tyler-smith/go-bip39"

	"github.com/Kilombino/kiloswap/guard/internal/api"
	"github.com/Kilombino/kiloswap/guard/internal/keys"
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
	if len(os.Args) > 1 && os.Args[1] == "newseed" {
		ent, err := bip39.NewEntropy(256)
		if err != nil {
			logger.Fatal(err)
		}
		words, err := bip39.NewMnemonic(ent)
		if err != nil {
			logger.Fatal(err)
		}
		fmt.Println(words)
		return
	}
	swapKeys, err := keys.FromMnemonicFile(must("SWAP_MNEMONIC_FILE"), env("SWAP_KEY_PATH", "m/0/0"))
	if err != nil {
		logger.Fatalf("swap key: %v", err)
	}
	if len(os.Args) > 1 && os.Args[1] == "xpub" {
		x, err := swapKeys.AccountXpub()
		if err != nil {
			logger.Fatal(err)
		}
		fmt.Println(x)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	net := &chaincfg.MainNetParams
	switch env("NETWORK", "mainnet") {
	case "mainnet":
	case "regtest":
		net = &chaincfg.RegressionNetParams
	default:
		logger.Fatal("NETWORK: mainnet or regtest")
	}
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
		MinLockBlocks:     num("MIN_LOCK_BLOCKS", 12),
		MaxLockBlocks:     num("MAX_LOCK_BLOCKS", 4320),
		HoldExpiryDelta:   num("HOLD_EXPIRY_DELTA", 18),
		SafetyBlocks:      num("SAFETY_BLOCKS", 6),
		ClaimMarginSat:    num("CLAIM_MARGIN_SAT", 100),
		Net:               net,
		Swaps:             db,
		LN:                sources.LN{Conn: conn},
		Chain: sources.Chain{URL: must("CHAIN_RPC_URL"), User: must("CHAIN_RPC_USER"),
			Password: secret("CHAIN_RPC_PASSWORD")},
		Keys:   swapKeys,
		LSP:    sources.LSP{Path: must("LSP_STATE")},
		Ledger: ledger,
	}
	alertURL := os.Getenv("ALERT_URL")
	p.Notify = func(event string, kv map[string]any) {
		logger.Printf("%s %v", event, kv)
		if alertURL == "" {
			return
		}
		m := map[string]any{"event": event}
		for k, v := range kv {
			m[k] = v
		}
		go func() {
			b, _ := json.Marshal(m)
			c := http.Client{Timeout: 5 * time.Second}
			if r, err := c.Post(alertURL, "application/json", bytes.NewReader(b)); err == nil {
				r.Body.Close()
			}
		}()
	}
	logger.Printf("kiloswap-guard: per op %d sat, daily %d sat, LSP reserve %d sat, %d pending reverse swaps",
		p.MaxPerOpSat, p.DailySat, p.ReserveSat, p.MaxPendingReverse)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				p.Tick(tctx)
				cancel()
			}
		}
	}()
	if addr := os.Getenv("API_ADDR"); addr != "" {
		go api.Serve(ctx, addr, secret("API_TOKEN"), p, logger)
	}
	middleware.Run(ctx, conn, p, logger)
}
