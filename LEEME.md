# Kiloswap

Swaps sin custodia entre BTC on-chain de la cadena Bitcoin BLAKE2b y su red
Lightning, en <https://swap.kilombino.com> (también por Tor).

El motor de los swaps es [Boltz](https://github.com/BoltzExchange), adaptado
a Bitcoin BLAKE2b por paulscode en
[Lightning Fork Swap](https://github.com/paulscode/lightning-fork-swap)
(lightningfork.com). Kiloswap le añade dos cosas:

- **Una sola cartera:** el dinero on-chain del swap está en la cartera del
  LND principal del operador, no en una cartera caliente aparte.
- **Un guardián** entre el backend del swap y LND, que limita lo que el
  backend puede hacerle hacer a LND aunque alguien tomara el control del
  backend.

[Read in English](README.md)

## Comisiones y límites

| | |
|---|---|
| On-chain → Lightning | 0,01 % + la comisión de red |
| Lightning → on-chain | 0,15 % + la comisión de red |
| Por swap | de 10.000 sats a 0,1 BTC (al principio; luego 1 BTC) |
| Todos los swaps juntos | 0,25 BTC cada 24 horas (al principio; luego 2 BTC) |
| Comisión de enrutado (on-chain → Lightning) | la paga lo que el depósito trae de más, al menos 10 sats |
| Confirmaciones antes de pagar | 1 hasta 0,01 BTC, 2 hasta 0,1 BTC, 3 por encima (del total expuesto) |

## Sin confiar en nadie

- **Tus claves:** se crean en tu navegador y guardas una clave de rescate
  (un archivo). El servicio nunca custodia tu dinero.
- **On-chain:** el dinero va a un contrato con plazo. O el swap se completa
  para los dos, o al vencer el plazo recuperas tus monedas solo con tu clave
  de rescate, aunque el servicio ya no exista.
- **Lightning → on-chain:** cobras on-chain con un secreto que solo tienes
  tú.
- **Probado en regtest con el servidor apagado:** reembolso por plazo y
  cobro, emitidos desde un nodo, con nada más que las claves del usuario.

**Lo que sí depende del servicio:** si el servicio desaparece **y** además
borraste tu navegador, para volver a encontrar tus swaps a partir de la
clave de rescate hay que preguntarle al servicio. Por eso su base de datos
se copia, cifrada, fuera de la máquina.

## El guardián

[`guard/`](guard/) es un middleware RPC de LND y el único que tiene la clave
de servidor de los swaps. El macaroon del backend lleva la condición
`kiloswap=v1`: LND le pasa cada llamada al guardián antes de ejecutarla, y
las rechaza todas mientras el guardián no esté registrado.

Está pensado para el caso de que alguien tome el backend y su base de datos:

- **La clave.** La clave de servidor de los swaps (el antiguo `seed.dat`) es
  solo del guardián. El backend tiene la clave pública extendida: pone
  nuestras claves públicas en los swaps nuevos y encuentra sus salidas, pero
  no puede firmar cobros, reembolsos ni gastos por la ruta de clave. No hay
  firma cooperativa. El guardián hace él mismo todos los gastos del lado del
  servidor, por la ruta de script y a direcciones de LND: cobra los depósitos
  submarinos cuando su pago sale bien, reembolsa los inversos tras el plazo
  y liquida la factura retenida en cuanto el cobro del usuario muestra el
  preimage en la cadena.
- **Salidas reconstruidas.** Antes de mover dinero, el guardián reconstruye
  la salida taproot del swap (MuSig2 de nuestra clave y la del usuario,
  hojas de cobro y de reembolso, hash, plazo) con nuestra clave y datos que
  comprueba, y la compara con la dirección o la salida en la cadena.
- **Envíos on-chain** solo a esa salida de un swap inverso cuya factura
  retenida creó el backend a través del guardián y LND tiene pagada, con
  todos los HTLC venciendo bastante después del plazo; una vez por factura,
  importe exacto, monedas confirmadas y comisión explícita bajo el tope.
- **Pagos** solo de facturas que LND descodifica (importe y hash de la
  factura, nunca de la base de datos), de un swap submarino cuya salida
  reconstruida el nodo ve sin gastar y con las confirmaciones que pide `CONF_TIERS`, por al menos el
  importe más un margen de cobro; cada depósito respalda un solo pago; el
  `cltv_limit` deja tiempo para cobrar antes del plazo; comisión de ruta como
  mucho 0,01 %; sin elegir canales ni rutas.
- **1 BTC por operación y 2 BTC cada 24 h**, entre envíos y pagos. Un importe
  solo se devuelve si LND dice que rechazó el envío o que el pago FALLÓ.
- **Reserva de 50.000.000 sats** para el LSP, más sus peticiones abiertas y
  los envíos aún sin respuesta.
- **Como mucho 5** facturas retenidas abiertas, según la cuenta del propio
  guardián.
- Consultas, cancelaciones y liquidaciones solo de facturas que el backend
  creó a través del guardián; una factura con su depósito fuera no se puede
  cancelar.
- Cada movimiento y cada rechazo los avisa el propio guardián al bot.

El backend está en una red interna de Docker sin salida: no llega a
`lnd-net`, a la red local ni a internet.

## Límites conocidos

- El guardián confía en el LND y el nodo a los que pregunta (los del
  operador).
- Privacidad, no fondos: el macaroon del backend aún lee canales, pares y
  transacciones on-chain del nodo, y su usuario RPC puede leer cualquier
  cartera cargada (la lista blanca es por método, no por cartera).
- Sin reembolsos cooperativos: un swap submarino fallido espera al plazo.
- Los reembolsos del guardián no suben de comisión solos.
- La web lleva `cooperativeDisabled`; esa compilación aún no ha pasado las
  pruebas de interfaz.

## Licencia

AGPL-3.0, como el código que lleva dentro (Boltz y Lightning Fork Swap).
