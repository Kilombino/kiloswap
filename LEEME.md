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
| Por swap | de 10.000 sats a 1 BTC |
| Todos los swaps juntos | 2 BTC cada 24 horas |
| Confirmaciones antes de pagar | 3 |

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

[`guard/`](guard/) es middleware RPC de LND. El macaroon del backend lleva
la condición `kiloswap=v1`, así que LND le pasa al guardián cada llamada
hecha con él antes de ejecutarla. Si el guardián no está, LND las rechaza
todas: falla cerrado.

El guardián deja pasar solo esto:

- **Envíos on-chain**, solo a la dirección de depósito de un swap
  Lightning → on-chain cuya factura retenida LND tiene como pagada
  (`ACCEPTED`, por al menos el importe del depósito). Además:
  - importe exacto y una sola vez;
  - solo monedas confirmadas, nunca `send_all`;
  - comisión de red con tope.
- **Pagos**, solo de la factura de un swap on-chain → Lightning cuyo
  depósito el nodo ve con 3 confirmaciones y por al menos el importe de la
  factura. Además:
  - comisión de ruta como mucho del 0,01 %;
  - sin AMP, keysend ni rutas forzadas.
- **Topes de importe:** como mucho **1 BTC por operación y 2 BTC cada 24
  horas**, contando envíos y pagos juntos. Lo recuerda aunque se reinicie.
- **Reserva para el LSP:** siempre quedan **50.000.000 sats** para el LSP
  del operador, más sus peticiones de canal en curso.
- **Swaps pendientes:** como mucho **5** swaps Lightning → on-chain a la
  vez.
- **Facturas:** cancela o cobra solo sus propias facturas retenidas, nunca
  las de otros servicios.
- **Nada más:** ni canales, ni `SendMany`, ni PSBT, ni macaroons.

La base de datos del backend es la palabra del propio backend. Por eso las
comprobaciones de dinero usan fuentes que el backend no puede escribir: la
factura de LND y la cadena del nodo.

## Límites conocidos

- **Swaps on-chain → Lightning:** se pagan cuando su depósito paga, en la
  cadena, la dirección de depósito del swap. El guardián comprueba la
  transacción y sus confirmaciones, pero no que esa dirección sea de verdad
  el contrato del swap con nuestra clave.
  - Si alguien tomara el control del backend, podría apuntar un swap a una
    dirección suya, depositar ahí sus propias monedas y cobrar su factura:
    recuperaría lo depositado y se llevaría además el pago.
  - Lo acotan el tope diario (2 BTC) y que cada pago llega al bot del
    operador.
  - El siguiente paso es que el guardián reconstruya la dirección del
    contrato a partir del script del swap y de la clave pública del
    servicio.
- **LND y el nodo:** el guardián se fía de ellos. Los dos son del propio
  operador.

## Licencia

AGPL-3.0, como el código que lleva dentro (Boltz y Lightning Fork Swap).
