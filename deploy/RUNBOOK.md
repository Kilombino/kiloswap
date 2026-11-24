# Kiloswap: puesta en marcha en el host (una sola ventana)

Este es el guion para montar Kiloswap junto al nodo BLAKE2b y el LND del
operador. Todo lo que no corta nada se hace antes. Lo que corta va junto en
**una sola ventana**:

- **Nodo (Knots):** ZMQ, usuario RPC y subred del stack, todo de una vez.
  Reiniciarlo deja a DATUM sin plantilla unos segundos.
- **LND:** `rpcmiddleware.enable`. Reiniciarlo deja los canales fuera un
  minuto y el bridge parado ese rato. No afecta a la minería.

Rutas de ejemplo (las de `.env.example`): repositorio en `/opt/kiloswap`,
datos en `${KILOSWAP_ROOT}`.

## Antes (sin cortes)

1. **Imágenes**
   - Backend: el fork `Kilombino/kiloswap-backend`, rama `kiloswap`
     (VersionCheck 0.21.8 y la cartera de LND).
   - Shim: `paulscode/lightning-fork-swap`, `shim/`.
   - Guardián: `guard/`.
   - Web: `Kilombino/kiloswap-webapp`, rama `kiloswap`, compilada con
     `bun run mainnet && bun run build:regular`. El resultado va a
     `${KILOSWAP_ROOT}/webapp`.
2. **`deploy/.env`**: copiar `.env.example` y ajustarlo. Después,
   `./setup.sh`. Imprime la línea `rpcauth=kiloswap:…` para el nodo.
3. **Cartera vacía del backend en el nodo**, en caliente:
   `bitcoin-cli createwallet kiloswap-empty true true`
   (sin claves y en blanco, solo para los caminos raros del backend).
4. **Macaroons**, en caliente: `./bake-macaroons.sh`.
   - Las raíces 31 y 32 están libres hoy (`lncli listmacaroonids`).
   - El macaroon del backend lleva la condición `kiloswap=v1`. Mientras
     LND no tenga el middleware activado, la condición no se comprueba.
     Por eso **no se arranca el backend hasta después de la ventana**.
5. **Red del stack**: `docker compose up --no-start`. Crea la red
   `172.30.91.0/24` sin arrancar nada.

## Ventana (cuando no haya alquileres de MRR a punto de acabar)

Mirar antes en el panel de MRR (`/api/overview`) que ningún alquiler acaba
en los 15 minutos siguientes.

1. **Copias**: `bitcoin.conf` → `bitcoin.conf.bak-<fecha>` y
   `lnd.conf` → `lnd.conf.bak-<fecha>`.
2. **Añadir a `bitcoin.conf`**, todo en un solo cambio:
   ```
   # Kiloswap
   zmqpubrawblock=tcp://<IP-del-host>:28332
   zmqpubrawtx=tcp://<IP-del-host>:28333
   rpcallowip=172.30.91.0/24
   rpcauth=kiloswap:<lo que imprime setup.sh>
   rpcwhitelist=kiloswap:getblock,getblockhash,getblockheader,getblockchaininfo,getbestblockhash,getblockcount,getrawtransaction,gettxout,gettxspendingprevout,getrawmempool,getmempoolinfo,getmempoolentry,estimatesmartfee,getnetworkinfo,getdeploymentinfo,getzmqnotifications,uptime,sendrawtransaction,testmempoolaccept,getnewaddress,getbalances,listunspent,gettransaction,getwalletinfo,getpeerinfo,getnodeaddresses
   # IMPRESCINDIBLE: sin esto, la lista blanca de arriba deja SIN PERMISOS
   # a todos los demás usuarios (LND, mempool, DATUM, Kilobot…)
   rpcwhitelistdefault=0
   ```
   - **ZMQ** escucha en la IP de la red local. El cortafuegos ya deja pasar
     a `172.16.0.0/12` (los contenedores) y a la red local, y solo publica
     bloques y transacciones (nada privado).
   - **Lista blanca**: son las llamadas que hacen el backend, el shim y el
     guardián, sacadas del registro RPC del laboratorio. Se añaden unas
     pocas de lectura de cartera (`getbalances`, `listunspent`,
     `gettransaction`, `getwalletinfo`, `getmempoolentry`) que no salieron
     en el registro, para los caminos raros con la cartera vacía.
3. **Añadir a `lnd.conf`**:
   ```
   [rpcmiddleware]
   rpcmiddleware.enable=true

   [sweeper]
   sweeper.nodeadlineconftarget=144
   ```
   - `invoices.holdexpirydelta=18` es el valor por defecto de LND y no hace
     falta.
   - **Comprobado el 25 nov: el LND de producción NO tiene el middleware
     activado.** El informe anterior decía que no hacía falta reiniciar
     LND, y era un error: hay que reiniciarlo.
4. **Reinicios, seguidos, para que sean una sola ventana:**
   ```
   systemctl --user restart bip110-node     # ~10 s sin plantillas de minado
   docker restart lnd-blake2b               # ~1 min sin canales; el wallet se desbloquea solo
   ```
5. **Comprobaciones:**
   - `bitcoin-cli getzmqnotifications`: muestra los 2 ZMQ.
   - `journalctl -u datum-convoy | grep "Server stats"`: mismos clientes
     que antes.
   - `lncli getinfo`: `synced_to_chain` y canales activos.
   - El bridge responde (`bridge.kilombino.com`).
   - Con el usuario `kiloswap`, `getblockcount` responde; con el usuario
     de siempre, todo sigue igual (LND, mempool, Kilobot).

**Vuelta atrás:** restaurar las dos copias y reiniciar lo mismo otra vez.
El stack de Kiloswap aún no ha arrancado, así que no hay nada que deshacer
en él.

## Después (sin cortes)

1. `docker compose up -d postgres shim guard`
   - Los logs del guardián deben decir `registered with lnd for the
     "kiloswap" caveat`.
2. `docker compose up -d boltz`
   - Arranca el backend y crea sus tablas.
   - Después, `./db-roles.sh`. El guardián lee las dos tablas con su
     usuario de solo lectura.
3. **Prueba de que el guardián manda.** Con el macaroon del backend:
   - `sendcoins` a una dirección cualquiera → rechazado por
     "kiloswap guard".
   - Con el guardián parado (`docker compose stop guard`) → LND rechaza
     incluso `getinfo`.
   - Volverlo a arrancar.
4. `docker compose up -d web`
   - Ver http://127.0.0.1:3260 y probar swaps de 10–50k sats en los dos
     sentidos con la cartera de Kilombino.
5. **Publicar.** Hasta este paso no se publica nada.
   - **Cloudflare:** en el túnel, una regla de entrada
     `swap.kilombino.com → http://127.0.0.1:3260` y el CNAME del
     subdominio.
   - **Tor:** en el Tor del sistema (no en el de Ammanaya), un servicio
     onion → `127.0.0.1:3261`. Su dirección va a `VITE_TOR_URL` en la
     siguiente compilación de la web.
   - **bridge.kilombino.com:** pestaña "Kiloswap", en la rama local
     `kiloswap-tab` de kilobridge. Es una tarjeta con comisiones, límites y
     un botón que abre swap.kilombino.com (en el .onion del bridge, el
     .onion de Kiloswap). No mete la web en un iframe:
     - Paul la hace no enmarcable a propósito (`frame-ancestors 'none'`).
     - En un iframe, los navegadores guardan aparte (por sitio) los datos
       de los swaps. Un swap empezado dentro del bridge no aparecería al
       abrir swap.kilombino.com directamente.
6. **Avisos:** `watch/watch.env.example` →
   `~/.config/kiloswap-watch/watch.env` y el servicio de usuario
   `kiloswap-watch`. El guardián le manda sus rechazos a
   `172.30.91.1:9599`.
7. **Copias:**
   - Contraseña en `/root/kiloswap-backup/passphrase` (600).
   - Timer `kiloswap-backup.timer` para la copia diaria completa.
   - Regla sudoers para que el vigilante lance la copia "change" tras cada
     cambio de swap:
     `<usuario> ALL=(root) NOPASSWD: /opt/kiloswap/deploy/backup/kiloswap-backup.sh change`

## Pausar o apagar

- **Pausar los swaps nuevos:** `touch ${KILOSWAP_ROOT}/webapp/PAUSED`.
  Los reembolsos, rescates y swaps en curso siguen funcionando.
  Para reanudar, borrar el archivo.
- **Parar todo:** `docker compose stop`. Ni el nodo ni LND se tocan. Los
  usuarios recuperan sus fondos con su clave de rescate cuando vence el
  plazo.
- **Revocar el acceso del backend a LND:** `lncli deletemacaroonid 31`.
