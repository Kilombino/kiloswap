// Package policy is the hard limit on what the Kiloswap swap backend can make
// the main lnd do. lnd hands every request made with the backend's macaroon
// (which carries the custom caveat CaveatName) to the guard before running
// it, and refuses such requests outright while no middleware for that
// caveat is registered, so a stopped guard stops the backend, never the
// other way round.
//
// What a backend taken over could do, and what stops it:
//   - send on-chain coins anywhere: SendCoins only to the lockup address of
//     a reverse swap whose hold invoice lnd itself holds as paid (ACCEPTED,
//     at least the lockup's amount), for its exact amount, once, never
//     send_all, never unconfirmed coins, fee rate capped. The backend's
//     database is the backend's own word; lnd's invoice is not;
//   - pay any invoice: SendPaymentV2 only the invoice of a submarine swap
//     whose lockup transaction the node shows confirmed (RequiredConfs),
//     paying at least the invoice's amount to the swap's lockup address;
//     routing fee capped;
//   - drain by many small moves: at most MaxPerOpSat each and DailySat in
//     any 24 hours, on-chain sends and payments together;
//   - starve the LSP: on-chain sends and new hold invoices must leave
//     ReserveSat plus the LSP's open requests untouched;
//   - tie up coins with unpaid reverse swaps: at most MaxPendingReverse at
//     once;
//   - touch other services' invoices: Cancel/SettleInvoice only for hold
//     invoices it created (the bridge's are never its own);
//   - anything else: only the methods in Allowed pass.
package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
)

const (
	// CaveatName is the custom macaroon caveat the guard answers for.
	CaveatName = "kiloswap"
	// CaveatCondition is the condition baked into the backend's macaroon.
	CaveatCondition = "v1"
)

// Methods the backend may call. true: judged below; false: a read or a
// call that cannot move funds, passed as it is. Anything else is refused.
var Allowed = map[string]bool{
	"/lnrpc.Lightning/GetInfo":                     false,
	"/lnrpc.Lightning/AddInvoice":                  false, // receives, never pays
	"/lnrpc.Lightning/LookupInvoice":               false,
	"/lnrpc.Lightning/DecodePayReq":                false,
	"/lnrpc.Lightning/GetNodeInfo":                 false,
	"/lnrpc.Lightning/GetChanInfo":                 false,
	"/lnrpc.Lightning/DescribeGraph":               false,
	"/lnrpc.Lightning/QueryRoutes":                 false,
	"/lnrpc.Lightning/ListChannels":                false,
	"/lnrpc.Lightning/ListPeers":                   false,
	"/lnrpc.Lightning/WalletBalance":               false,
	"/lnrpc.Lightning/SubscribePeerEvents":         false,
	"/lnrpc.Lightning/SubscribeChannelEvents":      false,
	"/lnrpc.Lightning/ExportAllChannelBackups":     false,
	"/lnrpc.Lightning/SubscribeChannelBackups":     false,
	"/lnrpc.Lightning/NewAddress":                  false,
	"/lnrpc.Lightning/GetTransactions":             false,
	"/routerrpc.Router/TrackPaymentV2":             false,
	"/routerrpc.Router/ResetMissionControl":        false,
	"/invoicesrpc.Invoices/SubscribeSingleInvoice": false,
	"/lnrpc.Lightning/SendCoins":                   true,
	"/routerrpc.Router/SendPaymentV2":              true,
	"/invoicesrpc.Invoices/AddHoldInvoice":         true,
	"/invoicesrpc.Invoices/CancelInvoice":          true,
	"/invoicesrpc.Invoices/SettleInvoice":          true,
}

// ReverseSwap is what the guard reads of a reverse swap (LN to on-chain).
type ReverseSwap struct {
	LockupAddress string
	OnchainAmount int64 // sat
	PreimageHash  string
	Status        string
}

// Swap is what the guard reads of a submarine swap (on-chain to LN).
type Swap struct {
	Invoice       string
	InvoiceAmount int64 // sat
	Status        string
	LockupAddress string
	LockupTxID    string
}

// Invoices reads lnd's own record of an invoice (the guard's macaroon).
type Invoices interface {
	// Invoice returns its state (OPEN 0, SETTLED 1, CANCELED 2, ACCEPTED 3)
	// and what was paid to it, in msat.
	Invoice(ctx context.Context, hash []byte) (state int32, paidMsat int64, err error)
}

// Chain reads the node: what a transaction pays to an address, and how
// deep it is.
type Chain interface {
	PaidTo(ctx context.Context, txid, address string) (sat int64, confirmations int64, err error)
}

// InvoiceAccepted is lnd's state for a hold invoice paid and held.
const InvoiceAccepted = 3

// Swaps reads the swap backend's database (a read-only role).
type Swaps interface {
	ReverseByLockup(ctx context.Context, address string) ([]ReverseSwap, error)
	ReverseByHash(ctx context.Context, hash string) (bool, error)
	// Reverse swaps that may still lock coins: count, and the on-chain
	// amount of those not locked yet.
	PendingReverse(ctx context.Context) (count int, unlockedSat int64, err error)
	SwapByInvoice(ctx context.Context, invoice string) (*Swap, error)
}

// Balance is lnd's on-chain wallet as WalletBalance reports it.
type Balance struct {
	Confirmed, Locked, AnchorReserve int64
}

// Wallet reads lnd's on-chain balance (a macaroon of the guard's own).
type Wallet interface {
	Balance(ctx context.Context) (Balance, error)
}

// LSP reports the sats the LSP has promised to open channels with and not
// spent yet.
type LSP interface {
	Reserved() (int64, error)
}

// Statuses in which a reverse swap waits for our lockup.
// The backend sends the lockup before it records the invoice as paid, so
// swap.created counts too: what proves the payment is lnd's invoice.
var lockupStatuses = map[string]bool{"swap.created": true, "invoice.pending": true, "minerfee.paid": true}

// Statuses in which a submarine swap's invoice may be paid: its lockup
// confirmed, the payment going.
var payStatuses = map[string]bool{"transaction.confirmed": true, "invoice.pending": true}

// Policy holds the limits and what the guard knows.
type Policy struct {
	MaxPerOpSat       int64   // 100_000_000
	DailySat          int64   // 200_000_000, sends and payments together
	ReserveSat        int64   // 50_000_000 kept for the LSP
	MaxPendingReverse int     // 5
	FeeRatio          float64 // 0.0001, the most a payment may pay to route
	MinFeeSat         int64   // 121, the backend's own floor
	MaxFeeRate        int64   // sat/vB a lockup may pay
	RequiredConfs     int64   // 3, a submarine lockup's depth before paying
	Swaps             Swaps
	Invoices          Invoices
	Chain             Chain
	Wallet            Wallet
	LSP               LSP
	Ledger            *Ledger
	Now               func() time.Time
	// Alert, when set, hears every refusal (for the operator's bot).
	Alert func(method, reason string)

	mu      sync.Mutex
	pending map[uint64]string // lnd request id -> ledger key, until answered
}

type refusal struct{ msg string }

func (r refusal) Error() string { return "kiloswap guard: " + r.msg }

// Refused tells whether err is a refusal by the policy.
func Refused(err error) bool { var r refusal; return errors.As(err, &r) }

func refuse(format string, a ...any) error { return refusal{fmt.Sprintf(format, a...)} }

func (p *Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Stream says whether a streaming call may be opened at all.
func (p *Policy) Stream(uri string) error {
	if _, ok := Allowed[uri]; !ok {
		return refuse("%s is not allowed", uri)
	}
	return nil
}

// Intercept judges one request. condition is the caveat condition of the
// macaroon used; requestID ties the request to lnd's answer.
func (p *Policy) Intercept(ctx context.Context, condition string, requestID uint64, m *lnrpc.RPCMessage) error {
	err := p.intercept(ctx, condition, requestID, m)
	if err != nil && p.Alert != nil {
		p.Alert(m.GetMethodFullUri(), err.Error())
	}
	return err
}

func (p *Policy) intercept(ctx context.Context, condition string, requestID uint64, m *lnrpc.RPCMessage) error {
	if condition != CaveatCondition {
		return refuse("unknown caveat condition %q", condition)
	}
	uri := m.GetMethodFullUri()
	judged, ok := Allowed[uri]
	if !ok {
		return refuse("%s is not allowed", uri)
	}
	if !judged {
		return nil
	}
	switch uri {
	case "/lnrpc.Lightning/SendCoins":
		var r lnrpc.SendCoinsRequest
		if err := decode(m, &r); err != nil {
			return err
		}
		return p.sendCoins(ctx, requestID, &r)
	case "/routerrpc.Router/SendPaymentV2":
		var r lnrpc.SendPaymentRequest
		if err := decode(m, &r); err != nil {
			return err
		}
		return p.sendPayment(ctx, requestID, &r)
	case "/invoicesrpc.Invoices/AddHoldInvoice":
		var r lnrpc.AddHoldInvoiceRequest
		if err := decode(m, &r); err != nil {
			return err
		}
		return p.addHold(ctx, &r)
	case "/invoicesrpc.Invoices/CancelInvoice":
		var r lnrpc.CancelInvoiceMsg
		if err := decode(m, &r); err != nil {
			return err
		}
		return p.ownInvoice(ctx, r.PaymentHash)
	case "/invoicesrpc.Invoices/SettleInvoice":
		var r lnrpc.SettleInvoiceMsg
		if err := decode(m, &r); err != nil {
			return err
		}
		if len(r.Preimage) != 32 {
			return refuse("settle needs a 32-byte preimage")
		}
		h := sha256.Sum256(r.Preimage)
		return p.ownInvoice(ctx, h[:])
	}
	return refuse("%s has no check", uri)
}

// decode reads the request, refusing one with fields the guard does not
// know: a field added later must not slip past the checks.
func decode(m *lnrpc.RPCMessage, into proto.Message) error {
	if err := proto.Unmarshal(m.GetSerialized(), into); err != nil {
		return refuse("unreadable request: %v", err)
	}
	if hasUnknown(into.ProtoReflect()) {
		return refuse("request carries fields the guard does not know")
	}
	return nil
}

func hasUnknown(m protoreflect.Message) bool {
	return len(m.GetUnknown()) > 0
}

// available is what on-chain sends may still use: confirmed coins minus
// what lnd holds back, the LSP's reserve and its open requests.
func (p *Policy) available(ctx context.Context) (int64, error) {
	b, err := p.Wallet.Balance(ctx)
	if err != nil {
		return 0, refuse("wallet balance unknown: %v", err)
	}
	lsp, err := p.LSP.Reserved()
	if err != nil {
		return 0, refuse("LSP requests unknown: %v", err)
	}
	return b.Confirmed - b.Locked - b.AnchorReserve - p.ReserveSat - lsp, nil
}

func (p *Policy) sendCoins(ctx context.Context, requestID uint64, r *lnrpc.SendCoinsRequest) error {
	switch {
	case r.SendAll:
		return refuse("send_all is never allowed")
	case r.SpendUnconfirmed || r.MinConfs < 0:
		return refuse("only confirmed coins may be spent")
	case len(r.Outpoints) > 0:
		return refuse("coins may not be chosen")
	case r.Amount <= 0:
		return refuse("amount must be positive")
	case r.Amount > p.MaxPerOpSat:
		return refuse("%d sat is over the %d sat limit per operation", r.Amount, p.MaxPerOpSat)
	case r.SatPerVbyte > uint64(p.MaxFeeRate) || r.SatPerByte > p.MaxFeeRate || r.SatPerByte < 0:
		return refuse("fee rate over %d sat/vB", p.MaxFeeRate)
	}
	swaps, err := p.Swaps.ReverseByLockup(ctx, r.Addr)
	if err != nil {
		return refuse("swap database: %v", err)
	}
	var match *ReverseSwap
	for i, s := range swaps {
		if s.OnchainAmount == r.Amount && lockupStatuses[s.Status] {
			match = &swaps[i]
		}
	}
	if match == nil {
		return refuse("no reverse swap waits for %d sat at that address", r.Amount)
	}
	hash, err := hex.DecodeString(match.PreimageHash)
	if err != nil || len(hash) != 32 {
		return refuse("the swap has no usable payment hash")
	}
	state, paid, err := p.Invoices.Invoice(ctx, hash)
	if err != nil {
		return refuse("lnd invoice: %v", err)
	}
	if state != InvoiceAccepted {
		return refuse("the swap's hold invoice is not paid and held (state %d)", state)
	}
	if paid < r.Amount*1000 {
		return refuse("the hold invoice holds %d msat, less than the %d sat lockup", paid, r.Amount)
	}
	key := "lockup:" + r.Addr
	return p.spend(ctx, requestID, key, r.Amount, true)
}

func (p *Policy) sendPayment(ctx context.Context, requestID uint64, r *lnrpc.SendPaymentRequest) error {
	switch {
	case r.PaymentRequest == "":
		return refuse("only invoices may be paid")
	case len(r.Dest) > 0 || len(r.PaymentHash) > 0 || r.Amt != 0 || r.AmtMsat != 0:
		return refuse("only an invoice's own amount and destination")
	case r.Amp || r.AllowSelfPayment || len(r.DestCustomRecords) > 0 || len(r.FirstHopCustomRecords) > 0:
		return refuse("AMP, self-payments and custom records are not allowed")
	case len(r.RouteHints) > 0 || len(r.LastHopPubkey) > 0 || len(r.DestFeatures) > 0 || len(r.PaymentAddr) > 0:
		return refuse("the route may not be steered")
	case r.FeeLimitSat < 0 || r.FeeLimitMsat < 0:
		return refuse("a fee limit is required")
	}
	s, err := p.Swaps.SwapByInvoice(ctx, r.PaymentRequest)
	if err != nil {
		return refuse("swap database: %v", err)
	}
	if s == nil {
		return refuse("not the invoice of a Kiloswap swap")
	}
	if !payStatuses[s.Status] {
		return refuse("the swap is %s, not ready to pay", s.Status)
	}
	if s.InvoiceAmount <= 0 || s.InvoiceAmount > p.MaxPerOpSat {
		return refuse("%d sat is over the %d sat limit per operation", s.InvoiceAmount, p.MaxPerOpSat)
	}
	if s.LockupTxID == "" || s.LockupAddress == "" {
		return refuse("the swap has no lockup transaction")
	}
	got, confs, err := p.Chain.PaidTo(ctx, s.LockupTxID, s.LockupAddress)
	if err != nil {
		return refuse("node: %v", err)
	}
	if confs < p.RequiredConfs {
		return refuse("the lockup has %d confirmations, %d needed", confs, p.RequiredConfs)
	}
	if got < s.InvoiceAmount {
		return refuse("the lockup pays %d sat, less than the %d sat invoice", got, s.InvoiceAmount)
	}
	maxFeeMsat := int64(math.Ceil(math.Max(float64(s.InvoiceAmount)*1000*p.FeeRatio, float64(p.MinFeeSat*1000))))
	feeMsat := r.FeeLimitMsat
	if r.FeeLimitSat != 0 {
		if r.FeeLimitMsat != 0 {
			return refuse("one fee limit only")
		}
		feeMsat = r.FeeLimitSat * 1000
	}
	if feeMsat > maxFeeMsat+999 { // the backend rounds up to whole sat
		return refuse("routing fee limit %d msat over %d msat", feeMsat, maxFeeMsat)
	}
	key := "pay:" + strings.ToLower(r.PaymentRequest)
	return p.spend(ctx, requestID, key, s.InvoiceAmount, false)
}

// spend checks the daily limit (and, on-chain, the coins left), then holds
// the amount in the ledger. A key spends once: a lockup sent again would
// pay twice; a payment retried is the same payment.
func (p *Policy) spend(ctx context.Context, requestID uint64, key string, sat int64, onchain bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if e, ok := p.Ledger.Get(key); ok {
		if onchain && e.Done {
			return refuse("that lockup was already sent")
		}
		if !onchain {
			p.track(requestID, key)
			return nil // the same payment again: counted once
		}
	}
	if used := p.Ledger.Since(now.Add(-24 * time.Hour)); used+sat > p.DailySat {
		return refuse("%d sat would pass the daily limit (%d of %d used)", sat, used, p.DailySat)
	}
	if onchain {
		avail, err := p.available(ctx)
		if err != nil {
			return err
		}
		if sat > avail {
			return refuse("%d sat would cut into the LSP reserve (%d available)", sat, avail)
		}
	}
	if err := p.Ledger.Put(Entry{Key: key, Sat: sat, At: now, Done: onchain}); err != nil {
		return refuse("ledger: %v", err)
	}
	p.track(requestID, key)
	return nil
}

func (p *Policy) track(requestID uint64, key string) {
	if p.pending == nil {
		p.pending = map[uint64]string{}
	}
	p.pending[requestID] = key
}

// Payment states of a SendPaymentV2 update.
const (
	paymentSucceeded = 2
	paymentFailed    = 3
)

// Answered hears lnd's answer to a request: an on-chain send lnd refused,
// or a payment that failed, did not move anything, so it no longer counts
// against the daily limit (a lockup may be sent again, a payment retried).
func (p *Policy) Answered(requestID uint64, m *lnrpc.RPCMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key, ok := p.pending[requestID]
	if !ok {
		return
	}
	failed := m.GetIsError()
	done := failed || !m.GetStreamRpc()
	if strings.HasPrefix(key, "pay:") && !failed {
		var u lnrpc.Payment
		if proto.Unmarshal(m.GetSerialized(), &u) == nil {
			switch u.Status {
			case paymentFailed:
				failed, done = true, true
			case paymentSucceeded:
				done = true
			}
		}
	}
	if !done {
		return // payment updates in flight: wait for the last one
	}
	delete(p.pending, requestID)
	if failed {
		_ = p.Ledger.Delete(key)
	}
}

func (p *Policy) addHold(ctx context.Context, r *lnrpc.AddHoldInvoiceRequest) error {
	if len(r.Hash) != 32 {
		return refuse("a hold invoice needs a 32-byte hash")
	}
	sat := r.Value
	if r.ValueMsat != 0 {
		if r.Value != 0 {
			return refuse("one amount only")
		}
		sat = (r.ValueMsat + 999) / 1000
	}
	if sat <= 0 || sat > p.MaxPerOpSat {
		return refuse("%d sat is not within 1..%d", sat, p.MaxPerOpSat)
	}
	count, unlocked, err := p.Swaps.PendingReverse(ctx)
	if err != nil {
		return refuse("swap database: %v", err)
	}
	if count >= p.MaxPendingReverse {
		return refuse("%d reverse swaps pending, the limit is %d", count, p.MaxPendingReverse)
	}
	avail, err := p.available(ctx)
	if err != nil {
		return err
	}
	if unlocked+sat > avail {
		return refuse("%d sat more would not fit the coins left (%d promised, %d available)", sat, unlocked, avail)
	}
	if err := p.Ledger.AddHold(hex.EncodeToString(r.Hash)); err != nil {
		return refuse("ledger: %v", err)
	}
	return nil
}

func (p *Policy) ownInvoice(ctx context.Context, hash []byte) error {
	if len(hash) != 32 {
		return refuse("a 32-byte payment hash is required")
	}
	h := hex.EncodeToString(hash)
	if p.Ledger.HasHold(h) {
		return nil
	}
	ok, err := p.Swaps.ReverseByHash(ctx, h)
	if err != nil {
		return refuse("swap database: %v", err)
	}
	if !ok {
		return refuse("not a Kiloswap hold invoice")
	}
	return nil
}
