// Package sources are what the guard reads to judge: the swap backend's
// database (read-only role), lnd's wallet balance and the LSP's requests.
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/policy"
)

// DB reads the backend's Postgres.
type DB struct{ Pool *pgxpool.Pool }

func OpenDB(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) ReverseByLockup(ctx context.Context, address string) ([]policy.ReverseSwap, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT "lockupAddress", "onchainAmount", "preimageHash", status
		   FROM "reverseSwaps" WHERE "lockupAddress" = $1`, address)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []policy.ReverseSwap
	for rows.Next() {
		var r policy.ReverseSwap
		if err := rows.Scan(&r.LockupAddress, &r.OnchainAmount, &r.PreimageHash, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) ReverseByHash(ctx context.Context, hash string) (bool, error) {
	var n int
	err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM "reverseSwaps" WHERE "preimageHash" = $1`, strings.ToLower(hash)).Scan(&n)
	return n > 0, err
}

// Statuses in which a reverse swap may still lock coins, and those in which
// it has not locked them yet.
const pendingStatuses = `('swap.created','invoice.set','invoice.pending','minerfee.paid','transaction.mempool','transaction.confirmed')`
const unlockedStatuses = `('swap.created','invoice.set','invoice.pending','minerfee.paid')`

func (d *DB) PendingReverse(ctx context.Context) (int, int64, error) {
	var n int
	var sat int64
	err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM "reverseSwaps" WHERE status IN `+pendingStatuses).Scan(&n)
	if err != nil {
		return 0, 0, err
	}
	err = d.Pool.QueryRow(ctx,
		`SELECT coalesce(sum("onchainAmount"),0) FROM "reverseSwaps" WHERE status IN `+unlockedStatuses).Scan(&sat)
	return n, sat, err
}

func (d *DB) SwapByInvoice(ctx context.Context, invoice string) (*policy.Swap, error) {
	var s policy.Swap
	var amt *int64
	var txid *string
	err := d.Pool.QueryRow(ctx,
		`SELECT invoice, "invoiceAmount", status, "lockupAddress", "lockupTransactionId"
		   FROM swaps WHERE lower(invoice) = lower($1)`, invoice).
		Scan(&s.Invoice, &amt, &s.Status, &s.LockupAddress, &txid)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	if amt != nil {
		s.InvoiceAmount = *amt
	}
	if txid != nil {
		s.LockupTxID = *txid
	}
	return &s, nil
}

// Wallet asks lnd for its on-chain balance with the guard's macaroon.
type Wallet struct{ Conn grpc.ClientConnInterface }

func (w Wallet) Balance(ctx context.Context) (policy.Balance, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var r lnrpc.WalletBalanceResponse
	if err := w.Conn.Invoke(ctx, "/lnrpc.Lightning/WalletBalance", &lnrpc.WalletBalanceRequest{}, &r); err != nil {
		return policy.Balance{}, err
	}
	return policy.Balance{Confirmed: r.ConfirmedBalance, Locked: r.LockedBalance, AnchorReserve: r.ReservedBalanceAnchorChan}, nil
}

// Invoices asks lnd for its own record of an invoice.
type Invoices struct{ Conn grpc.ClientConnInterface }

func (i Invoices) Invoice(ctx context.Context, hash []byte) (int32, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var r lnrpc.Invoice
	if err := i.Conn.Invoke(ctx, "/lnrpc.Lightning/LookupInvoice", &lnrpc.PaymentHash{RHash: hash}, &r); err != nil {
		return 0, 0, err
	}
	return r.State, r.AmtPaidMsat, nil
}

// Chain asks the node (Knots, JSON-RPC with a whitelisted user) what a
// transaction pays to an address and how deep it is.
type Chain struct {
	URL, User, Password string
	Client              *http.Client
}

func (c Chain) PaidTo(ctx context.Context, txid, address string) (int64, int64, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "guard", "method": "getrawtransaction", "params": []any{txid, true}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Content-Type", "application/json")
	cl := c.Client
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, 0, err
	}
	var r struct {
		Result *struct {
			Confirmations int64 `json:"confirmations"`
			Vout          []struct {
				Value        float64 `json:"value"`
				ScriptPubKey struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, 0, fmt.Errorf("node answer (%d): %w", resp.StatusCode, err)
	}
	if r.Error != nil {
		return 0, 0, fmt.Errorf("node: %s", r.Error.Message)
	}
	if r.Result == nil {
		return 0, 0, fmt.Errorf("node: no transaction %s", txid)
	}
	var sat int64
	for _, o := range r.Result.Vout {
		if o.ScriptPubKey.Address == address {
			sat += int64(math.Round(o.Value * 1e8))
		}
	}
	return sat, r.Result.Confirmations, nil
}

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
			Sats   int64  `json:"sats"`
			Status string `json:"status"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return 0, fmt.Errorf("LSP state: %w", err)
	}
	var s int64
	for _, o := range st.Orders {
		if o.Status == "waiting_payment" || o.Status == "paid" {
			s += o.Sats
		}
	}
	return s, nil
}
