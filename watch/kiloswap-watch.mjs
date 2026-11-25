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
const readSecret = f => { try { return fs.readFileSync(f, 'utf8').trim(); } catch { return ''; } };
const TOKEN = env.TELEGRAM_TOKEN || (env.TELEGRAM_TOKEN_FILE ? readSecret(env.TELEGRAM_TOKEN_FILE) : '');
const CHAT = env.TELEGRAM_CHAT || '';

const run = (cmd, args = []) => execFileSync(cmd[0], [...cmd.slice(1), ...args], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
const sql = q => run(PSQL, ['-c', q]).trim().split('\n').filter(Boolean).map(l => l.split('|'));
const fmt = n => Math.round(Number(n)).toLocaleString('es-ES');
const short = id => String(id).slice(0, 8);

let state = { swaps: {}, alerts: {} };
try { state = JSON.parse(fs.readFileSync(STATE, 'utf8')); } catch { /* first run */ }
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

function text(s) {
  const map = s.kind === 'sub' ? SUB : REV;
  const [icon, what] = map[s.status] || ['•', s.status];
  const dir = s.kind === 'sub' ? 'On-chain → Lightning' : 'Lightning → on-chain';
  const lines = [`${icon} Kiloswap ${short(s.id)} · ${dir}`, `${fmt(s.amount)} sats · ${what}`];
  if (s.status === 'transaction.claimed' || s.status === 'invoice.settled') {
    // Earned = the fee charged (service % + miner fee the user paid) minus what it cost us.
    const cost = s.kind === 'sub' ? (s.routingFeeMsat || 0) / 1000 : (s.ourMinerFee || 0);
    const earned = (s.fee || 0) - cost;
    lines.push(`Comisión cobrada ${fmt(s.fee || 0)} − ${s.kind === 'sub' ? 'ruta LN' : 'red on-chain'} ${fmt(cost)} = ganado ≈ ${fmt(earned)} sats`);
  }
  if (s.failureReason) lines.push(`Motivo: ${s.failureReason}`);
  return lines.join('\n');
}

let changed = false, lastBackup = 0;
async function swaps() {
  const subs = sql(`SELECT id, status, COALESCE("invoiceAmount", "expectedAmount", 0), COALESCE(fee,0), COALESCE("routingFee",0), COALESCE("failureReason",'') FROM swaps WHERE "createdAt" > now() - interval '7 days'`)
    .map(([id, status, amount, fee, rf, fr]) => ({ kind: 'sub', id, status, amount: +amount, fee: +fee, routingFeeMsat: +rf, failureReason: fr }));
  const revs = sql(`SELECT id, status, "invoiceAmount", COALESCE(fee,0), COALESCE("minerFeeOnchainAmount",0), COALESCE("failureReason",'') FROM "reverseSwaps" WHERE "createdAt" > now() - interval '7 days'`)
    .map(([id, status, amount, fee, mf, fr]) => ({ kind: 'rev', id, status, amount: +amount, fee: +fee, ourMinerFee: +mf, failureReason: fr }));
  for (const s of [...subs, ...revs]) {
    const known = state.swaps[s.id];
    if (known && known.status === s.status) continue;
    const t = text(s);
    if (known?.msg) await edit(known.msg, t);
    else { const m = await send(t); state.swaps[s.id] = { msg: m.message_id }; }
    state.swaps[s.id].status = s.status;
    changed = true;
    state.swaps[s.id].final = FINAL.has(s.status);
    if (['invoice.failedToPay', 'transaction.lockupFailed', 'transaction.failed'].includes(s.status))
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
    const avail = Number(b.confirmed_balance) - Number(b.locked_balance || 0) - Number(b.reserved_balance_anchor_chan || 0) - RESERVE;
    avail < 0 ? await alert('reserve', `🪫 Kiloswap: el on-chain de lnd (${fmt(b.confirmed_balance)} sats) está por debajo de la reserva del LSP (${fmt(RESERVE)}). Los swaps Lightning → on-chain se rechazan.`) : clear('reserve');
    avail >= 0 && avail < LOW_AVAIL ? await alert('avail', `🪫 Kiloswap: solo quedan ${fmt(avail)} sats on-chain por encima de la reserva del LSP. Los swaps Lightning → on-chain grandes se rechazarán.`) : clear('avail');
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
  for (const [id, s] of Object.entries(state.swaps)) if (s.final && (s.at ||= Date.now()) < cut) delete state.swaps[id];
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
        payment_succeeded: () => `⚡ Kiloswap: pago hecho ${sat(j.sat)} sats (swap ${j.swap})`,
        payment_failed: () => `⚠️ Kiloswap: pago fallido ${sat(j.sat)} sats (swap ${j.swap})`,
        claimed: () => `✅ Kiloswap: el guardián ha cobrado ${sat(j.sat)} sats (comisión de red ${sat(j.fee)}) — ${String(j.txid).slice(0, 16)}…`,
        refunded: () => `↩️ Kiloswap: el guardián ha reembolsado ${sat(j.sat)} sats (swap ${j.swap})`,
        settled: () => `💰 Kiloswap: factura liquidada ${sat(j.sat)} sats (swap ${j.swap})`,
        guard_claim_failed: () => `🚨 Kiloswap: el guardián no ha podido cobrar (swap ${j.swap}): ${String(j.error).slice(0, 200)}`,
        guard_lockup_error: () => `🚨 Kiloswap: depósito con problemas (swap ${j.swap}): ${String(j.error).slice(0, 200)}`,
        guard_started: () => `🔄 Kiloswap: el guardián se ha (re)iniciado`,
        guard_stream_ended: () => `⚠️ Kiloswap: el guardián perdió su conexión con lnd (se vuelve a registrar solo)`,
        claim_bumped: () => `⏫ Kiloswap: cobro sin confirmar, reenviado con más comisión (${j.rate} sat/vB, swap ${j.swap})`,
        refund_bumped: () => `⏫ Kiloswap: reembolso sin confirmar, reenviado con más comisión (${j.rate} sat/vB, swap ${j.swap})`,
        claim_confirmed: () => `✔️ Kiloswap: cobro confirmado (swap ${j.swap})`,
        refund_confirmed: () => `✔️ Kiloswap: reembolso confirmado (swap ${j.swap})`,
      };
      if (j.event === 'guard_alive' || j.event === 'guard_registered') { lastAlive = Date.now(); if (j.event === 'guard_alive') return; }
      if (moves[j.event]) { try { await send(moves[j.event]()); } catch { /* next one */ } return; }
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
