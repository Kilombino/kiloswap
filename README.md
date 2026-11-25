# Kiloswap

Non-custodial swaps between on-chain BTC on the Bitcoin BLAKE2b chain and its
Lightning network, run by Kilombino at <https://swap.kilombino.com> (also over
Tor). The swap engine is [Boltz](https://github.com/BoltzExchange) as adapted
to Bitcoin BLAKE2b by paulscode in
[Lightning Fork Swap](https://github.com/paulscode/lightning-fork-swap)
(lightningfork.com). Kiloswap adds two things:

- the swap's on-chain funds live in the operator's main lnd wallet, not in a
  separate hot wallet;
- because of that, a **guard** sits between the swap backend and lnd and
  bounds what the backend can make lnd do, even if the backend were taken
  over.

[Leer en español](LEEME.md)

## Fees and limits

| | |
|---|---|
| On-chain → Lightning (submarine) | 0.01 % + the on-chain fee |
| Lightning → on-chain (reverse) | 0.15 % + the on-chain fee |
| Per swap | 10,000 sat – 1 BTC |
| All swaps together | 2 BTC per 24 hours |
| Confirmations before paying out | 1 up to 0.01 BTC, 2 up to 0.1 BTC, 3 above (of total exposure) |

## Trustless

- Keys are made in your browser. You keep a rescue key (file); the service
  never holds your funds.
- On-chain funds go to a hash time-locked contract: either the swap
  completes for both sides, or after the timeout you take your coins back
  with your rescue key alone, even if the service is gone.
- Lightning → on-chain: you claim on-chain with a secret only you know.
- Tested on regtest with the service stopped: refund by the timeout path and
  claim by the script path, broadcast by a node, with nothing but the user's
  keys.
- The service signs nothing cooperatively. A submarine swap that fails is
  refunded with your rescue key **after its timeout** (about 3 days), not
  right away; a Lightning → on-chain swap is claimed by the script path.

What the service does need: if the service is gone **and** your browser was
wiped, finding your swaps again from the rescue key asks the service. That is
why its database is backed up, encrypted, off the host.

## The guard

[`guard/`](guard/) is lnd RPC middleware and the only holder of the swaps'
server key. The backend's macaroon carries the custom caveat `kiloswap=v1`;
lnd hands every call made with it to the guard before running it, and
refuses all of them while the guard is not registered (fail closed).

The threat it is built for is the backend and its database taken over.

- **The key.** The swaps' server key (the former `seed.dat`) is the guard's
  alone. The backend has the extended public key: it puts our public keys in
  new swaps and finds their outputs, but cannot sign a claim, a refund or a
  key-path spend. Cooperative signing is off. The guard makes every
  server-side spend itself, by the script path, to an address of lnd's
  wallet: it claims submarine lockups once their payment succeeded, refunds
  reverse lockups after their timeout, and settles a hold invoice as soon as
  the user's claim shows the preimage on chain.
- **Rebuilt outputs.** Before any money moves, the guard rebuilds the swap's
  taproot output (MuSig2 of our key and the user's, claim and refund
  leaves, the payment hash, the timeout) from our key and data it checks,
  and compares it with the address or the on-chain output.
- **On-chain sends** (`SendCoins`) only to such a reverse-swap output, whose
  hold invoice the backend created through the guard and lnd holds as paid
  with every HTLC expiring well after the timeout; once per invoice, exact
  amount, confirmed coins, an explicit fee rate under the cap, no
  `send_all`; timeout between 12 and 4,320 blocks away.
- **Payments** (`SendPaymentV2`) only of an invoice lnd decodes (amount and
  hash from the invoice, never from the database), for a submarine swap
  whose rebuilt output the node shows unspent and deep enough (`CONF_TIERS`), paying
  at least the invoice plus a claim margin; each lockup backs one payment;
  `cltv_limit` leaves time to claim before the timeout (lowered to it, and
  to 288 blocks at most); at most 4 parts; the routing fee limit rewritten
  to what the lockup pays over the invoice and the claim (10 sat floor,
  0.01 % at most); no AMP, keysend, custom records, route hints or chosen
  channels; at most twice one operation's limit in flight at once.
- At launch at most **0.1 BTC per operation and 0.25 BTC per 24 hours**
  (later 1 and 2 BTC, set explicitly), sends and payments
  together, remembered across restarts. An amount comes back only when lnd
  itself reports the send refused or the payment FAILED (asked again if a
  stream ends early); a cancelled stream or "already paid" gives nothing
  back.
- On-chain sends and new hold invoices must leave **50,000,000 sat** for the
  operator's LSP, the LSP's open requests and every send not answered yet.
- At most **5 open** Lightning → on-chain hold invoices, by the guard's own
  count.
- Lookups, cancels and settles only of invoices the backend created through
  the guard (recorded when lnd answers, never from the database); a hold
  invoice whose lockup is still out cannot be cancelled.
- Nothing else: no channels, no `SendMany`, no PSBTs, no macaroons, no
  mission control reset.
- Every movement it allows or makes, and every refusal, is posted to the
  operator's bot by the guard itself.

Deployment: the backend sits on an internal Docker network (postgres, the
node through the shim, the guard's API, and lnd's gRPC port through a proxy);
it has no route to `lnd-net`, the host's LAN or the internet. The guard has
its own node RPC user (reads and the broadcast of its own spends).

Tests: `go test ./...` in `guard/` (every rule, the claim and refund
transactions run through the script engine, the rebuilt outputs checked
against swaps the real backend made), and
[`guard/test/lab.sh`](guard/test/lab.sh) on the regtest lab: the backend on
the extended public key with an unrelated `seed.dat`, the e2e suite through
the guard (both directions, ~1 BTC swaps, user refunds and claims with the
server down, the guard's own timeout refund), then a backend taken over
calling lnd directly (the independent review's scenarios among them) and the
guard stopped.

## Known limits

- The guard trusts lnd and the node it asks. Both are the operator's own.
- **Middleware impersonation while the guard is down.** lnd refuses the
  backend's macaroon while no middleware is registered for the `kiloswap`
  caveat, but anything holding a macaroon allowed to register middleware
  (an admin macaroon) could register under that caveat itself and allow
  the backend's calls. lnd offers no way to list registered middleware, so
  the guard cannot check it; keep admin macaroons off this stack and watch
  the guard's `registered` / `stream ended` log lines.
- Privacy, not funds: the backend's macaroon still reads the node's
  channels, peers and on-chain transactions (`ListChannels`, `ListPeers`,
  `GetTransactions`, which the backend uses), and its node RPC user can read
  any loaded wallet of the node (`listunspent`, `gettransaction`): the
  node's RPC whitelist is per method, not per wallet.
- No cooperative refunds: a failed submarine swap waits for its timeout
  (the web app says so, in English and Spanish).
- `HOLD_EXPIRY_DELTA` must equal lnd's `invoices.holdexpirydelta` (18): lnd
  does not report it, so the guard cannot check it. Change both together.
- Confirmations scale with the amount (`CONF_TIERS`, default 1 up to 1M
  sat, 2 up to 10M, 3 above: the operator's choice, 25 Nov 2026; about half
  of the chain's hash rate sits in one pool, so a reorg of a few blocks is a
  known risk bounded by the daily cap). The backend's own `requiredConfirmations` is lower, so
  a big lockup's payment is refused by the guard until it is deep enough
  and the backend tries again.

- **One unhardened key family.** The backend holds the extended public key
  at `m/0/0` and the guard derives each swap's key from it unhardened. Under
  BIP32, one leaked child private key plus that public key gives the parent
  and so every swap key. Child keys never leave the guard, so a leak needs
  the guard itself; a later step could have the guard hand per-swap public
  keys from a hardened path through its API instead of an xpub.
- **The node's RPC under load.** The backend's calls go to the node through
  the shim; heavy use could fill the node's `rpcworkqueue`, which DATUM
  shares. Keep `rpcworkqueue` at its raised value on the node and watch for
  "work queue depth exceeded" in its log.

### Launch mode

The guard starts in launch mode (`LAUNCH_MODE`, default on): 0.1 BTC per
operation and 0.25 BTC a day, with the backend's `maxSwapAmount` at 0.1 BTC.
`LAUNCH_MODE=0` (and `maxSwapAmount = 100000000`) gives the later 1 BTC per
operation and 2 BTC a day.

### What the guard does about slow or hostile backends

- Each lnd interception is judged in its own goroutine with a 1.5 s
  deadline; a late judgment is a refusal and is undone. The guard's
  database connection has `statement_timeout` and `lock_timeout` and is
  read-only, under its own non-superuser role.
- Lockups lnd sent without telling the guard are found on chain; payments
  are always asked about after two minutes.
- Submarine: invoices of 10,000 sat or more, a lockup that covers the
  invoice plus our claim at the node's fee rate (routing comes out of what
  it pays over that, at least 10 sat), a swap timeout at
  least 144 blocks ahead, payments final 60 blocks before it, and the
  guard's claim replaced with higher fees (past `MAX_FEE_RATE`, up to 10 %
  of the lockup, in the last 24 blocks).
- Reverse: lockup fee rate at most 2x the node's estimate, timeouts within
  72..288 blocks, hold invoices with at most 16 HTLCs, a cltv_expiry of at
  most 432 and every HTLC expiring 18 + 36 blocks after the timeout.
- The guard's own spends are done at 3 confirmations (reorgs) and are
  replaced (RBF) when stuck or dropped; a refund cancels its hold invoice
  only as deep as the amount's tier, and pays what it takes near the HTLCs'
  deadline.
- A cancel and a lockup of one hold invoice are judged under one lock (the
  invoice is "canceling" until lnd answers).
- Depth is asked of the total not yet deep: all payments whose lockups are
  under the last tier's depth plus the new one (splitting a lockup does not help).
- Lockups lnd sent without telling the guard are looked for only among
  lnd's own wallet transactions; one never seen expires after 72 blocks.
- AddInvoice is judged (amount, a day's expiry, 30 a minute) and at most
  5000 of its hashes are kept a week; DescribeGraph, QueryRoutes,
  GetTransactions and GetNodeInfo are limited per minute; Postgres caps the
  backend's connections at 25 so the guard always finds one.

## Layout

| | |
|---|---|
| [`guard/`](guard/) | the guard (Go) |
| [`deploy/`](deploy/) | compose, backend config template, nginx, setup, macaroons, backups, [RUNBOOK](deploy/RUNBOOK.md) |
| [`watch/`](watch/) | Telegram messages per swap (fee earned), liquidity alerts, the guard's refusals |
| [Kilombino/kiloswap-backend](https://github.com/Kilombino/kiloswap-backend) (branch `kiloswap`) | the backend: Lightning Fork Swap's backend + lnd 0.21.8 + lnd's wallet as the swap wallet |
| [Kilombino/kiloswap-webapp](https://github.com/Kilombino/kiloswap-webapp) (branch `kiloswap`) | the web app: Lightning Fork Swap's web app with Kiloswap's name and icons, no donations |

## License

AGPL-3.0, as the code it carries (Boltz, Lightning Fork Swap). The guard's
middleware loop is adapted from Lightning Fork Swap's donations guard.
