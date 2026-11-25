#!/usr/bin/env node
// kiloswap-watch: tells the operator on Telegram about every Kiloswap swap,
// one message per swap that is edited as the swap moves on (created → paid →
// claimed / refunded / failed), with the fee earned, and raises alerts: low
// Lightning liquidity, lnd's on-chain wallet near the LSP reserve, the
// pending reverse swaps at their limit, failures, and every refusal of the
// guard (POSTed to it on ALERT_PORT).
//
// Reads, never writes: the backend's Postgres (a read-only user in
// production) and lnd (a read-only macaroon).
//
// Environment:
//   DRY_RUN=1          print the messages to stdout/LOG_FILE instead of Telegram
//   TELEGRAM_TOKEN / TELEGRAM_TOKEN_FILE, TELEGRAM_CHAT   (when not DRY_RUN)
//   STATE_FILE         what it told (default ./kiloswap-watch.state.json)
//   PSQL               command prefix that runs psql -At on the boltz DB
//   LNCLI              command prefix for lncli (read-only macaroon)
//   RESERVE_SAT        the LSP reserve the guard keeps        (default 50000000)
//   LOW_AVAIL_SAT      alert when on-chain above the reserve drops below this (default 10000000)
//   MAX_PENDING_REVERSE  the guard's limit, to warn when it is reached (default 5)
//   ALERT_PORT         listen here (127.0.0.1) for the guard's refusals (default 0 = off)
//   BACKUP_CMD         run (debounced, 2 min) after any swap changes state
//   LOW_LN_OUT_SAT     alert below this Lightning outbound   (default 5000000)
//   LOW_LN_IN_SAT      alert below this Lightning inbound    (default 5000000)
//   INTERVAL_S         seconds between passes (default 20; 0 = one pass)
//   MEMPOOL_URL        explorer for the transaction links (default https://mempool.kilombino.com)
'use strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';

const env = process.env;
const DRY = env.DRY_RUN === '1';
const STATE = env.STATE_FILE || './kiloswap-watch.state.json';
const LOG = env.LOG_FILE || '';
const split = s => s.split(' ').filter(Boolean);
const PSQL = split(env.PSQL || 'docker compose exec -T postgres psql -U boltz -d boltz -At -F |');
const LNCLI = split(env.LNCLI || 'docker compose exec -T lnd-swap lncli --network=regtest');
const RESERVE = Number(env.RESERVE_SAT || 50_000_000);
const LOW_AVAIL = Number(env.LOW_AVAIL_SAT || 10_000_000);
const MAX_PENDING = Number(env.MAX_PENDING_REVERSE || 5);
const ALERT_PORT = Number(env.ALERT_PORT || 0);
const BACKUP = env.BACKUP_CMD ? split(env.BACKUP_CMD) : null;
const LOW_OUT = Number(env.LOW_LN_OUT_SAT || 5_000_000);
const LOW_IN = Number(env.LOW_LN_IN_SAT || 5_000_000);
const INTERVAL = Number(env.INTERVAL_S ?? 20);
const MEMPOOL = (env.MEMPOOL_URL || 'https://mempool.kilombino.com').replace(/\/+$/, '');
const txLink = txid => `${MEMPOOL}/tx/${txid}`;
const readSecret = f => { try { return fs.readFileSync(f, 'utf8').trim(); } catch { return ''; } };
const TOKEN = env.TELEGRAM_TOKEN || (env.TELEGRAM_TOKEN_FILE ? readSecret(env.TELEGRAM_TOKEN_FILE) : '');
const CHAT = env.TELEGRAM_CHAT || '';

const run = (cmd, args = []) => execFileSync(cmd[0], [...cmd.slice(1), ...args], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], maxBuffer: 256 << 20 });
const sql = q => run(PSQL, ['-c', q]).trim().split('\n').filter(Boolean).map(l => l.split('|'));
const fmt = n => Math.round(Number(n)).toLocaleString('es-ES');
const short = id => String(id).slice(0, 8);

let state = { swaps: {}, alerts: {} };
try { state = JSON.parse(fs.readFileSync(STATE, 'utf8')); } catch { /* first run */ }
state.extra ||= {}; // per swap: the guard's claim (txid, fee), our lockup's fee

// What lnd's wallet paid in fees for one of its own transactions (our
// reverse lockups), from its wallet history (read at most once a minute);
// undefined when not found.
let walletTxs = { at: 0, fees: new Map() };
function walletTxFee(txid) {
  if (Date.now() - walletTxs.at > 60_000) {
    try {
      const txs = JSON.parse(run(LNCLI, ['listchaintxns'])).transactions || [];
      walletTxs = { at: Date.now(), fees: new Map(txs.map(t => [t.tx_hash, Number(t.total_fees)])) };
    } catch { return undefined; }
  }
  return walletTxs.fees.get(txid);
}
const save = () => { fs.writeFileSync(STATE + '.tmp', JSON.stringify(state), { mode: 0o600 }); fs.renameSync(STATE + '.tmp', STATE); };

// Telegram, or the dry-run log. send() returns a message id that edit() takes.
let dryId = 0;
async function tg(method, body) {
  if (DRY) {
    const line = `[${new Date().toISOString()}] ${method} ${body.message_id ?? ''}\n${body.text}\n`;
    process.stdout.write(line); if (LOG) fs.appendFileSync(LOG, line);
    return { message_id: body.message_id ?? ++dryId };
  }
  const r = await fetch(`https://api.telegram.org/bot${TOKEN}/${method}`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ chat_id: CHAT, disable_web_page_preview: true, ...body }),
  });
  const j = await r.json();
  if (!j.ok && !/not modified/.test(j.description || '')) throw new Error(`telegram ${method}: ${j.description}`);
  return j.result || {};
}
const send = text => tg('sendMessage', { text });
const edit = (message_id, text) => tg('editMessageText', { message_id, text });

// What each backend status means to the operator.
const SUB = { // on-chain → Lightning (submarine): the user locks on-chain, we pay their invoice
  'swap.created': ['🆕', 'creado, esperando el depósito on-chain'],
  'transaction.mempool': ['⏳', 'depósito visto, esperando confirmaciones'],
  'transaction.confirmed': ['⏳', 'depósito confirmado'],
  'invoice.pending': ['⚡', 'pagando su factura Lightning'],
  'invoice.paid': ['⚡', 'factura pagada'],
  'transaction.claim.pending': ['✅', 'pagado, cobrando el depósito'],
  'transaction.claimed': ['✅', 'COMPLETADO'],
  'invoice.failedToPay': ['❌', 'no se pudo pagar su factura: el usuario recupera su depósito'],
  'transaction.lockupFailed': ['❌', 'depósito incorrecto: el usuario lo recupera'],
  'swap.expired': ['⌛', 'caducado'],
};
const REV = { // Lightning → on-chain (reverse): the user pays us, we lock on-chain
  'swap.created': ['🆕', 'creado, esperando su pago Lightning'],
  'minerfee.paid': ['⏳', 'comisión de red pagada'],
  'transaction.mempool': ['⏳', 'pagado, depósito on-chain enviado'],
  'transaction.confirmed': ['⏳', 'depósito on-chain confirmado'],
  'invoice.settled': ['✅', 'COMPLETADO (el usuario cobró)'],
  'transaction.failed': ['❌', 'no se pudo enviar el depósito: se le devuelve el pago Lightning'],
  'transaction.refunded': ['↩️', 'el usuario no cobró: recuperamos el depósito'],
  'invoice.expired': ['⌛', 'no pagó a tiempo'],
  'swap.expired': ['⌛', 'caducado'],
};
const FINAL = new Set(['transaction.claimed', 'invoice.failedToPay', 'transaction.lockupFailed', 'swap.expired',
  'invoice.settled', 'transaction.failed', 'transaction.refunded', 'invoice.expired']);

// Only these statuses reach Telegram: the lockup seen and confirmed, the
// payment, the end (claimed / settled / refunded / failed / expired). The
// rest (swap.created, invoice.set, transaction.zeroconf.rejected,
// invoice.pending, minerfee.paid…) are intermediate and only noise. An
// expiry or failure of a swap that never got that far (nobody paid or
// locked anything) is not reported either.
const NOTIFY = {
  sub: new Set(['transaction.mempool', 'transaction.confirmed', 'invoice.paid', 'transaction.claimed',
    'invoice.failedToPay', 'transaction.lockupFailed', 'swap.expired']),
  rev: new Set(['transaction.mempool', 'transaction.confirmed', 'invoice.settled', 'transaction.failed',
    'transaction.refunded', 'swap.expired', 'invoice.expired']),
};
const ONLY_IF_SEEN = new Set(['swap.expired', 'invoice.expired', 'invoice.failedToPay', 'transaction.failed']);
// Test swaps (ids) that must never alert, from the environment
const IGNORE = new Set((env.IGNORE_SWAPS || '').split(/[\s,]+/).filter(Boolean));

function text(s) {
  const map = s.kind === 'sub' ? SUB : REV;
  const [icon, what] = map[s.status] || ['•', s.status];
  const dir = s.kind === 'sub' ? 'On-chain → Lightning' : 'Lightning → on-chain';
  const lines = [`${icon} Kiloswap ${short(s.id)} · ${dir}`, what];
  if (s.kind === 'sub' && s.status === 'transaction.lockupFailed') {
    // nothing was paid: say what was expected, what arrived and when the
    // user can take it back by the script path (the server signs nothing)
    lines.push(`On-chain: debía enviar ${fmt(s.onchain)} sats${s.locked ? ` y envió ${fmt(s.locked)}` : ''}`);
    lines.push('Lightning: no se le paga nada');
    if (s.timeout) lines.push(`Lo recupera con su clave de rescate (Rescue/Refund en la web) desde el bloque ${fmt(s.timeout)}`);
  } else if (s.kind === 'sub') {
    lines.push(`On-chain: el usuario envía ${fmt(s.onchain)} sats`);
    lines.push(`Lightning: le pagamos ${fmt(s.ln)} sats`);
  } else {
    lines.push(`Lightning: el usuario nos paga ${fmt(s.ln)} sats`);
    lines.push(`On-chain: le enviamos ${fmt(s.onchain)} sats`);
  }
  const x = state.extra[s.id] || {};
  if (s.status === 'transaction.claimed' || s.status === 'invoice.settled') {
    if (s.kind === 'sub') {
      // charged: the service % and the claim's miner fee (both in s.fee);
      // cost: the route and our claim's actual fee
      const route = (s.routingFeeMsat || 0) / 1000, claim = x.claimFee;
      const cost = route + (claim || 0);
      const earned = (s.fee || 0) - cost;
      lines.push(`Comisión cobrada ${fmt(s.fee || 0)} (servicio + red del cobro) − ruta LN ${fmt(route)} − red del cobro ${claim === undefined ? '¿?' : fmt(claim)} = neto ≈ ${earned >= 0 ? '+' : ''}${fmt(earned)} sats`);
    } else {
      // the user's Lightning payment less what they got on-chain is the
      // service % (s.fee) plus the miner fee charged (lockup + a possible
      // refund); our cost is the lockup's actual fee
      const charged = s.ln - s.onchain, service = s.fee || 0, minerCharged = charged - service;
      if (x.lockupFee === undefined && s.lockupTx) x.lockupFee = walletTxFee(s.lockupTx), state.extra[s.id] = x;
      const lf = x.lockupFee;
      const earned = charged - (lf || 0);
      lines.push(`Cobrado ${fmt(charged)} = servicio ${fmt(service)} + red ${fmt(minerCharged)} (depósito y reserva de reembolso)`);
      lines.push(`Red del depósito ${lf === undefined ? '¿?' : fmt(lf)} → neto ≈ ${earned >= 0 ? '+' : ''}${fmt(earned)} sats`);
    }
  }
  if (s.failureReason) lines.push(`Motivo: ${s.failureReason}`);
  if (s.lockupTx) lines.push(`Depósito: ${txLink(s.lockupTx)}`);
  if (x.claimTx) lines.push(`Cobro: ${txLink(x.claimTx)}`);
  return lines.join('\n');
}

let changed = false, lastBackup = 0;
async function swaps() {
  const subs = sql(`SELECT id, status, COALESCE("expectedAmount",0), COALESCE("invoiceAmount",0), COALESCE(fee,0), COALESCE("routingFee",0), COALESCE("failureReason",''), COALESCE("lockupTransactionId",''), COALESCE("onchainAmount",0), COALESCE("timeoutBlockHeight",0) FROM swaps WHERE "createdAt" > now() - interval '7 days'`)
    .map(([id, status, onchain, ln, fee, rf, fr, tx, locked, timeout]) => ({ kind: 'sub', id, status, onchain: +onchain, ln: +ln, fee: +fee, routingFeeMsat: +rf, failureReason: fr, lockupTx: tx, locked: +locked, timeout: +timeout }));
  const revs = sql(`SELECT id, status, COALESCE("onchainAmount",0), COALESCE("invoiceAmount",0), COALESCE(fee,0), COALESCE("failureReason",''), COALESCE("transactionId",'') FROM "reverseSwaps" WHERE "createdAt" > now() - interval '7 days'`)
    .map(([id, status, onchain, ln, fee, fr, tx]) => ({ kind: 'rev', id, status, onchain: +onchain, ln: +ln, fee: +fee, failureReason: fr, lockupTx: tx }));
  for (const s of [...subs, ...revs]) {
    if (IGNORE.has(s.id)) continue;
    const known = state.swaps[s.id];
    if (known && known.status === s.status) continue;
    // Any status change counts for the backup, reported or not
    changed = true;
    // a failure or expiry of a swap no money ever moved for (no lockup on
    // chain: the user never deposited, or we never locked up) is no news,
    // whatever its status history says
    const failure = ONLY_IF_SEEN.has(s.status) || ['transaction.lockupFailed'].includes(s.status);
    const noFunds = !s.lockupTx;
    const report = NOTIFY[s.kind].has(s.status) && !(ONLY_IF_SEEN.has(s.status) && !known?.msg) && !(failure && noFunds);
    if (!report) {
      state.swaps[s.id] = { ...(known || {}), status: s.status, final: FINAL.has(s.status) };
      save();
      continue;
    }
    const t = text(s);
    state.extra[s.id] = { ...(state.extra[s.id] || {}), last: s };
    if (known?.msg) await edit(known.msg, t);
    else { const m = await send(t); state.swaps[s.id] = { ...(known || {}), msg: m.message_id }; }
    state.swaps[s.id].status = s.status;
    state.swaps[s.id].final = FINAL.has(s.status);
    if (['invoice.failedToPay', 'transaction.lockupFailed', 'transaction.failed'].includes(s.status) && !noFunds)
      await alert(`fail-${s.id}`, `⚠️ Kiloswap: swap ${short(s.id)} ha fallado (${s.status}). Revísalo si se repite.`);
    save();
  }
}

// An alert is sent once and again only after it cleared.
async function alert(key, msg) { if (state.alerts[key]) return; state.alerts[key] = Date.now(); await send(msg); save(); }
function clear(key) { if (state.alerts[key]) { delete state.alerts[key]; save(); } }

async function liquidity() {
  try {
    const b = JSON.parse(run(LNCLI, ['walletbalance']));
    const conf = Number(b.confirmed_balance), pend = Number(b.unconfirmed_balance || 0);
    const avail = conf - Number(b.locked_balance || 0) - Number(b.reserved_balance_anchor_chan || 0) - RESERVE;
    const height = Number(JSON.parse(run(LNCLI, ['getinfo'])).block_height);
    // Short only because our own change is unconfirmed (a lockup spent a
    // big coin): it fixes itself with the next block. Alert only if, a block
    // later, it is still short; a real shortfall (confirmed + pending not
    // enough) at once.
    const split = `confirmado ${fmt(conf)}, pendiente ${fmt(pend)}`;
    const shortBy = (lim) => {
      if (avail >= lim) { delete state.alerts[`short-${lim}`]; return false; }
      if (avail + pend < lim) return true; // even with the pending coins
      const since = state.alerts[`short-${lim}`] ||= height;
      return height > since; // still short after the next block
    };
    if (shortBy(0)) await alert('reserve', `🪫 Kiloswap: el on-chain de lnd está por debajo de la reserva del LSP (${fmt(RESERVE)}): ${split}${avail + pend >= 0 ? ' — debería haberse resuelto con el último bloque, revisa lnd' : ` — faltan ${fmt(-(avail + pend))} sats aun contando lo pendiente`}. Los swaps Lightning → on-chain se rechazan.`);
    else clear('reserve');
    if (avail >= 0 && shortBy(LOW_AVAIL)) await alert('avail', `🪫 Kiloswap: solo quedan ${fmt(avail)} sats on-chain por encima de la reserva del LSP (${split}). Los swaps Lightning → on-chain grandes se rechazarán.`);
    else if (avail >= LOW_AVAIL || avail < 0) clear('avail');
  } catch (e) { await alert('wallet-read', `⚠️ Kiloswap: no puedo leer el on-chain de lnd (${e.message.split('\n')[0]})`); }
  try {
    const [[n]] = sql(`SELECT count(*) FROM "reverseSwaps" WHERE status IN ('swap.created','invoice.set','invoice.pending','minerfee.paid','transaction.mempool','transaction.confirmed')`);
    +n >= MAX_PENDING ? await alert('pending', `⏸️ Kiloswap: ${n} swaps Lightning → on-chain pendientes, el máximo es ${MAX_PENDING}: los nuevos se rechazan hasta que acaben.`) : clear('pending');
  } catch { /* the db alert covers it */ }
  try {
    const ch = JSON.parse(run(LNCLI, ['listchannels', '--active_only'])).channels || [];
    const out = ch.reduce((a, c) => a + Number(c.local_balance), 0);
    const inn = ch.reduce((a, c) => a + Number(c.remote_balance), 0);
    out < LOW_OUT ? await alert('ln-out', `🪫 Kiloswap: poca salida Lightning: ${fmt(out)} sats. Los swaps on-chain → Lightning grandes fallarán.`) : clear('ln-out');
    inn < LOW_IN ? await alert('ln-in', `🪫 Kiloswap: poca entrada Lightning: ${fmt(inn)} sats. Los swaps Lightning → on-chain grandes fallarán.`) : clear('ln-in');
  } catch (e) { await alert('ln-read', `⚠️ Kiloswap: no puedo leer lnd (${e.message.split('\n')[0]})`); }
}

async function pass() {
  try { await swaps(); clear('db'); } catch (e) { await alert('db', `⚠️ Kiloswap: no puedo leer la base de datos del swap (${e.message.split('\n')[0]})`); }
  await liquidity();
  if (changed && BACKUP && Date.now() - lastBackup > 120_000) {
    try { run(BACKUP); lastBackup = Date.now(); changed = false; clear('backup'); }
    catch (e) { await alert('backup', `⚠️ Kiloswap: la copia de seguridad ha fallado (${e.message.split('\n')[0]})`); }
  }
  // forget finished swaps after a week
  const cut = Date.now() - 8 * 864e5;
  for (const [id, s] of Object.entries(state.swaps)) if (s.final && (s.at ||= Date.now()) < cut) { delete state.swaps[id]; delete state.extra[id]; }
  save();
}

if (!DRY && (!TOKEN || !CHAT)) { console.error('TELEGRAM_TOKEN and TELEGRAM_CHAT, or DRY_RUN=1'); process.exit(2); }

// The guard POSTs every refusal here. Each one is news: in normal running the
// backend never asks for what the guard refuses, so a refusal means a bug or
// a backend that is not doing what it should.
if (ALERT_PORT) {
  const http = await import('node:http');
  let last = 0, burst = 0;
  const server = http.createServer((req, res) => {
    let body = '';
    req.on('data', c => { if (body.length < 4096) body += c; });
    req.on('end', async () => {
      res.end('ok');
      let j = {}; try { j = JSON.parse(body); } catch { return; }
      // Movements the guard allowed or made itself: these do not come from
      // the backend's database, so a backend taken over cannot hide them
      const sat = (n) => Number(n || 0).toLocaleString('es-ES');
      const moves = {
        lockup_sent: () => `⛓️ Kiloswap: depósito enviado ${sat(j.sat)} sats (swap ${j.swap})`,
        payment_succeeded: () => `⚡ Kiloswap: pago hecho ${sat(j.sat)} sats, ruta ${j.routingKnown ? sat(j.routing) + ' sats' : '¿?'} (swap ${j.swap})`,
        payment_failed: () => `⚠️ Kiloswap: pago fallido ${sat(j.sat)} sats (swap ${j.swap})`,
        claimed: () => j.invoice === undefined
          ? `✅ Kiloswap: el guardián ha cobrado ${sat(j.sat)} sats (comisión de red ${sat(j.fee)}) — ${String(j.txid).slice(0, 16)}…`
          : `✅ Kiloswap: on-chain → LN cerrado (swap ${j.swap}): cobrado ${sat(j.sat)} − pagado ${sat(j.invoice)} − ruta ${j.routingKnown ? sat(j.routing) : '¿?'} = ${Number(j.net) >= 0 ? '+' : ''}${sat(j.net)} sats netos (red del cobro ${sat(j.fee)})`,
        refunded: () => `↩️ Kiloswap: el guardián ha reembolsado ${sat(j.sat)} sats (swap ${j.swap})`,
        settled: () => `💰 Kiloswap: factura liquidada ${sat(j.sat)} sats (swap ${j.swap})`,
        guard_payment_lost: () => `🚨 Kiloswap: pagado por Lightning pero el usuario recuperó su depósito antes del cobro (swap ${j.swap}, ${sat(j.sat)} sats perdidos) — ${String(j.txid).slice(0, 16)}…`,
        guard_claim_failed: () => `🚨 Kiloswap: el guardián no ha podido cobrar (swap ${j.swap}): ${String(j.error).slice(0, 200)}`,
        guard_lockup_error: () => `🚨 Kiloswap: depósito con problemas (swap ${j.swap}): ${String(j.error).slice(0, 200)}`,
        lockup_allowed: () => `🟢 Kiloswap: depósito permitido ${sat(j.sat)} sats (swap ${j.swap})`,
        payment_allowed: () => `🟢 Kiloswap: pago permitido ${sat(j.sat)} sats (swap ${j.swap})`,
        lockup_dropped: () => `⚠️ Kiloswap: aprobación de depósito retirada (swap ${j.swap})`,
        guard_lockup_stuck: () => j.approved
          ? `🚨 Kiloswap: depósito aprobado hace ${j.blocks} bloques y sin rastro en la cadena (swap ${j.swap}); revisar lnd`
          : `🚨 Kiloswap: depósito atascado ${j.blocks} bloques (swap ${j.swap})`,
        guard_lockup_expired: () => `ℹ️ Kiloswap: depósito nunca enviado, liberado tras comprobarlo en la cadena (swap ${j.swap})`,
        guard_hold_canceled: () => `✂️ Kiloswap: factura retenida cancelada por demasiadas partes (${j.htlcs} HTLC)`,
        guard_ledger_error: () => `🚨 Kiloswap: el guardián no puede escribir su registro: ${String(j.error).slice(0, 200)}`,
        guard_stall: () => `🐢 Kiloswap: el guardián rechazó por lentitud: ${String(j.reason).slice(0, 200)}`,
        guard_registered: () => `✅ Kiloswap: el guardián se ha registrado en lnd`,
        guard_started: () => `🔄 Kiloswap: el guardián se ha (re)iniciado`,
        guard_limits: () => { const btc = (v) => (v / 1e8).toLocaleString('es-ES', { maximumFractionDigits: 8 }); return `📏 Kiloswap: límites en vigor: ${btc(j.per_op_sat)} BTC por swap, ${btc(j.daily_sat)} BTC al día, máximo ${j.pending_reverse} swaps inversos pendientes y máximo ${j.unresolved} depósitos nuestros sin confirmar a la vez`; },
        guard_stream_ended: () => `⚠️ Kiloswap: el guardián perdió su conexión con lnd (se vuelve a registrar solo)`,
        claim_bumped: () => `⏫ Kiloswap: cobro sin confirmar, reenviado con más comisión (${j.rate} sat/vB, swap ${j.swap})`,
        refund_bumped: () => `⏫ Kiloswap: reembolso sin confirmar, reenviado con más comisión (${j.rate} sat/vB, swap ${j.swap})`,
        claim_confirmed: () => `✔️ Kiloswap: cobro confirmado (swap ${j.swap})`,
        refund_confirmed: () => `✔️ Kiloswap: reembolso confirmado (swap ${j.swap})`,
      };
      if (j.event === 'guard_alive' || j.event === 'guard_registered') { lastAlive = Date.now(); if (j.event === 'guard_alive') return; }
      if (j.event === 'guard_stall' || j.event === 'lockup_allowed' || j.event === 'payment_allowed') {
        // frequent: at most a few a minute, like refusals
        const now = Date.now(); burst = now - last < 60_000 ? burst + 1 : 0; last = now;
        if (burst > 5) return;
      }
      if ((j.event === 'claimed' || j.event === 'settled') && j.swap) {
        const x = state.extra[j.swap] ||= {};
        if (j.event === 'claimed') { x.claimTx = j.txid; x.claimFee = Number(j.fee); }
        else if (j.claim) x.claimTx = j.claim;
        save();
        const k = state.swaps[j.swap];
        if (k?.msg && x.last) { try { await edit(k.msg, text(x.last)); } catch { /* next one */ } }
      }
      if (moves[j.event]) {
        let m = moves[j.event]();
        const tx = j.event === 'claimed' ? j.txid : j.event === 'settled' ? j.claim : j.event === 'lockup_sent' ? j.txid : j.event === 'refunded' ? j.txid : undefined;
        if (tx && /^[0-9a-f]{64}$/.test(tx)) m += `\n${txLink(tx)}`;
        try { await send(m); } catch { /* next one */ } return;
      }
      if (j.event !== 'guard_refusal') return;
      const now = Date.now(); burst = now - last < 60_000 ? burst + 1 : 0; last = now;
      if (burst > 5) return; // at most a few a minute; the guard's log has them all
      try { await send(`🛡️ Kiloswap: el guardián ha bloqueado ${String(j.method).split('/').pop()}\n${String(j.reason).slice(0, 300)}`); } catch { /* next one */ }
    });
  });
  // The host address on the stack's network exists only while the stack
  // is up: keep trying until it does
  const listen = () => server.listen(ALERT_PORT, env.ALERT_HOST || '127.0.0.1');
  server.on('error', () => setTimeout(listen, 30_000));
  listen();
}
await pass();
if (INTERVAL > 0) setInterval(pass, INTERVAL * 1000);

// The guard says it is alive every 5 minutes: silence for 15 is an alert
// (it may be stopped, crashing in a loop, or not registered with lnd).
let lastAlive = Date.now();
let aliveAlerted = false;
if (ALERT_PORT) setInterval(async () => {
  if (Date.now() - lastAlive > 15 * 60_000) {
    if (!aliveAlerted) { aliveAlerted = true; try { await send('🚨 Kiloswap: el guardián lleva 15 minutos sin dar señales (¿parado o reiniciándose?)'); } catch { /* next time */ } }
  } else aliveAlerted = false;
}, 60_000);
