// Package policy is the hard limit on what the Kiloswap swap backend can make
// the main lnd do. lnd hands every request made with the backend's macaroon
// (which carries the custom caveat CaveatName) to the guard before running
// it, and refuses such requests outright while no middleware for that
// caveat is registered, so a stopped guard stops the backend, never the
// other way round.
//
// The threat is the backend (and its database) taken over. Nothing it
// writes is trusted on its own: amounts and hashes come from lnd (the
// decoded invoice, lnd's own invoice record) and from the chain; swap
// outputs are rebuilt by the guard from the server key it alone holds (the
// backend only has the extended public key, so it can neither sign a swap
// spend nor take a lockup through the key path).
//
//   - send on-chain coins: SendCoins only to the lockup address of a reverse
//     swap whose output the guard rebuilt (our refund key, the user's claim
//     key, the hold invoice's hash, a sane timeout), whose hold invoice the
//     backend created through the guard and lnd holds as paid with every
//     HTLC expiring well after the timeout; once per hash, exact amount,
//     confirmed coins only, an explicit fee rate under the cap;
//   - pay an invoice: SendPaymentV2 only an invoice whose amount and hash lnd
//     decodes, for a submarine swap whose lockup output (rebuilt: our claim
//     key, that hash, the user's refund key, the timeout) the node shows
//     unspent with RequiredConfs confirmations, paying at least the invoice
//     plus a claim margin; each lockup pays once; cltv_limit leaves time to
//     claim before the timeout; routing fee capped; no steering;
//   - drain by many moves: at most MaxPerOpSat each and DailySat in any 24
//     hours, sends and payments together. An amount is given back only when
//     lnd itself reports the send refused or the payment FAILED;
//   - starve the LSP: sends and new hold invoices must leave ReserveSat, the
//     LSP's open requests and every send not yet answered untouched;
//   - tie up coins: at most MaxPendingReverse hold invoices open at once, by
//     the guard's own count;
//   - touch other invoices: lookups, cancels and settles only for invoices
//     the backend created through the guard; a hold invoice whose lockup is
//     still out cannot be cancelled;
//   - anything else: only the methods in Allowed pass.
//
// Server-side spends (claiming submarine lockups, refunding reverse
// lockups) and settling hold invoices when the user claims are done by the
// guard itself (spend.go), with the key the backend does not have.
package policy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/swapscript"
)

const (
	// CaveatName is the custom macaroon caveat the guard answers for.
	CaveatName = "kiloswap"
	// CaveatCondition is the condition baked into the backend's macaroon.
	CaveatCondition = "v1"
)

// Methods the backend may call. true: judged below; false: a read or a
// call that cannot move funds nor leak another service's secrets, passed
// as it is. Anything else is refused.
var Allowed = map[string]bool{
	"/lnrpc.Lightning/GetInfo":                     false,
	"/lnrpc.Lightning/AddInvoice":                  false, // receives; its hash is recorded
	"/lnrpc.Lightning/DecodePayReq":                false,
	"/lnrpc.Lightning/GetNodeInfo":                 false,
	"/lnrpc.Lightning/GetChanInfo":                 false,
	"/lnrpc.Lightning/QueryRoutes":                 false,
	"/lnrpc.Lightning/ListChannels":                false, // privacy: see README
	"/lnrpc.Lightning/ListPeers":                   false, // privacy: see README
	"/lnrpc.Lightning/WalletBalance":               false,
	"/lnrpc.Lightning/SubscribePeerEvents":         false,
	"/lnrpc.Lightning/SubscribeChannelEvents":      false,
	"/lnrpc.Lightning/ExportAllChannelBackups":     false,
	"/lnrpc.Lightning/SubscribeChannelBackups":     false,
	"/lnrpc.Lightning/NewAddress":                  false,
	"/lnrpc.Lightning/GetTransactions":             false, // the lnd wallet provider; privacy: see README
	"/routerrpc.Router/TrackPaymentV2":             false,
	"/lnrpc.Lightning/LookupInvoice":               true,
	"/invoicesrpc.Invoices/SubscribeSingleInvoice": true,
	"/lnrpc.Lightning/SendCoins":                   true,
	"/routerrpc.Router/SendPaymentV2":              true,
	"/invoicesrpc.Invoices/AddHoldInvoice":         true,
	"/invoicesrpc.Invoices/CancelInvoice":          true,
	"/invoicesrpc.Invoices/SettleInvoice":          true,
}

// ReverseSwap is what the backend's database says of a reverse swap (LN to
// on-chain): a claim to check, never a fact.
type ReverseSwap struct {
	ID            string
	LockupAddress string
	OnchainAmount int64 // sat
	PreimageHash  string
	ClaimKey      string // the user's, hex
	KeyIndex      int64
	Timeout       int64
	Version       int
}

// Swap is what the backend's database says of a submarine swap.
type Swap struct {
	ID           string
	PreimageHash string
	RefundKey    string // the user's, hex
	KeyIndex     int64
	Timeout      int64
	LockupTxID   string
	LockupVout   int64
	Version      int
}

// InvoiceInfo is lnd's own record of an invoice.
type InvoiceInfo struct {
	Memo      string
	State     int32
	PaidMsat  int64
	ValueMsat int64
	Created   time.Time
	// Expiry heights of the HTLCs held (accepted, not cancelled)
	HTLCExpiries []int32
}

// PayReq is lnd's decoding of an invoice.
type PayReq struct {
	Hash       []byte
	AmountMsat int64
}

// Lightning is lnd through the guard's own macaroon.
type Lightning interface {
	// LookupInvoice returns nil, nil if lnd has no such invoice.
	LookupInvoice(ctx context.Context, hash []byte) (*InvoiceInfo, error)
	DecodePayReq(ctx context.Context, payReq string) (*PayReq, error)
	Balance(ctx context.Context) (Balance, error)
	SettleInvoice(ctx context.Context, preimage []byte) error
	CancelInvoice(ctx context.Context, hash []byte) error
	// TrackPayment returns the payment's status (1 in flight, 2 succeeded,
	// 3 failed) and its preimage once succeeded; 0 if lnd has no payment.
	TrackPayment(ctx context.Context, hash []byte) (int32, string, int64, error)
	NewAddress(ctx context.Context) (string, error)
	// WalletOwns tells whether txid, mined at height, was sent by lnd's own
	// wallet.
	WalletOwns(ctx context.Context, txid string, height int64) (bool, error)
	// WalletPaid finds among all of lnd's wallet transactions from
	// startHeight on (unconfirmed too) one paying sat to address.
	WalletPaid(ctx context.Context, pkScript []byte, sat int64, startHeight int64) (string, uint32, int64, bool, error)
	// ListUnspent lists lnd's spendable coins with at least minConfs
	// confirmations (lnd leaves out the ones it has leased).
	ListUnspent(ctx context.Context, minConfs int32) ([]Utxo, error)
}

// Utxo is one of lnd's wallet coins.
type Utxo struct {
	TxID  string
	Vout  uint32
	Sat   int64
	Confs int64
	Type  int32 // lnrpc.AddressType: 0 p2wkh, 1 np2wkh, 4 p2tr
}

// Output is one transaction output as the node sees it.
type Output struct {
	PkScript      []byte
	Sat           int64
	Confirmations int64
}

// Spend is the transaction spending an outpoint, as the node sees it.
type Spend struct {
	TxID          string
	Witness       [][]byte // of the input spending the outpoint
	Confirmations int64
}

// Chain is the node through the guard's own RPC user.
type Chain interface {
	Output(ctx context.Context, txid string, vout uint32) (*Output, error)
	// Outputs lists a transaction's outputs and its confirmations.
	Outputs(ctx context.Context, txid string) ([]Output, error)
	Unspent(ctx context.Context, txid string, vout uint32) (bool, error)
	Height(ctx context.Context) (int64, error)
	// Spender finds the transaction spending txid:vout in the mempool or
	// in the blocks from fromHeight on; nil if none.
	Spender(ctx context.Context, txid string, vout uint32, fromHeight int64) (*Spend, error)
	FeeRate(ctx context.Context) (float64, error) // sat/vB
	Broadcast(ctx context.Context, rawHex string) (string, error)
	Confirmations(ctx context.Context, txid string) (int64, error)
	// FindOutputs lists the outputs paying sat to pkScript in the blocks
	// from fromHeight on.
	FindOutputs(ctx context.Context, pkScript []byte, sat int64, fromHeight, toHeight int64) ([]Found, error)
}

// Found is an output FindOutputs found.
type Found struct {
	TxID   string
	Vout   uint32
	Height int64
}

// Keys are the swaps' server keys, held by the guard alone.
type Keys interface {
	Public(index uint32) (*btcec.PublicKey, error)
	Private(index uint32) (*btcec.PrivateKey, error)
}

// Swaps reads the swap backend's database (a read-only role): what the
// backend claims, to be checked against lnd, the chain and our keys.
type Swaps interface {
	ReverseByLockup(ctx context.Context, address string) ([]ReverseSwap, error)
	SwapByInvoice(ctx context.Context, invoice string) (*Swap, error)
}

// Balance is lnd's on-chain wallet as WalletBalance reports it.
type Balance struct {
	Confirmed, Locked, AnchorReserve int64
	Unconfirmed                      int64 // pending (our own change too): never counted, only told
}

// LSP reports the sats the LSP has promised to open channels with and not
// spent yet.
type LSP interface {
	Reserved() (int64, error)
}

// InvoiceAccepted is lnd's state for a hold invoice paid and held.
const (
	InvoiceOpen     = 0
	InvoiceSettled  = 1
	InvoiceCanceled = 2
	InvoiceAccepted = 3
)

// Policy holds the limits and what the guard knows.
type Policy struct {
	MaxPerOpSat       int64   // 100_000_000
	DailySat          int64   // 200_000_000, sends and payments together
	ReserveSat        int64   // 50_000_000 kept for the LSP
	MaxPendingReverse int     // 5
	FeeRatio          float64 // 0.0001, the most a payment may pay to route
	MinFeeSat         int64   // 121, the backend's own floor
	MaxFeeRate        int64   // sat/vB a lockup or a guard spend may pay
	RequiredConfs     int64   // 3, a submarine lockup's depth before paying
	MinLockBlocks     int64   // a reverse lockup's timeout at least this far
	MaxLockBlocks     int64   // and at most this far
	HoldExpiryDelta   int64   // lnd's invoices.holdexpirydelta (18)
	SafetyBlocks      int64   // margin to settle or claim before a deadline
	ClaimMarginSat    int64   // a submarine lockup over the invoice by this
	// MinInvoiceSat is the smallest submarine invoice paid (smaller ones
	// cost more to claim than they bring).
	MinInvoiceSat int64
	// ClaimMarginBlocks is how many blocks before a submarine swap's
	// timeout the payment must be final (cltv_limit) so that the guard's
	// claim confirms even against a peer that holds the HTLC to its expiry.
	ClaimMarginBlocks int64
	// MinSubTimeoutBlocks is the least a submarine swap's timeout may be
	// ahead when its invoice is paid.
	MinSubTimeoutBlocks int64
	// ConfTiers scale a submarine lockup's required depth with its amount
	// (a reorg with ~50% of the hash rate is cheaper for small amounts):
	// the first tier whose MaxSat covers the lockup applies.
	ConfTiers []ConfTier
	// UrgentBlocks: a claim this close to the timeout pays whatever it
	// takes, up to UrgentFeeFraction of the lockup, past MaxFeeRate.
	UrgentBlocks      int64
	UrgentFeeFraction float64
	// MaxHoldParts is the most HTLCs a hold invoice may hold when its
	// lockup is sent (each one takes a slot the bridge and LSP share).
	MaxHoldParts int
	// ReverseMarginBlocks: every HTLC of a hold invoice must outlast the
	// lockup's timeout by HoldExpiryDelta plus this, to settle after a
	// claim that comes at the last moment.
	ReverseMarginBlocks int64
	// MaxHoldCltv is the most cltv_expiry a hold invoice may ask.
	MaxHoldCltv uint64
	// MaxUnresolved caps lockups allowed and not seen on chain yet.
	MaxUnresolved int
	// HoldSlotTime is how long an unpaid hold invoice counts against
	// MaxPendingReverse.
	HoldSlotTime time.Duration
	// MaxSubTimeoutBlocks is the most a submarine swap's timeout may be
	// ahead (a long one ties the payment's outcome up for days).
	MaxSubTimeoutBlocks int64
	// MaxInflightPayments caps payments made and not claimed yet.
	MaxInflightPayments int
	// MaxInflightSat caps their total value (independent of DailySat).
	MaxInflightSat int64
	// ApprovedExpireBlocks: a lockup allowed but never found in lnd's wallet
	// after this many blocks is given up (its reserve released, its hold
	// invoice cancellable).
	ApprovedExpireBlocks int64
	// LockupFeeFactor caps a lockup's fee rate at this times the node's
	// estimate (at least 2 sat/vB): no paying the miners to grief.
	LockupFeeFactor float64
	// MinReverseSat is the smallest reverse swap (its hold invoice; the
	// backend's minSwapAmount): tiny ones cost the operator more in miner
	// fees than they could ever pay.
	MinReverseSat int64
	// MaxLockupsPerBlock caps the lockups allowed at one height (a loop of
	// swaps never claimed spends the operator's coins on fees one block at
	// a time, not all at once).
	MaxLockupsPerBlock int
	// RoutingCap (on by default, option A decided 25 Nov 2026): a submarine
	// payment's routing fee limit is lowered, by replacing the request, to
	// min(max(FeeRatio of the invoice, MinFeeSat), max(lockup surplus over
	// the invoice and the claim, RoutingFloorSat)).
	RoutingCap      bool
	RoutingFloorSat int64
	Net             *chaincfg.Params
	Swaps           Swaps
	LN              Lightning
	Chain           Chain
	Keys            Keys
	LSP             LSP
	Ledger          *Ledger
	Now             func() time.Time
	// Notify, when set, hears every refusal and every movement allowed or
	// made (for the operator's bot): event, then key/values.
	Notify func(event string, kv map[string]any)

	mu          sync.Mutex // judgments that touch the ledger, one at a time
	smu         sync.Mutex // the guard's own spends (claims, refunds, bumps), one at a time
	pmu         sync.Mutex
	pending     map[uint64]pendingReq
	claimErr    map[string]string // last claim error per payment hash
	calls       map[string][]time.Time
	lastBal     Balance
	chosen      map[string]time.Time // coins given to lockups lately (lnd may not have spent them yet)
	epoch       uint64               // bumped on each registration with lnd (ids start over)
	lastSaveErr string
	confCache   map[string]confAt
	replace     map[uint64][]byte
}

// ConfTier is one step of ConfTiers.
type ConfTier struct {
	MaxSat int64
	Confs  int64
}

// confsFor is the depth a submarine lockup of sat needs.
func (p *Policy) confsFor(sat int64) int64 {
	c := p.RequiredConfs
	for _, t := range p.ConfTiers {
		if sat <= t.MaxSat {
			return max(c, t.Confs)
		}
	}
	if n := len(p.ConfTiers); n > 0 {
		return max(c, p.ConfTiers[n-1].Confs)
	}
	return c
}

// shallowExposure is what payments not claimed deep yet stand on: the sum
// of their lockups that are less deep than the last tier, and how many
// payments are not claimed yet (caller holds p.mu).
func (p *Policy) shallowExposure(ctx context.Context) (int64, int) {
	deep := p.RequiredConfs
	if n := len(p.ConfTiers); n > 0 {
		deep = max(deep, p.ConfTiers[n-1].Confs)
	}
	type lk struct {
		tx   string
		vout uint32
		sat  int64
	}
	var check []lk
	inflight := 0
	p.Ledger.Lock()
	for _, x := range p.Ledger.Payments {
		switch {
		case x.State == PayInflight || x.State == PayUnknown || x.State == PaySucceeded:
			inflight++
			check = append(check, lk{x.LockupTx, x.Vout, x.LockupSat})
		case x.State == PayClaimed && !x.Bump.Done:
			check = append(check, lk{x.LockupTx, x.Vout, x.LockupSat})
		}
	}
	p.Ledger.Unlock()
	var s int64
	height, herr := p.Chain.Height(ctx)
	for _, c := range check {
		confs, err := p.cachedConfs(ctx, c.tx, height, herr == nil)
		if err != nil || confs < deep {
			s += c.sat // unknown counts as shallow
		}
	}
	return s, inflight
}

type confAt struct {
	confs, height int64
}

// cachedConfs asks a transaction's depth once per block (judgments under the
// lock must not make one call per payment each time).
func (p *Policy) cachedConfs(ctx context.Context, txid string, height int64, useCache bool) (int64, error) {
	p.pmu.Lock()
	if p.confCache == nil {
		p.confCache = map[string]confAt{}
	}
	c, ok := p.confCache[txid]
	p.pmu.Unlock()
	if useCache && ok && c.height == height {
		return c.confs, nil
	}
	confs, err := p.Chain.Confirmations(ctx, txid)
	if err != nil {
		return 0, err
	}
	p.pmu.Lock()
	if len(p.confCache) > 1000 {
		p.confCache = map[string]confAt{}
	}
	p.confCache[txid] = confAt{confs, height}
	p.pmu.Unlock()
	return confs, nil
}

// ClaimVbytes is what the guard's claim of a submarine lockup weighs at
// most (one taproot script-path input with preimage, one output).
const ClaimVbytes = 151

// A submarine payment goes in at most MaxPayParts parts, with a cltv_limit of
// at most MaxPayCltv blocks (two days): the guard lowers larger ones.
const (
	MaxPayParts = 4
	MaxPayCltv  = 288
)

// What a reverse swap's hold invoice must pay over the lockup, at the
// lockup's own fee rate: the lockup transaction (LockupVbytes), a refund if
// the user never claims (RefundVbytes) and RefundDustSat (the refund's
// output and rounding). The backend charges the same as the reverse
// "lockup" miner fee.
const (
	LockupVbytes  = 155
	RefundVbytes  = 111
	RefundDustSat = 330
)

// ReverseCostSat is what a lockup at rate costs the operator beyond its
// amount, at worst (sent and later refunded).
func ReverseCostSat(rate float64) int64 {
	return int64(math.Ceil(float64(LockupVbytes+RefundVbytes)*rate)) + RefundDustSat
}

type pendingReq struct {
	epoch  uint64 // the registration with lnd it belongs to
	method string
	hash   string
	sat    int64
	at     time.Time
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

func (p *Policy) notify(event string, kv map[string]any) {
	if p.Notify != nil {
		p.Notify(event, kv)
	}
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
	if err != nil {
		p.notify("guard_refusal", map[string]any{"method": m.GetMethodFullUri(), "reason": err.Error()})
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
	if uri == "/lnrpc.Lightning/AddInvoice" {
		return p.addInvoice(requestID, m)
	}
	if lim, heavy := heavyCalls[uri]; heavy && !p.rate(uri, lim) {
		return refuse("%s more than %d times a minute", uri, lim)
	}
	if uri == "/lnrpc.Lightning/GetTransactions" {
		if err := p.judgeGetTransactions(ctx, m); err != nil {
			return err
		}
	}
	if uri == "/lnrpc.Lightning/NewAddress" && !p.rateWindow(uri, 30, time.Hour) {
		return refuse("NewAddress more than 30 times an hour")
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
		return p.addHold(ctx, requestID, &r)
	case "/invoicesrpc.Invoices/CancelInvoice":
		var r lnrpc.CancelInvoiceMsg
		if err := decode(m, &r); err != nil {
			return err
		}
		return p.cancel(requestID, r.PaymentHash)
	case "/invoicesrpc.Invoices/SettleInvoice":
		var r lnrpc.SettleInvoiceMsg
		if err := decode(m, &r); err != nil {
			return err
		}
		if len(r.Preimage) != 32 {
			return refuse("settle needs a 32-byte preimage")
		}
		h := sha256.Sum256(r.Preimage)
		return p.settle(requestID, h[:])
	case "/lnrpc.Lightning/LookupInvoice":
		var r lnrpc.PaymentHash
		if err := decode(m, &r); err != nil {
			return err
		}
		hash := r.RHash
		if r.RHashStr != "" {
			if len(hash) > 0 {
				return refuse("one hash only")
			}
			var err error
			if hash, err = hex.DecodeString(r.RHashStr); err != nil {
				return refuse("unreadable hash")
			}
		}
		return p.ownLookup(hash)
	case "/invoicesrpc.Invoices/SubscribeSingleInvoice":
		var r lnrpc.SubscribeSingleInvoiceRequest
		if err := decode(m, &r); err != nil {
			return err
		}
		return p.ownLookup(r.RHash)
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

// available is what on-chain sends may still use (caller holds p.mu and
// the ledger lock): confirmed coins minus what lnd holds back, the LSP's
// reserve and open requests, the sends allowed and not answered yet, and
// what open hold invoices without a lockup may still ask for.
func (p *Policy) available(ctx context.Context) (int64, error) {
	b, err := p.LN.Balance(ctx)
	if err != nil {
		return 0, refuse("wallet balance unknown: %v", err)
	}
	lsp, err := p.LSP.Reserved()
	if err != nil {
		return 0, refuse("LSP requests unknown: %v", err)
	}
	var inflight int64
	for _, l := range p.Ledger.Lockups {
		if l.State == LockApproved {
			inflight += l.Sat
		}
	}
	p.pmu.Lock()
	p.lastBal = b
	p.pmu.Unlock()
	return b.Confirmed - b.Locked - b.AnchorReserve - p.ReserveSat - lsp - inflight, nil
}

// balanceNote says how the last balance read splits into confirmed and
// pending (a lockup's change is pending until the next block).
func (p *Policy) balanceNote() string {
	p.pmu.Lock()
	b := p.lastBal
	p.pmu.Unlock()
	if b.Unconfirmed > 0 {
		return fmt.Sprintf("; confirmado %d, pendiente %d — se resuelve con el próximo bloque si lo pendiente es cambio propio", b.Confirmed, b.Unconfirmed)
	}
	return fmt.Sprintf("; confirmado %d", b.Confirmed)
}

// promised is what open hold invoices without a lockup may still lock.
func (p *Policy) promised() int64 {
	var s int64
	for h, x := range p.Ledger.Holds {
		if x.State != "open" && x.State != HoldCanceling && x.State != HoldCreating {
			continue
		}
		if _, locked := p.Ledger.Lockups[h]; !locked {
			s += x.Sat
		}
	}
	return s
}

// openHolds counts the open hold invoices and those allowed whose creation
// lnd has not confirmed yet (so two at once cannot both pass the limit).
func (p *Policy) openHolds() int {
	n := 0
	for h, x := range p.Ledger.Holds {
		if x.State == HoldCreating {
			n++
			continue
		}
		if x.State != "open" {
			continue
		}
		// an unpaid invoice holds a slot only for a while (its amount stays
		// promised until it is closed): slots are not for squatting
		_, locked := p.Ledger.Lockups[h]
		if locked || p.HoldSlotTime <= 0 || p.now().Sub(x.At) < p.HoldSlotTime {
			n++
		}
	}
	return n
}

func (p *Policy) userKey(s string) (*btcec.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(b)
}

func (p *Policy) addrScript(address string) ([]byte, error) {
	a, err := btcutil.DecodeAddress(address, p.Net)
	if err != nil {
		return nil, err
	}
	if !a.IsForNet(p.Net) {
		return nil, errors.New("address of another network")
	}
	return txscript.PayToAddrScript(a)
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
	case r.Amount < p.MinReverseSat/2:
		// a lockup is the hold invoice less the fees: never under half the
		// smallest reverse swap
		return refuse("%d sat is under the %d sat minimum of a lockup", r.Amount, p.MinReverseSat/2)
	case r.Amount > p.MaxPerOpSat:
		return refuse("%d sat is over the %d sat limit per operation", r.Amount, p.MaxPerOpSat)
	case r.TargetConf != 0:
		return refuse("a fee rate is required, not a confirmation target")
	case r.SatPerByte < 0 || (r.SatPerVbyte > 0) == (r.SatPerByte > 0):
		return refuse("exactly one positive fee rate is required")
	case r.SatPerVbyte > uint64(p.MaxFeeRate) || r.SatPerByte > p.MaxFeeRate:
		return refuse("fee rate over %d sat/vB", p.MaxFeeRate)
	case len(r.Label) > 100:
		return refuse("label over 100 characters")
	case r.MinConfs > 6:
		return refuse("min_confs over 6")
	case r.CoinSelectionStrategy != 0:
		return refuse("the default coin selection only")
	}
	if p.LockupFeeFactor > 0 {
		rate := float64(r.SatPerVbyte) + float64(r.SatPerByte)
		est, err := p.Chain.FeeRate(ctx)
		if err != nil || est < 1 {
			est = 1
		}
		if cap := math.Max(2, math.Ceil(est*p.LockupFeeFactor)); rate > cap {
			return refuse("fee rate %.0f sat/vB over %.0f (%.1fx the node's estimate)", rate, cap, p.LockupFeeFactor)
		}
		// nor so low that the lockup sits unconfirmed past its timeout
		if floor := math.Max(1, math.Floor(est*0.8)); rate < floor {
			return refuse("fee rate %.0f sat/vB under %.0f (the node's estimate)", rate, floor)
		}
	}
	if r.Addr != strings.ToLower(r.Addr) {
		return refuse("only lowercase (canonical) addresses")
	}
	want, err := p.addrScript(r.Addr)
	if err != nil {
		return refuse("unreadable address: %v", err)
	}
	rows, err := p.Swaps.ReverseByLockup(ctx, r.Addr)
	if err != nil {
		return refuse("swap database: %v", err)
	}
	height, err := p.Chain.Height(ctx)
	if err != nil {
		return refuse("node height: %v", err)
	}
	var lastErr error = refuse("no reverse swap waits for %d sat at that address", r.Amount)
	for _, s := range rows {
		if s.OnchainAmount != r.Amount {
			continue
		}
		l, err := p.checkReverse(ctx, s, want, height)
		if err != nil {
			lastErr = err
			continue
		}
		rate := float64(r.SatPerVbyte) + float64(r.SatPerByte)
		if err := p.chooseCoins(ctx, requestID, r, rate); err != nil {
			return err
		}
		return p.allowSend(ctx, requestID, l, height, rate)
	}
	return lastErr
}

// checkReverse checks a reverse swap row against our key, lnd and the chain.
func (p *Policy) checkReverse(ctx context.Context, s ReverseSwap, want []byte, height int64) (*Lockup, error) {
	if s.Version != 1 {
		return nil, refuse("only taproot swaps")
	}
	if s.LockupAddress != strings.ToLower(s.LockupAddress) {
		return nil, refuse("the swap's lockup address is not canonical (lowercase)")
	}
	hash, err := hex.DecodeString(s.PreimageHash)
	if err != nil || len(hash) != 32 {
		return nil, refuse("the swap has no usable payment hash")
	}
	hh := hex.EncodeToString(hash)
	if s.KeyIndex < 0 || s.KeyIndex >= 1<<31 {
		return nil, refuse("bad key index")
	}
	if s.Timeout-height < p.MinLockBlocks || s.Timeout-height > p.MaxLockBlocks {
		return nil, refuse("timeout %d is %d blocks away, not within %d..%d", s.Timeout, s.Timeout-height, p.MinLockBlocks, p.MaxLockBlocks)
	}
	server, err := p.Keys.Public(uint32(s.KeyIndex))
	if err != nil {
		return nil, refuse("key: %v", err)
	}
	user, err := p.userKey(s.ClaimKey)
	if err != nil {
		return nil, refuse("the swap's claim key: %v", err)
	}
	out, err := swapscript.Build(swapscript.Params{Kind: swapscript.Reverse, Server: server, User: user, PaymentHash: hash, Timeout: uint32(s.Timeout)})
	if err != nil {
		return nil, refuse("swap output: %v", err)
	}
	if !out.Matches(want) {
		return nil, refuse("the lockup address is not the swap output the guard rebuilds")
	}
	p.Ledger.Lock()
	hold, own := p.Ledger.Holds[hh]
	_, locked := p.Ledger.Lockups[hh]
	p.Ledger.Unlock()
	if !own {
		return nil, refuse("the hold invoice was not created through the guard")
	}
	if hold.State != "open" {
		return nil, refuse("the hold invoice is no longer open (%s)", hold.State)
	}
	if locked {
		return nil, refuse("that hold invoice already has a lockup")
	}
	inv, err := p.LN.LookupInvoice(ctx, hash)
	if err != nil {
		return nil, refuse("lnd invoice: %v", err)
	}
	if inv == nil || inv.State != InvoiceAccepted {
		return nil, refuse("the swap's hold invoice is not paid and held")
	}
	if inv.PaidMsat < s.OnchainAmount*1000 {
		return nil, refuse("the hold invoice holds %d msat, less than the %d sat lockup", inv.PaidMsat, s.OnchainAmount)
	}
	if len(inv.HTLCExpiries) == 0 {
		return nil, refuse("the hold invoice holds no HTLC")
	}
	if p.MaxHoldParts > 0 && len(inv.HTLCExpiries) > p.MaxHoldParts {
		return nil, refuse("the hold invoice holds %d HTLCs, at most %d", len(inv.HTLCExpiries), p.MaxHoldParts)
	}
	margin := max(p.SafetyBlocks, p.ReverseMarginBlocks)
	for _, e := range inv.HTLCExpiries {
		if int64(e)-p.HoldExpiryDelta-margin <= s.Timeout {
			return nil, refuse("an HTLC expires at %d, too close to the timeout %d to settle after a claim", e, s.Timeout)
		}
	}
	return &Lockup{
		SwapID: s.ID, Hash: hh, Address: s.LockupAddress, Sat: s.OnchainAmount,
		KeyIndex: uint32(s.KeyIndex), UserKey: strings.ToLower(s.ClaimKey), Timeout: uint32(s.Timeout),
		Vout: -1, State: LockApproved,
	}, nil
}

func (p *Policy) allowSend(ctx context.Context, requestID uint64, l *Lockup, height int64, rate float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	if _, locked := p.Ledger.Lockups[l.Hash]; locked {
		return refuse("that hold invoice already has a lockup")
	}
	if h := p.Ledger.Holds[l.Hash]; h == nil || h.State != "open" {
		return refuse("the hold invoice is no longer open (being cancelled?)")
	}
	unresolved := 0
	for _, x := range p.Ledger.Lockups {
		if x.State == LockApproved {
			unresolved++
		}
	}
	if p.MaxUnresolved > 0 && unresolved >= p.MaxUnresolved {
		return refuse("%d lockups allowed and not seen on chain yet, the limit is %d", unresolved, p.MaxUnresolved)
	}
	if p.MaxLockupsPerBlock > 0 {
		n := 0
		for _, x := range p.Ledger.Lockups {
			if x.SentAt == height {
				n++
			}
		}
		if n >= p.MaxLockupsPerBlock {
			return refuse("%d lockups allowed at height %d already, at most %d a block", n, height, p.MaxLockupsPerBlock)
		}
	}
	if h := p.Ledger.Holds[l.Hash]; h != nil && h.Sat < p.MinReverseSat {
		return refuse("the hold invoice is %d sat, under the %d sat minimum", h.Sat, p.MinReverseSat)
	}
	// and lnd still holds it paid, asked again under the lock
	hb, _ := hex.DecodeString(l.Hash)
	p.Ledger.Unlock()
	inv, err := p.LN.LookupInvoice(ctx, hb)
	p.Ledger.Lock()
	if err != nil || inv == nil || inv.State != InvoiceAccepted || inv.PaidMsat < l.Sat*1000 {
		return refuse("the hold invoice is no longer paid and held")
	}
	// what the payer paid must cover the lockup, its fee and a refund's
	if cover := l.Sat + ReverseCostSat(rate); inv.PaidMsat < cover*1000 {
		return refuse("the hold invoice holds %d msat, less than the lockup with its fee and a refund's (%d sat at %.0f sat/vB)", inv.PaidMsat, cover, rate)
	}
	if h := p.Ledger.Holds[l.Hash]; h == nil || h.State != "open" {
		return refuse("the hold invoice is no longer open (being cancelled?)")
	}
	now := p.now()
	if used := p.Ledger.since(now.Add(-24 * time.Hour)); used+l.Sat > p.DailySat {
		return refuse("%d sat would pass the daily limit (%d of %d used)", l.Sat, used, p.DailySat)
	}
	avail, err := p.available(ctx)
	if err != nil {
		return err
	}
	// other open hold invoices keep what they may still ask for; this
	// one's promise is this lockup
	others := p.promised()
	if h := p.Ledger.Holds[l.Hash]; h != nil {
		others -= h.Sat
	}
	avail -= others
	// the lockup's own fee and a refund at the highest rate come out of the
	// same coins
	need := l.Sat + 2*ClaimVbytes*p.MaxFeeRate
	if need > avail {
		return refuse("%d sat (with the lockup's and a refund's fees) would cut into the LSP reserve (%d available%s)", need, avail, p.balanceNote())
	}
	l.At, l.SentAt = now, height
	p.Ledger.Lockups[l.Hash] = l
	p.Ledger.Entries["send:"+l.Hash] = Entry{Key: "send:" + l.Hash, Sat: l.Sat, At: now, Done: true}
	if err := p.Ledger.save(); err != nil {
		delete(p.Ledger.Lockups, l.Hash)
		delete(p.Ledger.Entries, "send:"+l.Hash)
		return refuse("ledger: %v", err)
	}
	p.track(requestID, pendingReq{method: "/lnrpc.Lightning/SendCoins", hash: l.Hash, sat: l.Sat})
	p.notify("lockup_allowed", map[string]any{"swap": l.SwapID, "sat": l.Sat, "timeout": l.Timeout})
	return nil
}

func (p *Policy) sendPayment(ctx context.Context, requestID uint64, r *lnrpc.SendPaymentRequest) error {
	switch {
	case r.PaymentRequest == "":
		return refuse("only invoices may be paid")
	case len(r.Dest) > 0 || len(r.PaymentHash) > 0 || r.Amt != 0 || r.AmtMsat != 0 || r.FinalCltvDelta != 0:
		return refuse("only an invoice's own amount, hash and destination")
	case r.Amp || r.AllowSelfPayment || len(r.DestCustomRecords) > 0 || len(r.FirstHopCustomRecords) > 0:
		return refuse("AMP, self-payments and custom records are not allowed")
	case len(r.RouteHints) > 0 || len(r.LastHopPubkey) > 0 || len(r.DestFeatures) > 0 || len(r.PaymentAddr) > 0:
		return refuse("the route may not be steered")
	case len(r.OutgoingChanIds) > 0:
		return refuse("outgoing channels may not be chosen")
	case r.MaxParts > 16:
		return refuse("at most 16 parts")
	case r.FeeLimitSat < 0 || r.FeeLimitMsat < 0:
		return refuse("a fee limit is required")
	case r.CltvLimit <= 0:
		return refuse("a cltv_limit is required")
	case r.TimeoutSeconds < 60 || r.TimeoutSeconds > 300:
		return refuse("timeout_seconds must be within 60..300 (the backend asks 300)")
	}
	pr, err := p.LN.DecodePayReq(ctx, r.PaymentRequest)
	if err != nil {
		return refuse("lnd cannot decode the invoice: %v", err)
	}
	if pr.AmountMsat <= 0 {
		return refuse("only invoices with an amount")
	}
	if len(pr.Hash) != 32 {
		return refuse("the invoice has no usable hash")
	}
	sat := (pr.AmountMsat + 999) / 1000
	if sat > p.MaxPerOpSat {
		return refuse("%d sat is over the %d sat limit per operation", sat, p.MaxPerOpSat)
	}
	if sat < p.MinInvoiceSat {
		return refuse("%d sat is under the %d sat minimum", sat, p.MinInvoiceSat)
	}
	hh := hex.EncodeToString(pr.Hash)
	s, err := p.Swaps.SwapByInvoice(ctx, r.PaymentRequest)
	if err != nil {
		return refuse("swap database: %v", err)
	}
	if s == nil {
		return refuse("not the invoice of a Kiloswap swap")
	}
	if s.Version != 1 {
		return refuse("only taproot swaps")
	}
	if !strings.EqualFold(s.PreimageHash, hh) {
		return refuse("the invoice's hash is not the swap's")
	}
	if s.KeyIndex < 0 || s.KeyIndex >= 1<<31 || s.LockupVout < 0 || s.LockupVout > math.MaxUint32 {
		return refuse("bad key index or vout")
	}
	if s.LockupTxID == "" {
		return refuse("the swap has no lockup transaction")
	}
	server, err := p.Keys.Public(uint32(s.KeyIndex))
	if err != nil {
		return refuse("key: %v", err)
	}
	user, err := p.userKey(s.RefundKey)
	if err != nil {
		return refuse("the swap's refund key: %v", err)
	}
	out, err := swapscript.Build(swapscript.Params{Kind: swapscript.Submarine, Server: server, User: user, PaymentHash: pr.Hash, Timeout: uint32(s.Timeout)})
	if err != nil {
		return refuse("swap output: %v", err)
	}
	vout := uint32(s.LockupVout)
	o, err := p.Chain.Output(ctx, s.LockupTxID, vout)
	if err != nil {
		return refuse("node: %v", err)
	}
	if !out.Matches(o.PkScript) {
		return refuse("the lockup output is not the swap output the guard rebuilds")
	}
	if o.Confirmations < p.RequiredConfs {
		return refuse("the lockup has %d confirmations, %d needed", o.Confirmations, p.RequiredConfs)
	}
	maxFeeMsat := int64(math.Ceil(math.Max(float64(pr.AmountMsat)*p.FeeRatio, float64(p.MinFeeSat*1000))))
	// the lockup must cover the invoice and our claim at the node's current
	// fee rate (what the backend charges as the swap's miner fee); routing
	// (at most FeeRatio or MinFeeSat) is the operator's cost by design, and
	// MinInvoiceSat keeps a stream of tiny swaps from adding up
	est, err := p.Chain.FeeRate(ctx)
	if err != nil || est < 1 {
		est = 1
	}
	// the backend priced the swap's miner fee at its estimate when the swap
	// was created; the node's estimate moves, so accept down to 75% of
	// today's claim cost (a few sats at most are the operator's)
	claimCost := max(p.ClaimMarginSat, int64(math.Ceil(float64(ClaimVbytes)*est)))
	cover := sat + max(p.ClaimMarginSat, int64(math.Ceil(float64(ClaimVbytes)*est*0.75)))
	if o.Sat < cover {
		return refuse("the lockup pays %d sat, less than the %d sat invoice plus our claim cost (%d)", o.Sat, sat, cover)
	}
	unspent, err := p.Chain.Unspent(ctx, s.LockupTxID, vout)
	if err != nil {
		return refuse("node: %v", err)
	}
	if !unspent {
		return refuse("the lockup is already spent")
	}
	height, err := p.Chain.Height(ctx)
	if err != nil {
		return refuse("node height: %v", err)
	}
	if s.Timeout-height < p.MinSubTimeoutBlocks {
		return refuse("the swap's timeout is %d blocks away, at least %d needed", s.Timeout-height, p.MinSubTimeoutBlocks)
	}
	margin := max(p.SafetyBlocks, p.ClaimMarginBlocks)
	left := s.Timeout - height - margin
	if left <= 0 {
		return refuse("cltv_limit %d leaves no time to claim before the timeout (%d blocks left, %d kept to claim)", r.CltvLimit, s.Timeout-height, margin)
	}
	feeMsat := r.FeeLimitMsat
	if r.FeeLimitSat != 0 {
		if r.FeeLimitMsat != 0 {
			return refuse("one fee limit only")
		}
		feeMsat = r.FeeLimitSat * 1000
	}
	if p.MaxSubTimeoutBlocks > 0 && s.Timeout-height > p.MaxSubTimeoutBlocks {
		return refuse("the swap's timeout is %d blocks away, at most %d", s.Timeout-height, p.MaxSubTimeoutBlocks)
	}
	// what lnd runs is the judged request with, at most, these lowered: the
	// routing fee limit (option A: paid out of what the lockup brings over
	// the invoice and the claim, never under the floor, never over
	// FeeRatio), the parts (MaxPayParts) and cltv_limit (the blocks left to
	// claim, at most MaxPayCltv). Never raised, never anything else
	wantFee, wantParts, wantCltv := feeMsat, r.MaxParts, r.CltvLimit
	// without RoutingCap (Kilombino, 25 Nov 2026: routing like upstream):
	// any limit over max(MinFeeSat, FeeRatio of the invoice) is lowered to
	// it, never refused
	wantFee = min(feeMsat, maxFeeMsat)
	if p.RoutingCap {
		wantFee = min(feeMsat, min(maxFeeMsat, max((o.Sat-sat-claimCost)*1000, p.RoutingFloorSat*1000)))
	}
	if wantParts == 0 || wantParts > MaxPayParts {
		wantParts = MaxPayParts // lnd's default (16) when 0
	}
	if int64(wantCltv) > min(left, MaxPayCltv) {
		wantCltv = int32(min(left, MaxPayCltv))
	}
	if wantFee != feeMsat || r.FeeLimitSat != 0 || wantParts != r.MaxParts || wantCltv != r.CltvLimit {
		repl := proto.Clone(r).(*lnrpc.SendPaymentRequest)
		repl.FeeLimitSat, repl.FeeLimitMsat = 0, wantFee
		repl.MaxParts, repl.CltvLimit = wantParts, wantCltv
		b, err := proto.Marshal(repl)
		if err != nil {
			return refuse("the request cannot be rewritten safely: %v", err)
		}
		// checked before it is sent: only those three fields changed
		var back lnrpc.SendPaymentRequest
		if proto.Unmarshal(b, &back) != nil || back.FeeLimitMsat != wantFee || back.FeeLimitSat != 0 ||
			back.PaymentRequest != r.PaymentRequest || back.CltvLimit != wantCltv || back.TimeoutSeconds != r.TimeoutSeconds ||
			back.MaxParts != wantParts || back.Amt != 0 || back.AmtMsat != 0 || len(back.OutgoingChanIds) != 0 ||
			len(back.LastHopPubkey) != 0 || len(back.RouteHints) != 0 || len(back.Dest) != 0 || back.Amp ||
			back.AllowSelfPayment || len(back.DestCustomRecords) != 0 || len(back.FirstHopCustomRecords) != 0 ||
			len(back.PaymentAddr) != 0 || len(back.DestFeatures) != 0 || len(back.PaymentHash) != 0 {
			return refuse("the request cannot be rewritten safely")
		}
		p.setReplacement(requestID, b)
	}
	return p.allowPay(ctx, o.Confirmations, &Payment{
		SwapID: s.ID, Hash: hh, Sat: sat, Request: requestID, State: PayInflight,
		KeyIndex: uint32(s.KeyIndex), UserKey: strings.ToLower(s.RefundKey), Timeout: uint32(s.Timeout),
		LockupTx: strings.ToLower(s.LockupTxID), Vout: vout, LockupSat: o.Sat,
	})
}

func (p *Policy) allowPay(ctx context.Context, confs int64, pay *Payment) error {
	requestID := pay.Request
	p.mu.Lock()
	defer p.mu.Unlock()
	// depth by exposure: every payment made against a lockup that is not
	// deep yet counts, so splitting one big lockup into many small outputs
	// (each under a tier) asks the depth of their total
	shallow, inflight := p.shallowExposure(ctx)
	if p.MaxInflightPayments > 0 && inflight >= p.MaxInflightPayments {
		return refuse("%d payments not claimed yet, the limit is %d", inflight, p.MaxInflightPayments)
	}
	if p.MaxInflightSat > 0 {
		// the value out and not claimed back yet, whatever the 24 h window
		// says (a payment lnd has not finished counts until lnd says FAILED)
		p.Ledger.Lock()
		var out int64
		for _, x := range p.Ledger.Payments {
			if x.State == PayInflight || x.State == PayUnknown || x.State == PaySucceeded {
				out += x.Sat
			}
		}
		p.Ledger.Unlock()
		if out+pay.Sat > p.MaxInflightSat {
			return refuse("%d sat more in flight would pass %d sat (%d not claimed back yet)", pay.Sat, p.MaxInflightSat, out)
		}
	}
	if need := p.confsFor(shallow + pay.LockupSat); confs < need {
		return refuse("the lockup has %d confirmations, %d needed for %d sat not deep yet (this one and %d more)", confs, need, shallow+pay.LockupSat, shallow)
	}
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	op := fmt.Sprintf("%s:%d", pay.LockupTx, pay.Vout)
	if h, used := p.Ledger.Outpoints[op]; used {
		return refuse("that lockup already backs a payment (%s)", h)
	}
	if old, ok := p.Ledger.Payments[pay.Hash]; ok && old.State != PayFailed {
		return refuse("that invoice is already %s", old.State)
	}
	now := p.now()
	if used := p.Ledger.since(now.Add(-24 * time.Hour)); used+pay.Sat > p.DailySat {
		return refuse("%d sat would pass the daily limit (%d of %d used)", pay.Sat, used, p.DailySat)
	}
	pay.At = now
	p.Ledger.Payments[pay.Hash] = pay
	p.Ledger.Outpoints[op] = pay.Hash
	p.Ledger.Entries["pay:"+pay.Hash] = Entry{Key: "pay:" + pay.Hash, Sat: pay.Sat, At: now}
	if err := p.Ledger.save(); err != nil {
		delete(p.Ledger.Payments, pay.Hash)
		delete(p.Ledger.Outpoints, op)
		delete(p.Ledger.Entries, "pay:"+pay.Hash)
		return refuse("ledger: %v", err)
	}
	p.track(requestID, pendingReq{method: "/routerrpc.Router/SendPaymentV2", hash: pay.Hash, sat: pay.Sat})
	p.notify("payment_allowed", map[string]any{"swap": pay.SwapID, "sat": pay.Sat})
	return nil
}

func (p *Policy) addHold(ctx context.Context, requestID uint64, r *lnrpc.AddHoldInvoiceRequest) error {
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
	if sat < p.MinReverseSat {
		return refuse("%d sat is under the %d sat minimum of a reverse swap", sat, p.MinReverseSat)
	}
	if p.MaxHoldCltv > 0 && r.CltvExpiry > p.MaxHoldCltv {
		return refuse("cltv_expiry %d over %d", r.CltvExpiry, p.MaxHoldCltv)
	}
	if r.Expiry < 60 || r.Expiry > 3600 {
		// an unpaid hold expires (lnd cancels it) within the hour
		return refuse("expiry %d s not within 60..3600", r.Expiry)
	}
	if inv, err := p.LN.LookupInvoice(ctx, r.Hash); err != nil {
		return refuse("lnd invoice: %v", err)
	} else if inv != nil {
		return refuse("an invoice with that hash already exists")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	hh := hex.EncodeToString(r.Hash)
	if _, ok := p.Ledger.Holds[hh]; ok {
		return refuse("that hash was used before")
	}
	if n := p.openHolds(); n >= p.MaxPendingReverse {
		return refuse("%d reverse swaps pending, the limit is %d", n, p.MaxPendingReverse)
	}
	avail, err := p.available(ctx)
	if err != nil {
		return err
	}
	if promised := p.promised(); promised+sat > avail {
		return refuse("%d sat more would not fit the coins left (%d promised, %d available%s)", sat, promised, avail, p.balanceNote())
	}
	// recorded before lnd is told yes: an answer lost on the way (a stream
	// reset) leaves the hold known, and closeHolds settles it from lnd
	// a nonce of the guard's in the invoice's memo (the request is replaced
	// with it): only an invoice carrying it can ever be taken for this hold
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	memo := "kiloswap " + hex.EncodeToString(nonce)
	repl := proto.Clone(r).(*lnrpc.AddHoldInvoiceRequest)
	repl.Memo = memo
	b, err := proto.Marshal(repl)
	if err != nil {
		return refuse("replacing the memo: %v", err)
	}
	p.setReplacement(requestID, b)
	p.Ledger.Holds[hh] = &Hold{Sat: sat, At: p.now(), State: HoldCreating, Memo: memo}
	if err := p.Ledger.save(); err != nil {
		delete(p.Ledger.Holds, hh)
		return refuse("ledger: %v", err)
	}
	p.track(requestID, pendingReq{method: "/invoicesrpc.Invoices/AddHoldInvoice", hash: hh, sat: sat})
	return nil
}

func (p *Policy) ownLookup(hash []byte) error {
	if len(hash) != 32 {
		return refuse("a 32-byte payment hash is required")
	}
	h := hex.EncodeToString(hash)
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	if x, ok := p.Ledger.Holds[h]; ok {
		if x.State == HoldCreating {
			return refuse("the hold invoice is not confirmed yet")
		}
		return nil
	}
	if _, ok := p.Ledger.Invoices[h]; ok {
		return nil
	}
	return refuse("not an invoice the backend created")
}

func (p *Policy) cancel(requestID uint64, hash []byte) error {
	if len(hash) != 32 {
		return refuse("a 32-byte payment hash is required")
	}
	h := hex.EncodeToString(hash)
	// lnd's invoice must be the guard's own (its memo nonce), asked before
	// taking the locks
	p.Ledger.Lock()
	var memo string
	if x := p.Ledger.Holds[h]; x != nil {
		memo = x.Memo
	}
	p.Ledger.Unlock()
	if err := p.memoMatches(hash, memo); err != nil {
		return err
	}
	// the same lock as allowSend: a cancel and a lockup of one invoice can
	// never both be allowed (lnd would drop the HTLCs and the coins would go)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	hold, ok := p.Ledger.Holds[h]
	if !ok {
		return refuse("not a Kiloswap hold invoice")
	}
	if hold.State == "canceled" {
		return nil // the guard cancelled it already: lnd's answer is a no-op
	}
	if hold.State != "open" {
		// a hold lnd has not confirmed making ('creating') may carry another
		// service's hash: never cancel it
		return refuse("the hold invoice is %s, not open", hold.State)
	}
	if l, ok := p.Ledger.Lockups[h]; ok && l.State != LockRefunded && l.State != LockExpired {
		// (an allowed lockup never seen is not "expired" until lnd itself
		// dropped the HTLCs: findApproved)
		return refuse("its lockup is %s: the invoice stays until the lockup is refunded", l.State)
	}
	if hold.State == "open" {
		hold.State = HoldCanceling // no lockup from now on; back to open if lnd refuses
		if err := p.Ledger.save(); err != nil {
			hold.State = "open"
			return refuse("ledger: %v", err)
		}
	}
	p.track(requestID, pendingReq{method: "/invoicesrpc.Invoices/CancelInvoice", hash: h})
	return nil
}

func (p *Policy) settle(requestID uint64, hash []byte) error {
	h := hex.EncodeToString(hash)
	p.Ledger.Lock()
	hold, ok := p.Ledger.Holds[h]
	var state, memo string
	if ok {
		state, memo = hold.State, hold.Memo
	}
	p.Ledger.Unlock()
	if !ok {
		return refuse("not a Kiloswap hold invoice")
	}
	if state != "open" && state != HoldCanceling {
		return refuse("the hold invoice is %s", state)
	}
	if err := p.memoMatches(hash, memo); err != nil {
		return err
	}
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	if l := p.Ledger.Lockups[h]; l != nil && (l.State == LockRefunding || l.State == LockRefunded || l.State == LockExpired) {
		// the coins are coming (or came) back to the operator: the payer
		// keeps the payment too. A user's claim that wins over the refund
		// is seen on chain and the guard settles then itself.
		return refuse("the lockup is %s: the hold invoice is settled only from a claim on chain", l.State)
	}
	p.track(requestID, pendingReq{method: "/invoicesrpc.Invoices/SettleInvoice", hash: h})
	return nil
}

func (p *Policy) track(requestID uint64, r pendingReq) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.pending == nil {
		p.pending = map[uint64]pendingReq{}
	}
	r.at = p.now()
	r.epoch = p.epoch
	p.pending[requestID] = r
	// streams that never got a final answer are forgotten after a day
	cut := r.at.Add(-24 * time.Hour)
	for id, x := range p.pending {
		if x.at.Before(cut) {
			delete(p.pending, id)
		}
	}
}

func (p *Policy) pendingFor(requestID uint64) (pendingReq, bool) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	r, ok := p.pending[requestID]
	return r, ok
}

func (p *Policy) done(requestID uint64) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	delete(p.pending, requestID)
}

// Payment states of a SendPaymentV2 update.
const (
	paymentInFlight  = 1
	paymentSucceeded = 2
	paymentFailed    = 3
)

// Answered hears lnd's answer to a request. Only lnd's own answers change
// what the guard records: an amount comes back only when lnd refused the
// send or reported the payment FAILED, on the stream that started it.
func (p *Policy) Answered(requestID uint64, m *lnrpc.RPCMessage) {
	r, ok := p.pendingFor(requestID)
	if !ok {
		return
	}
	if u := m.GetMethodFullUri(); u != "" && u != r.method {
		return // another call with a reused id (lnd restarted): not this one
	}
	isErr := m.GetIsError()
	switch r.method {
	case "/lnrpc.Lightning/AddInvoice":
		p.done(requestID)
		var resp lnrpc.AddInvoiceResponse
		if !isErr && proto.Unmarshal(m.GetSerialized(), &resp) == nil && len(resp.RHash) == 32 {
			p.Ledger.Lock()
			p.Ledger.Invoices[hex.EncodeToString(resp.RHash)] = p.now()
			p.Ledger.MarkDirty() // saved with the next round, not one write each
			p.Ledger.Unlock()
		}
	case "/invoicesrpc.Invoices/AddHoldInvoice":
		p.Ledger.Lock()
		if h := p.Ledger.Holds[r.hash]; h != nil && h.State == HoldCreating {
			if isErr {
				delete(p.Ledger.Holds, r.hash) // lnd did not create it
			} else {
				h.State = "open"
			}
			_ = p.Ledger.save()
		}
		p.Ledger.Unlock()
		p.done(requestID)
	case "/invoicesrpc.Invoices/CancelInvoice", "/invoicesrpc.Invoices/SettleInvoice":
		// the hold's state changes before the call stops counting as
		// pending: nothing may see neither
		defer p.done(requestID)
		if isErr {
			if r.method == "/invoicesrpc.Invoices/CancelInvoice" {
				p.mu.Lock()
				p.Ledger.Lock()
				if h := p.Ledger.Holds[r.hash]; h != nil && h.State == HoldCanceling {
					h.State = "open" // lnd did not cancel it
					_ = p.Ledger.save()
				}
				p.Ledger.Unlock()
				p.mu.Unlock()
			}
			return
		}
		p.Ledger.Lock()
		if h := p.Ledger.Holds[r.hash]; h != nil {
			if r.method == "/invoicesrpc.Invoices/CancelInvoice" {
				h.State = "canceled"
			} else {
				h.State = "settled"
				if l := p.Ledger.Lockups[r.hash]; l != nil && l.State != LockRefunded {
					l.State = LockSettled
				}
			}
		}
		_ = p.Ledger.save()
		p.Ledger.Unlock()
	case "/lnrpc.Lightning/SendCoins":
		p.done(requestID)
		var resp lnrpc.SendCoinsResponse
		p.Ledger.Lock()
		l := p.Ledger.Lockups[r.hash]
		if l != nil && l.State == LockApproved {
			if !isErr && proto.Unmarshal(m.GetSerialized(), &resp) == nil && resp.Txid != "" {
				l.State, l.TxID = LockSent, resp.Txid
				p.notify("lockup_sent", map[string]any{"swap": l.SwapID, "sat": l.Sat, "txid": l.TxID})
			} else if isErr {
				// lnd said no; whether anything left the wallet is settled
				// from lnd's own history (findApproved), not from the error
				l.Note = "lnd answered an error"
			}
		}
		_ = p.Ledger.save()
		p.Ledger.Unlock()
	case "/routerrpc.Router/SendPaymentV2":
		p.answeredPayment(requestID, r, m)
	}
}

func (p *Policy) answeredPayment(requestID uint64, r pendingReq, m *lnrpc.RPCMessage) {
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	pay := p.Ledger.Payments[r.hash]
	if pay == nil || pay.Request != requestID {
		p.done(requestID)
		return
	}
	if m.GetIsError() {
		// the stream ended (refused, cancelled, already paid): lnd may still
		// be paying. Nothing is given back until lnd reports the outcome.
		p.done(requestID)
		if pay.State == PayInflight {
			pay.State = PayUnknown
			_ = p.Ledger.save()
		}
		return
	}
	var u lnrpc.Payment
	if proto.Unmarshal(m.GetSerialized(), &u) != nil {
		return
	}
	if !strings.EqualFold(u.PaymentHash, pay.Hash) && u.PaymentHash != "" {
		return
	}
	switch u.Status {
	case paymentSucceeded:
		p.done(requestID)
		p.paid(pay, u.PaymentPreimage, u.FeeMsat, true)
	case paymentFailed:
		p.done(requestID)
		p.failed(pay)
	}
}

// paid records a payment lnd reports as succeeded (caller holds the
// ledger lock).
func (p *Policy) paid(pay *Payment, preimage string, routingMsat int64, known bool) {
	pre, err := hex.DecodeString(preimage)
	h := sha256.Sum256(pre)
	if err != nil || len(pre) != 32 || hex.EncodeToString(h[:]) != pay.Hash {
		pay.State = PayUnknown // reconcile asks lnd again
		_ = p.Ledger.save()
		return
	}
	pay.State, pay.Preimage = PaySucceeded, hex.EncodeToString(pre)
	if known {
		pay.RoutingMsat, pay.RoutingKnown = routingMsat, true
	}
	if e, ok := p.Ledger.Entries["pay:"+pay.Hash]; ok {
		e.Done = true
		p.Ledger.Entries["pay:"+pay.Hash] = e
	}
	_ = p.Ledger.save()
	p.notify("payment_succeeded", map[string]any{"swap": pay.SwapID, "sat": pay.Sat,
		"routing": (pay.RoutingMsat + 999) / 1000, "routingKnown": pay.RoutingKnown})
}

// failed gives back a payment lnd reports as failed (caller holds the lock).
func (p *Policy) failed(pay *Payment) {
	pay.State = PayFailed
	delete(p.Ledger.Entries, "pay:"+pay.Hash)
	delete(p.Ledger.Outpoints, fmt.Sprintf("%s:%d", pay.LockupTx, pay.Vout))
	_ = p.Ledger.save()
	p.notify("payment_failed", map[string]any{"swap": pay.SwapID, "sat": pay.Sat})
}

// ResetPending forgets every call waiting for lnd's answer: after lnd
// restarts its request ids start again, and its old calls ended unanswered
// (lockups among them are found on chain, payments asked about).
func (p *Policy) ResetPending() {
	p.pmu.Lock()
	old := p.pending
	p.pending = map[uint64]pendingReq{}
	p.replace = nil // replacements of requests lnd will never send again
	p.epoch++
	p.pmu.Unlock()
	_ = old // holds, lockups and payments of those calls are on the ledger:
	// closeHolds, watchLockups and reconcilePayments resolve them from lnd
	// and the chain
}

// judgeGetTransactions bounds the backend's wallet listing: from a recent
// height, at most a page at a time (the answers come back through the guard
// and must stay far under its message limit).
func (p *Policy) judgeGetTransactions(ctx context.Context, m *lnrpc.RPCMessage) error {
	var r lnrpc.GetTransactionsRequest
	if err := decode(m, &r); err != nil {
		return err
	}
	tip, err := p.Chain.Height(ctx)
	if err != nil {
		return refuse("node height: %v", err)
	}
	switch {
	case r.Account != "":
		return refuse("the default account only")
	case r.MaxTransactions == 0 || r.MaxTransactions > MaxTxPage:
		// lnd's answer carries each transaction's raw hex (up to ~800 KB
		// each): a page must stay under the guard's 8 MiB message limit
		return refuse("max_transactions must be 1..%d", MaxTxPage)
	case int64(r.StartHeight) < tip-4320:
		return refuse("start_height %d is too old (at most 4320 blocks back)", r.StartHeight)
	case r.EndHeight != -1 && r.EndHeight < r.StartHeight:
		return refuse("bad height range")
	}
	return nil
}

// heavyCalls are calls cheap to ask and dear to answer (the whole graph,
// pathfinding, the wallet's history): at most so many a minute.
var heavyCalls = map[string]int{
	"/lnrpc.Lightning/QueryRoutes":     120,
	"/lnrpc.Lightning/GetTransactions": 60,
	"/lnrpc.Lightning/GetNodeInfo":     240,
}

// rate is a per-call limit over the last minute.
func (p *Policy) rate(key string, perMinute int) bool {
	return p.rateWindow(key, perMinute, time.Minute)
}

// rateWindow is a per-call limit over the last window.
func (p *Policy) rateWindow(key string, perMinute int, window time.Duration) bool {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.calls == nil {
		p.calls = map[string][]time.Time{}
	}
	now := p.now()
	cut := now.Add(-window)
	ts := p.calls[key]
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= perMinute {
		p.calls[key] = ts
		return false
	}
	p.calls[key] = append(ts, now)
	return true
}

// addInvoice judges AddInvoice: the backend receives with it (no money
// leaves), but every one is remembered, so their number, size and life
// are bounded.
func (p *Policy) addInvoice(requestID uint64, m *lnrpc.RPCMessage) error {
	var r lnrpc.AddInvoiceCheck
	if err := proto.Unmarshal(m.GetSerialized(), &r); err != nil {
		return refuse("unreadable invoice: %v", err)
	}
	// only the fields the backend sets (lnrpc.Invoice numbers): no AMP
	// (is_amp, 27), no private channels advertised (private, 15), no
	// blinded paths or anything newer the guard does not read
	if f, err := invoiceFields(m.GetSerialized()); err != nil {
		return refuse("unreadable invoice: %v", err)
	} else if f[27] {
		return refuse("AMP invoices are not allowed")
	} else {
		for n := range f {
			if !addInvoiceFields[n] {
				return refuse("invoice field %d is not allowed", n)
			}
		}
	}
	if r.Private {
		return refuse("private invoices (advertising private channels) are not allowed")
	}
	sat := r.Value
	if r.ValueMsat != 0 {
		sat = (r.ValueMsat + 999) / 1000
	}
	switch {
	case len(r.RPreimage) > 0:
		return refuse("the preimage is lnd's to choose")
	case sat < 0 || sat > p.MaxPerOpSat:
		return refuse("%d sat is not within 0..%d", sat, p.MaxPerOpSat)
	case r.Expiry < 0 || r.Expiry > 86400:
		return refuse("expiry %d s over a day", r.Expiry)
	case len(r.Memo) > 256:
		return refuse("memo too long")
	}
	if !p.rate("AddInvoice", MaxInvoicesPerMinute) {
		return refuse("more than %d invoices a minute", MaxInvoicesPerMinute)
	}
	p.track(requestID, pendingReq{method: "/lnrpc.Lightning/AddInvoice"})
	return nil
}

// addInvoiceFields are the lnrpc.Invoice fields AddInvoice may carry: memo,
// r_preimage (refused when set), value, description_hash, expiry,
// route_hints, private (refused when true), value_msat.
var addInvoiceFields = map[protowire.Number]bool{1: true, 3: true, 5: true, 10: true, 11: true, 14: true, 15: true, 23: true}

// invoiceFields lists the top-level field numbers present in b.
func invoiceFields(b []byte) (map[protowire.Number]bool, error) {
	f := map[protowire.Number]bool{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return nil, protowire.ParseError(m)
		}
		b = b[m:]
		f[num] = true
	}
	return f, nil
}

// MaxTxPage is the most transactions one GetTransactions page may ask.
const MaxTxPage = 7

// MaxInvoicesPerMinute caps AddInvoice.
const MaxInvoicesPerMinute = 30

func (p *Policy) setReplacement(requestID uint64, b []byte) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.replace == nil {
		p.replace = map[uint64][]byte{}
	}
	p.replace[requestID] = b
}

// Replacement is the request the guard sends lnd instead of the one judged
// (nil to keep it), once: AddHoldInvoice gets the guard's memo nonce.
func (p *Policy) Replacement(requestID uint64) []byte {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	b := p.replace[requestID]
	delete(p.replace, requestID)
	return b
}

// memoMatches checks that lnd's invoice for hash carries the guard's own
// memo nonce: the invoice is the one the guard had made, no other service's.
func (p *Policy) memoMatches(hash []byte, memo string) error {
	if memo == "" {
		return nil // a hold from before nonces were kept
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inv, err := p.LN.LookupInvoice(ctx, hash)
	if err != nil {
		return refuse("lnd invoice: %v", err)
	}
	if inv == nil || inv.Memo != memo {
		return refuse("lnd's invoice with that hash is not the guard's (memo)")
	}
	return nil
}

// ReportLimits tells the operator the limits in force (a deployment that
// forgot the launch limits shows here, not after a large swap).
func (p *Policy) ReportLimits() {
	p.notify("guard_limits", map[string]any{"per_op_sat": p.MaxPerOpSat, "daily_sat": p.DailySat,
		"reserve_sat": p.ReserveSat, "pending_reverse": p.MaxPendingReverse, "unresolved": p.MaxUnresolved})
}

// Lifecycle reports the guard starting, registering with lnd or losing its
// stream (the operator's bot alerts on starts and lost streams).
func (p *Policy) Lifecycle(event string) { p.notify(event, map[string]any{}) }

// Stall reports a judgment that took too long or requests that came too
// fast: the backend's database blocked or slowed down, or lnd flooded. The
// request was refused; the operator should know.
func (p *Policy) Stall(why string) {
	p.notify("guard_stall", map[string]any{"reason": why})
}

// Forget undoes what an allowed judgment recorded when lnd was told no
// after all (its judgment came too late): lnd never runs the call, so
// nothing may stay counted or reserved for it.
func (p *Policy) Forget(requestID uint64) { p.ForgetIn(requestID, p.Epoch()) }

// Epoch is the current registration with lnd.
func (p *Policy) Epoch() uint64 {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.epoch
}

// ForgetIn is Forget for a judgment begun in registration epoch: a request
// id reused after lnd restarted belongs to another call, never undone.
func (p *Policy) ForgetIn(requestID, epoch uint64) {
	p.pmu.Lock()
	if p.epoch == epoch {
		delete(p.replace, requestID) // a late judgment's replacement is never sent
	}
	p.pmu.Unlock()
	r, ok := p.pendingFor(requestID)
	if !ok || r.epoch != epoch {
		return
	}
	p.done(requestID)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	switch r.method {
	case "/lnrpc.Lightning/SendCoins":
		if l := p.Ledger.Lockups[r.hash]; l != nil && l.State == LockApproved {
			delete(p.Ledger.Lockups, r.hash)
			delete(p.Ledger.Entries, "send:"+r.hash)
		}
	case "/invoicesrpc.Invoices/CancelInvoice":
		if h := p.Ledger.Holds[r.hash]; h != nil && h.State == HoldCanceling {
			h.State = "open"
		}
	case "/invoicesrpc.Invoices/AddHoldInvoice":
		if h := p.Ledger.Holds[r.hash]; h != nil && h.State == HoldCreating {
			delete(p.Ledger.Holds, r.hash)
		}
	case "/routerrpc.Router/SendPaymentV2":
		if x := p.Ledger.Payments[r.hash]; x != nil && x.Request == requestID && x.State == PayInflight {
			delete(p.Ledger.Payments, r.hash)
			delete(p.Ledger.Outpoints, fmt.Sprintf("%s:%d", x.LockupTx, x.Vout))
			delete(p.Ledger.Entries, "pay:"+r.hash)
		}
	}
	_ = p.Ledger.save()
}
