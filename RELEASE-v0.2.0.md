# kindling v0.2.0

20 commits desde `v0.1.0`. 56 ficheros, **+8 070 / −235** líneas, y **cero dependencias
externas** — el `go.mod` no ha cambiado un byte.

La versión anterior demostraba que la idea funciona: microVMs que descongelan en
milisegundos y un gateway que las despierta bajo demanda. Esta la hace instalable,
autenticada y capaz de guardar estado, y arregla nueve fallos que solo aparecen cuando
metes servicios de verdad.

---

## Lo que cambia de un vistazo

| | v0.1.0 | v0.2.0 |
|---|---|---|
| Instalación | 3 scripts a mano, como root | `kling up` (kernel e imagen dentro del binario) |
| Gateway | abierto a quien alcance el puerto | token obligatorio, `crypto/subtle` |
| Clientes de IA | 2 | **7** |
| Catálogo de servidores MCP | a mano | registro oficial: `kling search` / `kling add` |
| Estado persistente | ninguno | volúmenes con journal, hasta 4 por microVM, compartibles en lectura |
| Tests | 2 | **56** + 6 benchmarks |
| Prueba de extremo a extremo | — | `90-e2e.sh`, 21 comprobaciones |
| CI | — | gofmt + vet + `go test -race` en cada push |
| Dependencias externas | 0 | **0** |

---

## Superficie de comandos

### Comandos nuevos

| Comando | Qué hace |
|---|---|
| `kling up` | Deja el runtime listo: comprueba KVM, nftables, el usuario `kindling`, artefactos e imágenes, y arranca daemon y gateway. Lo que exige privilegios lo **imprime** en vez de ejecutarlo |
| `kling status` | Diagnóstico de una pasada: endpoint, daemon, KVM, firecracker, gateway y agentes de IA detectados |
| `kling search <consulta>` | Busca en `registry.modelcontextprotocol.io` |
| `kling add <servidor>` | Empaqueta un servidor del registro, lo importa y lo deja congelado como servicio |
| `kling volume create\|ls\|rm` | Almacenamiento que sobrevive a la microVM |

`kling run`, `kling add` y `kling mcp import` aceptan los tres `-volume` y `-mount`.

### Comandos que cambian

| Comando | Cambio |
|---|---|
| `run` | Nuevos `-volume` y `-mount` |
| `gateway` | Exige `Authorization: Bearer` por defecto. Nuevos `-no-auth` y `-pprof`, ambos solo en loopback |
| `connect` | `-install all` cubre 7 clientes; nuevo `-token` |
| `mcp import` | Nuevos `-volume` y `-mount`. Y ahora **aplica `defaults.mem_mib`**, que era el fallo detrás de los timeouts en paralelo |
| `config` | Nueva clave `gateway.token`, enmascarada al mostrarla |

### Clientes soportados por `connect -install`

| Cliente | v0.1.0 | v0.2.0 |
|---|:--:|:--:|
| Claude Code | ✅ | ✅ |
| opencode | ✅ | ✅ |
| Cursor | — | ✅ |
| VS Code | — | ✅ |
| Windsurf | — | ✅ |
| Cline | — | ✅ |
| Zed | — | ⚠️ imprime el rodeo con `mcp-remote` |

Zed no se parchea a propósito: no habla MCP remoto por HTTP y su `settings.json` admite
comentarios, así que reescribirlo destruiría la configuración del usuario.

---

## Rendimiento

### Búsqueda de herramientas en el catálogo (`find_tools`)

El `haystack` —el texto sobre el que se busca— se precomputa al construir el catálogo en
vez de recomponerlo en cada consulta. El resultado es inmutable, así que pagarlo una vez
por herramienta y búsqueda era trabajo tirado.

| Herramientas | v0.1.0 | v0.2.0 | Mejora | Basura generada |
|---:|---:|---:|---:|---|
| 10 | 4 067 ns | **2 377 ns** | 1,7× | 1 888 B / 20 allocs → **0** |
| 100 | 40 090 ns | **24 100 ns** | 1,7× | 19 264 B / 200 allocs → **0** |
| 1 000 | 405 300 ns | **242 090 ns** | 1,7× | 194 240 B / 2 000 allocs → **0** |

Lo que más importa no es el 1,7×: es la columna de la derecha. Con 50 herramientas
cargadas y un agente que busca en cada turno, la versión anterior generaba basura
proporcional al catálogo **en cada búsqueda**. Ahora no genera ninguna.

### Otras medidas

| Qué | Coste | Nota |
|---|---:|---|
| `expandTerms` | 539 ns | Una vez por búsqueda, no por herramienta |
| `Coerce` (esquema ya deserializado) | 182 ns / 3 allocs | El recorrido del esquema es barato |
| `CoerceArgs` (caso sano, extremo a extremo) | 5 734 ns / 127 allocs | El coste está en el JSON de ida y vuelta, no en arreglar tipos |
| `persist` con 500 máquinas | 19,3 µs / 1 alloc | Bajo el lock global; con 50 máquinas es ruido |

Máquina de medida: Apple Silicon, `-benchmem`. Ninguno toca KVM, red ni daemon: corren en
CI como un test normal.

### Ciclo de vida, medido en hardware real

Ejecución de `scripts/90-e2e.sh` contra un daemon con KVM (Debian, i7):

| Operación | Tiempo |
|---|---:|
| Arranque en frío | 51 ms |
| Congelar a snapshot (`freeze`) | 2,06 s |
| **Descongelar (`thaw`)** | **25 ms** |

---

## Corrección: nueve fallos que solo salen con servicios de verdad

| Fallo | Síntoma que daba | Causa |
|---|---|---|
| `mcp import` ignoraba `defaults.mem_mib` | «~25 % de timeouts en paralelo», atribuido al gateway | Los servicios se congelaban con 256 MiB; el OOM-killer mataba node y el puente sobrevivía, así que la microVM quedaba viva y sorda |
| El snapshot no guardaba la política de red | Todo servidor MCP que use internet fallaba al despertarlo | El snapshot se restauraba con `egress: none` |
| El snapshot no guardaba su volumen | La escritura decía «success» y el fichero no estaba después | El gateway despertaba el servicio sin el disco; se escribía en un overlay que moría con la máquina |
| Volúmenes sin journal | `EBADMSG` al leer desde la siguiente microVM | Se reutilizaba el formateador de overlays (`-O ^has_journal`), correcto para algo desechable y fatal para algo que debe sobrevivir a matar el VMM |
| Dos microVMs podían montar el mismo ext4 | Corrupción silenciosa | Faltaba el guard: un ext4 no admite dos escritores |
| Bucle infinito de reintentos en el gateway | El proxy giraba durante minutos | El `ErrorHandler` reservía una petición cuyo cuerpo ya se había consumido |
| El segador del gateway congelaba microVMs con trabajo en vuelo | `read: connection timed out` de TCP tras varios minutos, que parece un servidor caído | La inactividad se medía por la LLEGADA de peticiones: una herramienta más lenta que `gateway.idle` se quedaba sin máquina a media faena |
| El puente cortaba toda respuesta a los 120 s | La misma llamada fallaba siempre en el mismo segundo exacto | Plazo incrustado, más estricto que el del gateway que tiene delante, y con un mensaje que decía «no respondió» en vez de «sigo trabajando» |
| El empaquetador ejecutaba código ajeno como root | — | `npm install` sin `--ignore-scripts` durante el chroot |

Tres clases de timeout mal calibrados estrangulaban trabajo legítimo (construir una imagen,
`/guest`, el proxy del gateway). Uno de ellos, además, mataba el script de construcción a
media faena con `SIGKILL` —donde el `trap` de bash no llega— y dejaba la imagen montada en
escritura: el invitado siguiente entraba en pánico con `EUCLEAN`, y eso se veía como «el
servidor no abrió el puerto».

---

## Volúmenes

```sh
kling volume create notas -size 2G
kling run -name apuntes -volume notas -mount /data
kling mcp import notas-mcp -volume notas
```

Un fichero ext4 disperso en el host, expuesto como tercer disco (`vdc`).

**No es un directorio del host montado dentro**, y no por comodidad: Firecracker no tiene
virtio-fs, y sobre todo un directorio del anfitrión en escritura le daría al invitado —que
se considera hostil— un canal directo al sistema de ficheros de fuera. Sería tirar por
tierra la frontera que justifica usar microVMs en vez de contenedores.

| Restricción | Por qué |
|---|---|
| Un solo escritor a la vez | Un ext4 no admite dos sistemas montándolo en escritura: cada uno cachea metadatos que el otro no ve |
| Se declara al importar, no después | Firecracker no permite añadir discos a una VM restaurada; el dispositivo tiene que existir cuando se congela el snapshot |
| Formateado con journal | Parar una microVM es matar el VMM, que para el invitado es un corte de corriente |

Antes de matar una máquina, el daemon le pide al invitado que vacíe la caché al disco; si
el apagado es ordenado, el puente además desmonta. Antes de cada arranque se le pasa un
`e2fsck -p`, que sobre un volumen sano cuesta milisegundos.

---

## Pruebas

| | v0.1.0 | v0.2.0 |
|---|---:|---:|
| `cmd/kling` | 0 | 6 |
| `cmd/kling-bridge` | 0 | 5 |
| `internal/daemon` | 0 | 2 |
| `internal/gateway` | 2 | 17 |
| `internal/machine` | 0 | 26 |
| **Total** | **2** | **56** |
| Benchmarks | 0 | 6 |
| Ficheros `_test.go` | 1 | 15 |

Los tests de Go cubren la lógica; no pueden cubrir lo que de verdad se rompe aquí. Para eso
está `scripts/90-e2e.sh`, que corre contra un daemon real con KVM:

```
1. Daemon                 KVM disponible · firecracker instalado
2. Ciclo de vida          arranque en frío 51 ms · freeze → warm · thaw 25 ms
3. Volumen persistente    creado · con journal · no se borra en uso ·
                          no se monta dos veces · dice quién lo usa ·
                          se libera al destruir · queda limpio tras matar el VMM
4. Gateway                /services sin token → 401 · /healthz abierto → 200
5. Reinicio del daemon    las microVMs vivas sobreviven

15 ok · 0 fallo(s)
```

---

## Notas de actualización

- **El gateway ahora exige token.** `make deploy` genera uno en `/etc/kling/gateway.env` la
  primera vez y lo conserva. Apunta tu CLI con `kling config set gateway.token …` y vuelve a
  correr `kling connect -all -install all`.
- **Los servicios importados con v0.1.0 conviene reimportarlos.** Sus snapshots no guardan la
  política de red, así que los que usen internet fallarán al despertar.
- **`make deploy` no actualiza el puente dentro de las imágenes ya construidas.** El puente
  vive *dentro* de cada imagen y se copia cuando la imagen se construye, así que un servicio
  empaquetado antes seguirá con el puente viejo — y sin el vaciado de volumen que esta
  versión añade. Reconstruye los que usen volumen con `kling add <servidor> -volume …`.
- **Los volúmenes creados antes de esta versión no tienen journal.** `kling volume ls` los
  sigue mostrando, pero no sobrevivirán bien a un apagado brusco: recréalos.
