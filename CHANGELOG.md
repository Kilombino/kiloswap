# Changelog

Kiloswap's guard, shim and deployment, by review round (local history before
the first deployment).

## Review v9 (25 Nov 2026)

- Shim: `getrawmempool` only on its own, never inside a batch.
- Guard: at most `MAX_INFLIGHT_SAT` (twice one operation's limit by default)
  paid and not claimed back, whatever the 24 h window; the payment request is
  rewritten to at most 4 parts and a `cltv_limit` of at most the blocks left
  to claim and 288 (lowered, never raised).
- Guard: AddInvoice refuses AMP (`is_amp`), private invoices and any field
  the backend does not set.
- RUNBOOK: the node's ZMQ topics with `hwm=20` are a precondition of the
  maintenance window.

## Option A (25 Nov 2026, decided by Kilombino)

- Guard: the routing fee limit of every submarine payment is rewritten to
  `min(max(FEE_RATIO × invoice, 10 sat), max(lockup surplus − claim, 10 sat))`
  (`ROUTING_CAP=1` by default). A larger limit is lowered, never refused.
- Backend: routing fee floor 10 sat; three "no route" failures give the swap
  up as `invoice.failedToPay` and the user refunds after the timeout.

  Correction: commit a50261a says "routing cap off". That was true only
  until option A (3ee746f); the cap is on by default.

## Review v8

- Reverse swaps from 10,000 sat; the hold invoice must pay the lockup, its
  fee and a refund's at the lockup's rate; 2 lockups a block;
  `LockupFeeFactor` 1.25; GetTransactions pages of 7.
- Shim: 10 raw items per batch (4 blocks), answers up to 32 MB, 256 MB of
  memory, stalled ZMQ subscribers dropped.
- Lost payments remembered; replacements freed; no backend settle while the
  guard refunds.

## Review v7

- Shim: strict per-method parameters, canonical requests, answer cap.
- Guard: canonical SendCoins, unresolved-lockup cap, open holds with the
  memo nonce only, approval search incremental and after the real work.
- Deploy: launch limits (0.1 / 0.25 BTC) by default everywhere.
