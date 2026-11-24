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

What the service does need: if the service is gone **and** your browser was
wiped, finding your swaps again from the rescue key asks the service. That is
why its database is backed up, encrypted, off the host.

## The guard

[`guard/`](guard/) is lnd RPC middleware. The backend's macaroon carries the
custom caveat `kiloswap=v1`; lnd hands every call made with it to the guard
before running it, and refuses all of them while the guard is not registered
(fail closed). The guard allows:

- **on-chain sends** (`SendCoins`) only to the lockup address of a reverse
  swap whose hold invoice lnd itself holds as paid (`ACCEPTED`, at least the
  lockup's amount), exact amount, once, confirmed coins only, no `send_all`,
  fee rate capped;
- **payments** (`SendPaymentV2`) only of the invoice of a submarine swap whose
  lockup the node shows confirmed (3 confirmations) paying at least the
  invoice's amount; routing fee at most 0.01 % (121 sat floor); no AMP, keysend,
  custom records or route steering;
- at most **1 BTC per operation and 2 BTC per 24 hours**, sends and payments
  together, remembered across restarts;
- on-chain sends and new hold invoices must leave **50,000,000 sat** for the
  operator's LSP plus the LSP's open channel requests;
- at most **5 pending** Lightning → on-chain swaps;
- cancel/settle only its own hold invoices (never those of the operator's
  other services);
- nothing else: no channels, no `SendMany`, no PSBTs, no macaroons.

The backend's database is the backend's own word, so the money checks use
sources the backend cannot write: lnd's invoice and the node's chain.

Tests: `go test ./...` in `guard/` (every rule), and
[`guard/test/lab.sh`](guard/test/lab.sh) on the regtest lab: the whole e2e
suite (both directions, ~1 BTC swaps, refunds, rescues with the server down)
through the guard, then a backend taken over calling lnd directly (sends to
random addresses, sweeps, random invoices, other services' invoices, forged
database rows, channels, too-wide macaroons) and the guard stopped. All
refused.

## Known limits

- A submarine swap is paid once its lockup pays the swap's lockup address on
  chain. The guard checks the transaction and its depth, not that the
  address is really the swap's contract with our claim key. A backend taken
  over could write a swap pointing at an address of the attacker's, then
  lock coins there itself and have its own invoice paid: it would get back
  what it locked plus the payment. The daily limit (2 BTC) bounds it, every
  payment reaches the operator's bot, and the next step is for the guard to
  rebuild the contract's address from the swap's script and the service's
  public key.
- The guard trusts lnd and the node it asks. Both are the operator's own.

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
