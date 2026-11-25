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
	stdnet "net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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

// tiers reads "maxsat:confs,..." (CONF_TIERS).
func tiers(k, def string) []policy.ConfTier {
	var out []policy.ConfTier
	for _, part := range strings.Split(env(k, def), ",") {
		var t policy.ConfTier
		if _, err := fmt.Sscanf(strings.TrimSpace(part), "%d:%d", &t.MaxSat, &t.Confs); err != nil || t.Confs < 1 {
			log.Fatalf("%s: maxsat:confs,...", k)
		}
		out = append(out, t)
	}
	return out
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
	// launch mode (the default): 0.1 BTC per operation and 0.25 BTC a day;
	// LAUNCH_MODE=0 gives the later 1 BTC and 2 BTC (MAX_PER_OP_SAT and
	// DAILY_SAT override either way)
	perOp, daily := int64(10_000_000), int64(25_000_000)
	if env("LAUNCH_MODE", "1") == "0" {
		perOp, daily = 100_000_000, 200_000_000
	}
	p := &policy.Policy{
		MaxPerOpSat:         num("MAX_PER_OP_SAT", perOp),
		DailySat:            num("DAILY_SAT", daily),
		ReserveSat:          num("RESERVE_SAT", 50_000_000),
		MaxPendingReverse:   int(num("MAX_PENDING_REVERSE", 5)),
		MaxUnresolved:       int(num("MAX_UNRESOLVED", 2)),
		FeeRatio:            ratio,
		MinFeeSat:           num("MIN_FEE_SAT", 10),
		MaxFeeRate:          num("MAX_FEE_RATE", 25),
		RequiredConfs:       num("REQUIRED_CONFS", 3),
		MinLockBlocks:       num("MIN_LOCK_BLOCKS", 72),
		MaxLockBlocks:       num("MAX_LOCK_BLOCKS", 288),
		HoldExpiryDelta:     num("HOLD_EXPIRY_DELTA", 18),
		SafetyBlocks:        num("SAFETY_BLOCKS", 6),
		ClaimMarginSat:      num("CLAIM_MARGIN_SAT", 100),
		MinInvoiceSat:       num("MIN_INVOICE_SAT", 10_000),
		ClaimMarginBlocks:   num("CLAIM_MARGIN_BLOCKS", 60),
		MinSubTimeoutBlocks: num("MIN_SUB_TIMEOUT_BLOCKS", 144),
		ConfTiers:           tiers("CONF_TIERS", "1000000:3,10000000:6,100000000:12"),
		UrgentBlocks:        num("URGENT_BLOCKS", 24),
		UrgentFeeFraction:   0.1,
		MaxHoldParts:        int(num("MAX_HOLD_PARTS", 16)),
		ReverseMarginBlocks: num("REVERSE_MARGIN_BLOCKS", 36),
		MaxHoldCltv:         uint64(num("MAX_HOLD_CLTV", 432)),
		LockupFeeFactor:     1.25,
		MinReverseSat:       num("MIN_REVERSE_SAT", 10_000),
		MaxLockupsPerBlock:  int(num("MAX_LOCKUPS_PER_BLOCK", 2)),
		// option A (Kilombino, 25 Nov 2026): routing paid from the lockup's
		// surplus, at least 10 sat, at most FeeRatio of the invoice
		RoutingCap:           env("ROUTING_CAP", "1") == "1",
		RoutingFloorSat:      num("ROUTING_FLOOR_SAT", 10),
		ApprovedExpireBlocks: num("APPROVED_EXPIRE_BLOCKS", 72),
		MaxSubTimeoutBlocks:  num("MAX_SUB_TIMEOUT_BLOCKS", 1200),
		MaxInflightPayments:  int(num("MAX_INFLIGHT_PAYMENTS", 10)),
		HoldSlotTime:         time.Duration(num("HOLD_SLOT_MINUTES", 15)) * time.Minute,
		Net:                  net,
		Swaps:                db,
		LN:                   sources.LN{Conn: conn},
		Chain: sources.Chain{URL: must("CHAIN_RPC_URL"), User: must("CHAIN_RPC_USER"),
			Password: secret("CHAIN_RPC_PASSWORD")},
		Keys:   swapKeys,
		LSP:    sources.LSP{Path: must("LSP_STATE")},
		Ledger: ledger,
	}
	alertURL := os.Getenv("ALERT_URL")
	// alerts go out one at a time from a bounded queue; the same refusal or
	// stall repeated within a minute is sent once (the log keeps them all)
	alerts := make(chan map[string]any, 100)
	var seenMu sync.Mutex
	seen := map[string]time.Time{}
	go func() {
		c := http.Client{Timeout: 5 * time.Second}
		for m := range alerts {
			b, _ := json.Marshal(m)
			if r, err := c.Post(alertURL, "application/json", bytes.NewReader(b)); err == nil {
				r.Body.Close()
			}
		}
	}()
	p.Notify = func(event string, kv map[string]any) {
		logger.Printf("%s %v", event, kv)
		if alertURL == "" {
			return
		}
		if event == "guard_refusal" || event == "guard_stall" {
			key := fmt.Sprint(event, kv["method"], kv["reason"])
			seenMu.Lock()
			last, dup := seen[key]
			if !dup || time.Since(last) > time.Minute {
				seen[key] = time.Now()
			}
			if len(seen) > 1000 {
				seen = map[string]time.Time{}
			}
			seenMu.Unlock()
			if dup && time.Since(last) <= time.Minute {
				return
			}
		}
		m := map[string]any{"event": event}
		for k, v := range kv {
			m[k] = v
		}
		select {
		case alerts <- m:
		default: // the queue is full: dropped, the log has it
		}
	}
	logger.Printf("kiloswap-guard: per op %d sat, daily %d sat, LSP reserve %d sat, %d pending reverse swaps, %d unresolved lockups",
		p.MaxPerOpSat, p.DailySat, p.ReserveSat, p.MaxPendingReverse, p.MaxUnresolved)
	p.Lifecycle("guard_started")
	p.ReportLimits() // the operator sees the limits in force on every start
	// one full round of its own work (settle claims seen, claim paid
	// lockups, refund timed-out ones) before lnd can hand it anything: a
	// restart loop must not keep that work from ever running
	{
		tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		p.Tick(tctx)
		cancel()
	}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		alive := time.NewTicker(5 * time.Minute)
		defer alive.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-alive.C:
				p.Lifecycle("guard_alive") // the watcher alerts when these stop
			case <-t.C:
				tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				p.Tick(tctx)
				cancel()
			}
		}
	}()
	if addr := os.Getenv("API_ADDR"); addr != "" {
		// the API claims and refunds: never on every interface
		if h, _, err := stdnet.SplitHostPort(addr); err != nil || h == "" || h == "0.0.0.0" || h == "::" {
			logger.Fatal("API_ADDR: bind to the internal network's address, not all interfaces")
		}
		go api.Serve(ctx, addr, secret("API_TOKEN"), p, logger)
	}
	middleware.Run(ctx, conn, p, logger)
}
