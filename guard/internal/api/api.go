// Package api is where the swap backend asks the guard to claim a
// submarine swap's lockup (once its payment succeeded) or refund a reverse
// swap's (once its timeout passed). The guard does both on its own anyway;
// asking only makes it immediate and hands the backend the transaction for
// its records. Nothing the backend sends is trusted but the swap's id,
// used to find the guard's own record.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/policy"
)

type request struct {
	ID string `json:"id"`
}

type answer struct {
	TransactionID string `json:"transactionId,omitempty"`
	Fee           int64  `json:"fee,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Serve answers on addr until ctx ends.
func Serve(ctx context.Context, addr, token string, p *policy.Policy, logger *log.Logger) {
	mux := http.NewServeMux()
	handle := func(do func(context.Context, string) (string, int64, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, []byte("Bearer "+token)) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var req request
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.ID == "" || len(req.ID) > 64 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(answer{Error: "a swap id is required"})
				return
			}
			c, cancel := context.WithTimeout(r.Context(), time.Minute)
			defer cancel()
			txid, fee, err := do(c, req.ID)
			if err != nil {
				code := http.StatusInternalServerError
				if errors.Is(err, policy.ErrNotYet) {
					code = http.StatusConflict
				}
				logger.Printf("api %s %s: %v", r.URL.Path, req.ID, err)
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(answer{Error: err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(answer{TransactionID: txid, Fee: fee})
		}
	}
	mux.HandleFunc("/v1/claim", handle(p.Claim))
	mux.HandleFunc("/v1/refund", handle(p.Refund))
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("api: %v", err)
	}
}
