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
| Confirmations before paying out | 3 |

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
  whose rebuilt output the node shows unspent with 3 confirmations, paying
  at least the invoice plus a claim margin; each lockup backs one payment;
  `cltv_limit` leaves time to claim before the timeout; routing fee at most
  0.01 % (121 sat floor); no AMP, keysend, custom records, route hints or
  chosen channels.
- At most **1 BTC per operation and 2 BTC per 24 hours**, sends and payments
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
- Privacy, not funds: the backend's macaroon still reads the node's
  channels, peers and on-chain transactions (`ListChannels`, `ListPeers`,
  `GetTransactions`, which the backend uses), and its node RPC user can read
  any loaded wallet of the node (`listunspent`, `gettransaction`): the
  node's RPC whitelist is per method, not per wallet.
- No cooperative refunds: a failed submarine swap waits for its timeout.
- The guard's refunds are not fee-bumped; it pays the node's estimate (the
  chain's fees are low). A stuck refund would need a manual replacement.
- The web app is set to `cooperativeDisabled` (branch `kiloswap`); that
  build was not run through the UI tests yet.

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
