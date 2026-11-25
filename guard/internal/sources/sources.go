// Package sources are what the guard reads to judge: the swap backend's
// database (read-only role), lnd's wallet balance and the LSP's requests.
package sources

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/policy"
)

// DB reads the backend's Postgres.
type DB struct{ Pool *pgxpool.Pool }

// OpenDB connects with the guard's read-only role. Every statement has a
// deadline of its own on the server (and the judgment's context on the
// client): a backend that locks the tables or plants huge rows makes the
// guard refuse, never wait.
func OpenDB(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["statement_timeout"] = "1000"
	rp["lock_timeout"] = "500"
	rp["idle_in_transaction_session_timeout"] = "5000"
	rp["default_transaction_read_only"] = "on"
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) ReverseByLockup(ctx context.Context, address string) ([]policy.ReverseSwap, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT id, "lockupAddress", "onchainAmount", "preimageHash", coalesce("claimPublicKey",''),
		        coalesce("keyIndex",-1), "timeoutBlockHeight", version
		   FROM "reverseSwaps" WHERE "lockupAddress" = $1 LIMIT 4`, address)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []policy.ReverseSwap
	for rows.Next() {
		var r policy.ReverseSwap
		if err := rows.Scan(&r.ID, &r.LockupAddress, &r.OnchainAmount, &r.PreimageHash, &r.ClaimKey,
			&r.KeyIndex, &r.Timeout, &r.Version); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SwapByInvoice(ctx context.Context, invoice string) (*policy.Swap, error) {
	var s policy.Swap
	err := d.Pool.QueryRow(ctx,
		`SELECT id, "preimageHash", coalesce("refundPublicKey",''), coalesce("keyIndex",-1),
		        "timeoutBlockHeight", coalesce("lockupTransactionId",''), coalesce("lockupTransactionVout",-1), version
		   FROM swaps WHERE lower(invoice) = lower($1) LIMIT 1`, invoice).
		Scan(&s.ID, &s.PreimageHash, &s.RefundKey, &s.KeyIndex, &s.Timeout, &s.LockupTxID, &s.LockupVout, &s.Version)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// LN is lnd through the guard's own macaroon.
type LN struct{ Conn grpc.ClientConnInterface }

func call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 10*time.Second)
}

func (l LN) Balance(ctx context.Context) (policy.Balance, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	var r lnrpc.WalletBalanceResponse
	if err := l.Conn.Invoke(ctx, "/lnrpc.Lightning/WalletBalance", &lnrpc.WalletBalanceRequest{}, &r); err != nil {
		return policy.Balance{}, err
	}
	return policy.Balance{Confirmed: r.ConfirmedBalance, Locked: r.LockedBalance, AnchorReserve: r.ReservedBalanceAnchorChan}, nil
}

// FindWalletSend finds a transaction of lnd's own wallet (from startHeight,
// unconfirmed ones too) paying sat to address: a lockup lnd sent. Only the
// wallet's own transactions count: anyone can pay the same address.
func (l LN) FindWalletSend(ctx context.Context, address string, sat int64, startHeight int64) (string, uint32, int64, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	var r lnrpc.TransactionDetails
	if err := l.Conn.Invoke(ctx, "/lnrpc.Lightning/GetTransactions",
		&lnrpc.GetTransactionsRequest{StartHeight: int32(max(startHeight, 0)), EndHeight: -1, MaxTransactions: 2000}, &r); err != nil {
		return "", 0, 0, err
	}
	for _, t := range r.Transactions {
		if t.Amount >= 0 { // not a send of ours
			continue
		}
		for _, o := range t.OutputDetails {
			if !o.IsOurAddress && o.Address == address && o.Amount == sat {
				return t.TxHash, uint32(o.OutputIndex), int64(t.NumConfirmations), nil
			}
		}
	}
	return "", 0, 0, nil
}

func (l LN) LookupInvoice(ctx context.Context, hash []byte) (*policy.InvoiceInfo, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	var r lnrpc.Invoice
	if err := l.Conn.Invoke(ctx, "/lnrpc.Lightning/LookupInvoice", &lnrpc.PaymentHash{RHash: hash}, &r); err != nil {
		if status.Code(err) == codes.NotFound || strings.Contains(err.Error(), "unable to locate invoice") {
			return nil, nil
		}
		return nil, err
	}
	info := &policy.InvoiceInfo{State: r.State, PaidMsat: r.AmtPaidMsat}
	for _, h := range r.Htlcs {
		if h.State == 0 || h.State == 1 { // accepted or settled
			info.HTLCExpiries = append(info.HTLCExpiries, h.ExpiryHeight)
		}
	}
	return info, nil
}

func (l LN) DecodePayReq(ctx context.Context, payReq string) (*policy.PayReq, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	var r lnrpc.PayReq
	if err := l.Conn.Invoke(ctx, "/lnrpc.Lightning/DecodePayReq", &lnrpc.PayReqString{PayReq: payReq}, &r); err != nil {
		return nil, err
	}
	h, err := hex.DecodeString(r.PaymentHash)
	if err != nil {
		return nil, err
	}
	return &policy.PayReq{Hash: h, AmountMsat: r.NumMsat}, nil
}

func (l LN) SettleInvoice(ctx context.Context, preimage []byte) error {
	ctx, cancel := call(ctx)
	defer cancel()
	var r emptyMsg
	return l.Conn.Invoke(ctx, "/invoicesrpc.Invoices/SettleInvoice", &lnrpc.SettleInvoiceMsg{Preimage: preimage}, &r)
}

func (l LN) CancelInvoice(ctx context.Context, hash []byte) error {
	ctx, cancel := call(ctx)
	defer cancel()
	var r emptyMsg
	return l.Conn.Invoke(ctx, "/invoicesrpc.Invoices/CancelInvoice", &lnrpc.CancelInvoiceMsg{PaymentHash: hash}, &r)
}

func (l LN) NewAddress(ctx context.Context) (string, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	var r lnrpc.NewAddressResponse
	if err := l.Conn.Invoke(ctx, "/lnrpc.Lightning/NewAddress", &lnrpc.NewAddressRequest{Type: 4}, &r); err != nil {
		return "", err
	}
	return r.Address, nil
}

var trackDesc = &grpc.StreamDesc{StreamName: "TrackPaymentV2", ServerStreams: true}

// TrackPayment reads lnd's first answer on the payment (its current state).
func (l LN) TrackPayment(ctx context.Context, hash []byte) (int32, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st, err := l.Conn.NewStream(ctx, trackDesc, "/routerrpc.Router/TrackPaymentV2")
	if err != nil {
		return 0, "", err
	}
	if err := st.SendMsg(&lnrpc.TrackPaymentRequest{PaymentHash: hash, NoInflightUpdates: true}); err != nil {
		return 0, "", err
	}
	if err := st.CloseSend(); err != nil {
		return 0, "", err
	}
	var p lnrpc.Payment
	if err := st.RecvMsg(&p); err != nil {
		if status.Code(err) == codes.NotFound || strings.Contains(err.Error(), "not found") {
			return 0, "", nil
		}
		return 0, "", err
	}
	return p.Status, p.PaymentPreimage, nil
}

// emptyMsg reads an answer the guard does not need.
type emptyMsg = lnrpc.WalletBalanceRequest

// Chain asks the node (Knots, JSON-RPC with the guard's own whitelisted
// user) about transactions, and broadcasts the guard's own spends.
type Chain struct {
	URL, User, Password string
	Client              *http.Client
}

func (c Chain) rpc(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "guard", "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Content-Type", "application/json")
	cl := c.Client
	if cl == nil {
		cl = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("node answer to %s (%d): %w", method, resp.StatusCode, err)
	}
	if r.Error != nil {
		return fmt.Errorf("node %s: %s", method, r.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

type rawTx struct {
	Txid          string `json:"txid"`
	Confirmations int64  `json:"confirmations"`
	Vin           []struct {
		Txid        string   `json:"txid"`
		Vout        uint32   `json:"vout"`
		Txinwitness []string `json:"txinwitness"`
	} `json:"vin"`
	Vout []struct {
		Value        json.Number `json:"value"`
		N            uint32      `json:"n"`
		ScriptPubKey struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

func sats(v json.Number) (int64, error) {
	// exact: "1.23456789" BTC to sat without floating point
	s := v.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 8 {
		return 0, fmt.Errorf("value %s has more than 8 decimals", v)
	}
	frac += strings.Repeat("0", 8-len(frac))
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if neg {
		n = -n
	}
	return n, err
}

func (c Chain) Outputs(ctx context.Context, txid string) ([]policy.Output, error) {
	var t rawTx
	if err := c.rpc(ctx, "getrawtransaction", []any{txid, 1}, &t); err != nil {
		return nil, err
	}
	out := make([]policy.Output, len(t.Vout))
	for i, o := range t.Vout {
		pk, err := hex.DecodeString(o.ScriptPubKey.Hex)
		if err != nil {
			return nil, err
		}
		s, err := sats(o.Value)
		if err != nil {
			return nil, err
		}
		out[i] = policy.Output{PkScript: pk, Sat: s, Confirmations: t.Confirmations}
	}
	return out, nil
}

func (c Chain) Output(ctx context.Context, txid string, vout uint32) (*policy.Output, error) {
	outs, err := c.Outputs(ctx, txid)
	if err != nil {
		return nil, err
	}
	if int(vout) >= len(outs) {
		return nil, fmt.Errorf("transaction %s has no output %d", txid, vout)
	}
	return &outs[vout], nil
}

func (c Chain) Unspent(ctx context.Context, txid string, vout uint32) (bool, error) {
	var r json.RawMessage
	if err := c.rpc(ctx, "gettxout", []any{txid, vout, true}, &r); err != nil {
		return false, err
	}
	return len(r) > 0 && string(r) != "null", nil
}

func (c Chain) Height(ctx context.Context) (int64, error) {
	var h int64
	err := c.rpc(ctx, "getblockcount", []any{}, &h)
	return h, err
}

func (c Chain) Confirmations(ctx context.Context, txid string) (int64, error) {
	var t rawTx
	if err := c.rpc(ctx, "getrawtransaction", []any{txid, 1}, &t); err != nil {
		return 0, err
	}
	return t.Confirmations, nil
}

func (c Chain) Broadcast(ctx context.Context, rawHex string) (string, error) {
	var txid string
	err := c.rpc(ctx, "sendrawtransaction", []any{rawHex}, &txid)
	return txid, err
}

func (c Chain) FeeRate(ctx context.Context) (float64, error) {
	var r struct {
		Feerate float64 `json:"feerate"` // BTC/kvB
	}
	if err := c.rpc(ctx, "estimatesmartfee", []any{2}, &r); err != nil {
		return 0, err
	}
	return r.Feerate * 1e8 / 1000, nil
}

func witness(hexes []string) ([][]byte, error) {
	w := make([][]byte, len(hexes))
	for i, h := range hexes {
		b, err := hex.DecodeString(h)
		if err != nil {
			return nil, err
		}
		w[i] = b
	}
	return w, nil
}

// Spender looks in the mempool (gettxspendingprevout), then in the blocks
// from fromHeight to the tip.
func (c Chain) Spender(ctx context.Context, txid string, vout uint32, fromHeight int64) (*policy.Spend, error) {
	find := func(t rawTx, confs int64) (*policy.Spend, error) {
		for _, in := range t.Vin {
			if strings.EqualFold(in.Txid, txid) && in.Vout == vout {
				w, err := witness(in.Txinwitness)
				if err != nil {
					return nil, err
				}
				return &policy.Spend{TxID: t.Txid, Witness: w, Confirmations: confs}, nil
			}
		}
		return nil, nil
	}
	var mp []struct {
		Spendingtxid string `json:"spendingtxid"`
	}
	if err := c.rpc(ctx, "gettxspendingprevout", []any{[]map[string]any{{"txid": txid, "vout": vout}}}, &mp); err == nil {
		for _, m := range mp {
			if m.Spendingtxid == "" {
				continue
			}
			var t rawTx
			if err := c.rpc(ctx, "getrawtransaction", []any{m.Spendingtxid, 1}, &t); err != nil {
				return nil, err
			}
			if s, err := find(t, 0); s != nil || err != nil {
				return s, err
			}
		}
	}
	tip, err := c.Height(ctx)
	if err != nil {
		return nil, err
	}
	if fromHeight < 1 || tip-fromHeight > maxScan {
		fromHeight = tip - maxScan
	}
	for h := tip; h >= fromHeight; h-- {
		var hash string
		if err := c.rpc(ctx, "getblockhash", []any{h}, &hash); err != nil {
			return nil, err
		}
		var b struct {
			Tx []rawTx `json:"tx"`
		}
		if err := c.rpc(ctx, "getblock", []any{hash, 2}, &b); err != nil {
			return nil, err
		}
		for _, t := range b.Tx {
			if s, err := find(t, tip-h+1); s != nil || err != nil {
				return s, err
			}
		}
	}
	return nil, nil
}

// maxScan bounds how many blocks one search reads.
const maxScan = 1500

// LSP reads the LSP's state file. Requests waiting for payment or paid
// hold their amount until the channel's funding transaction is out (then
// the coins are gone from the confirmed balance anyway).
type LSP struct{ Path string }

func (l LSP) Reserved() (int64, error) {
	b, err := os.ReadFile(l.Path)
	if err != nil {
		return 0, err
	}
	var st struct {
		Orders map[string]struct {
			Sats     int64  `json:"sats"`
			Status   string `json:"status"`
			Expires  int64  `json:"expires"` // unix seconds
			Unsigned bool   `json:"unsigned"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return 0, fmt.Errorf("LSP state: %w", err)
	}
	now := time.Now().Unix()
	var s, anon int64
	for _, o := range st.Orders {
		switch {
		case o.Status == "paid":
			s += o.Sats
		case o.Status == "waiting_payment" && (o.Expires == 0 || now <= o.Expires):
			// anyone can ask the public LSP for a quote: those not signed by
			// a node count up to a cap, so quotes alone cannot stop swaps
			if o.Unsigned {
				anon += o.Sats
			} else {
				s += o.Sats
			}
		}
	}
	return s + min(anon, MaxAnonymousReserve), nil
}

// MaxAnonymousReserve caps what unsigned LSP quotes waiting for payment
// hold back.
const MaxAnonymousReserve = 20_000_000
