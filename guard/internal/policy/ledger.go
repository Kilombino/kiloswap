package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one amount the guard let through (the daily limit).
type Entry struct {
	Key  string    `json:"key"`
	Sat  int64     `json:"sat"`
	At   time.Time `json:"at"`
	Done bool      `json:"done"` // moved for good: never released
}

// Hold is a hold invoice the backend created through the guard. It is
// recorded only once lnd has answered that it exists.
type Hold struct {
	Sat   int64     `json:"sat"`
	At    time.Time `json:"at"`
	State string    `json:"state"` // open, settled, canceled
}

// Lockup is an on-chain send the guard allowed for a reverse swap, with the
// output it rebuilt itself: what it will settle (the user's claim reveals
// the preimage) or refund (after the timeout) on its own.
type Lockup struct {
	SwapID   string    `json:"swapId"`
	Hash     string    `json:"hash"`
	Address  string    `json:"address"`
	Sat      int64     `json:"sat"`
	KeyIndex uint32    `json:"keyIndex"`
	UserKey  string    `json:"userKey"`
	Timeout  uint32    `json:"timeout"`
	TxID     string    `json:"txid,omitempty"`
	Vout     int32     `json:"vout"` // -1 until found
	State    string    `json:"state"`
	At       time.Time `json:"at"`
	SentAt   int64     `json:"sentHeight,omitempty"`
	RefundTx string    `json:"refundTx,omitempty"`
	Preimage string    `json:"preimage,omitempty"`
	Note     string    `json:"note,omitempty"`
	Bump     Bump      `json:"refundBump,omitempty"`
	ScanFrom int64     `json:"scanFrom,omitempty"` // where the chain scan for it resumes
	// StuckSince is the height a lockup was first seen stuck (not
	// confirmed, or "not yet" again and again); alerted once
	StuckSince   int64 `json:"stuckSince,omitempty"`
	StuckAlerted bool  `json:"stuckAlerted,omitempty"`
}

// Bump is what the guard needs to replace one of its own spends that did not
// confirm (or fell out of the mempool) with a higher fee: every transaction
// id it broadcast for that spend, the address it pays, the last fee rate and
// the height it was broadcast at.
type Bump struct {
	TxIDs  []string `json:"txids,omitempty"`
	Dest   string   `json:"dest,omitempty"`
	Rate   float64  `json:"rate,omitempty"`
	Height int64    `json:"height,omitempty"`
	Done   bool     `json:"confirmed,omitempty"`
}

// Ours tells whether txid is one of the spends broadcast for it.
func (b *Bump) Ours(txid string) bool {
	for _, t := range b.TxIDs {
		if strings.EqualFold(t, txid) {
			return true
		}
	}
	return false
}

// Lockup states.
const (
	LockApproved  = "approved"  // allowed, lnd has not answered
	LockSent      = "sent"      // on chain
	LockClaimed   = "claimed"   // the user claimed: preimage known, settle pending
	LockSettled   = "settled"   // our hold invoice settled
	LockRefunding = "refunding" // our refund broadcast
	LockRefunded  = "refunded"  // our refund confirmed
	LockExpired   = "expired"   // allowed, never seen on chain: given up
)

// HoldCanceling is a hold invoice whose cancel the guard allowed and lnd has
// not answered: no lockup may be allowed for it.
const HoldCanceling = "canceling"

// HoldCreating is a hold invoice the guard allowed and lnd has not confirmed
// creating: recorded first so a lost answer cannot hide it.
const HoldCreating = "creating"

// Payment is a submarine swap's invoice the guard allowed to be paid, with
// the lockup output it checked: what it claims on its own once paid.
type Payment struct {
	SwapID    string    `json:"swapId"`
	Hash      string    `json:"hash"`
	Sat       int64     `json:"sat"`
	Request   uint64    `json:"request"`
	State     string    `json:"state"`
	Preimage  string    `json:"preimage,omitempty"`
	KeyIndex  uint32    `json:"keyIndex"`
	UserKey   string    `json:"userKey"`
	Timeout   uint32    `json:"timeout"`
	LockupTx  string    `json:"lockupTx"`
	Vout      uint32    `json:"vout"`
	LockupSat int64     `json:"lockupSat"`
	ClaimTx   string    `json:"claimTx,omitempty"`
	ClaimFee  int64     `json:"claimFee,omitempty"`
	Bump      Bump      `json:"claimBump,omitempty"`
	At        time.Time `json:"at"`
}

// Payment states.
const (
	PayInflight  = "inflight"
	PayUnknown   = "unknown" // the stream ended without a final state
	PaySucceeded = "succeeded"
	PayFailed    = "failed"
	PayClaimed   = "claimed"
)

// Ledger is the guard's memory, on disk so a restart forgets nothing. Only
// the guard writes it; nothing in it comes from the backend's word alone.
type Ledger struct {
	mu        sync.Mutex
	path      string
	Entries   map[string]Entry     `json:"entries"`
	Holds     map[string]*Hold     `json:"holds"`
	Lockups   map[string]*Lockup   `json:"lockups"`   // by payment hash
	Payments  map[string]*Payment  `json:"payments"`  // by payment hash
	Outpoints map[string]string    `json:"outpoints"` // txid:vout -> payment hash
	Invoices  map[string]time.Time `json:"invoices"`  // AddInvoice hashes
	dirty     bool                 // invoices changed, saved with the next round
	now       func() time.Time
}

// maxLedgerBytes bounds what the guard reads at start: its records are
// capped, so a ledger this big means something is wrong.
const maxLedgerBytes = 32 << 20

// MaxInvoices caps the AddInvoice hashes remembered (oldest go first), and
// invoiceKeep is how long one is remembered.
const (
	MaxInvoices = 5000
	invoiceKeep = 7 * 24 * time.Hour
)

// keep is how long finished records are remembered.
const keep = 30 * 24 * time.Hour

// OpenLedger reads the ledger at path ("" keeps it in memory, for tests).
func OpenLedger(path string, now func() time.Time) (*Ledger, error) {
	if now == nil {
		now = time.Now
	}
	l := &Ledger{path: path, now: now}
	l.init()
	if path == "" {
		return l, nil
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxLedgerBytes {
		return nil, fmt.Errorf("ledger %s is %d bytes, over %d: something filled it (see its invoices)", path, st.Size(), maxLedgerBytes)
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, l.save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, err
	}
	l.init()
	return l, nil
}

func (l *Ledger) init() {
	if l.Entries == nil {
		l.Entries = map[string]Entry{}
	}
	if l.Holds == nil {
		l.Holds = map[string]*Hold{}
	}
	if l.Lockups == nil {
		l.Lockups = map[string]*Lockup{}
	}
	if l.Payments == nil {
		l.Payments = map[string]*Payment{}
	}
	if l.Outpoints == nil {
		l.Outpoints = map[string]string{}
	}
	if l.Invoices == nil {
		l.Invoices = map[string]time.Time{}
	}
}

// Lock and Unlock guard every read and write of the maps.
func (l *Ledger) Lock()   { l.mu.Lock() }
func (l *Ledger) Unlock() { l.mu.Unlock() }

// Save writes the ledger (caller holds the lock): temporary file synced,
// renamed, directory synced, so a power cut leaves the old or the new.
func (l *Ledger) Save() error { return l.save() }

func (l *Ledger) save() error {
	if l.path == "" {
		return nil
	}
	cut := l.now().Add(-keep)
	for k, e := range l.Entries {
		if e.At.Before(cut) {
			delete(l.Entries, k)
		}
	}
	for h, x := range l.Holds {
		if x.State != "open" && x.At.Before(cut) {
			delete(l.Holds, h)
		}
	}
	for h, x := range l.Lockups {
		if (x.State == LockSettled || x.State == LockRefunded || x.State == LockExpired) && x.At.Before(cut) {
			delete(l.Lockups, h)
		}
	}
	for h, x := range l.Payments {
		claimed := x.State == PayClaimed && (x.Bump.Done || x.Bump.Dest == "")
		if (claimed || x.State == PayFailed) && x.At.Before(cut) {
			delete(l.Payments, h)
		}
	}
	// an outpoint is remembered while its payment is: once that is gone
	// (claimed and deep, or failed long ago) the outpoint is spent or free
	for op, h := range l.Outpoints {
		if _, ok := l.Payments[h]; !ok {
			delete(l.Outpoints, op)
		}
	}
	icut := l.now().Add(-invoiceKeep)
	for h, at := range l.Invoices {
		if at.Before(icut) {
			delete(l.Invoices, h)
		}
	}
	if len(l.Invoices) > MaxInvoices {
		type hv struct {
			h  string
			at time.Time
		}
		all := make([]hv, 0, len(l.Invoices))
		for h, at := range l.Invoices {
			all = append(all, hv{h, at})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		for _, x := range all[:len(all)-MaxInvoices] {
			delete(l.Invoices, x.h)
		}
	}
	l.dirty = false
	b, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(l.path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// since sums the entries after t (caller holds the lock).
func (l *Ledger) since(t time.Time) int64 {
	var s int64
	for _, e := range l.Entries {
		if e.At.After(t) {
			s += e.Sat
		}
	}
	return s
}

// MarkDirty notes a change saved with the next SaveIfDirty (caller holds the
// lock): records whose loss in a crash only costs a refused lookup.
func (l *Ledger) MarkDirty() { l.dirty = true }

// SaveIfDirty writes the ledger if MarkDirty was called since the last save.
func (l *Ledger) SaveIfDirty() error {
	l.Lock()
	defer l.Unlock()
	if !l.dirty {
		return nil
	}
	return l.save()
}
