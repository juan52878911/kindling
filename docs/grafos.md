# Grafos de microVMs

Un grafo es un entorno completo —app, base, caché— descrito en un fichero: varias
máquinas con nombre y las **aristas** que dicen quién llega a quién. Cada arista es
una autorización, no "la red": un nodo solo alcanza a otro por una arista declarada,
y el daemon la resuelve en cada conexión. El grafo se congela entero, se guarda en un
instante consistente y se ramifica en N copias vivas que no se ven entre sí.

El diseño y sus decisiones están en [grafos-diseno.md](grafos-diseno.md); esto es el
uso. Capacidad `graphs` del daemon ([api.md](api.md#grafos)).

## Un ejemplo

```yaml
# tienda.yaml
name: tienda
nodes:
  db:    {from: pg16-golden, ports: [5432], wake: lazy, idle_freeze: 120}
  api:   {from: api-node, ports: [8081], egress: allowlist, allow_domains: [api.stripe.com]}
  web:   {from: web-static, ports: [8000]}
edges:
  - {from: api, to: db, kind: credential, port: 5432, user: app, database: shop, env: PGPASSWORD, secret_env: SHOP_PG_PASS}
  - {from: web, to: api, kind: link, port: 8081}
```

```console
$ SHOP_PG_PASS=... kling graph up tienda.yaml
graph tienda (4f2a9c1e0b7d) up in 412ms
  NODE  STATE          MACHINE       WAKE   FROM        PORTS
  api   running        9c1e0b7d4f2a  eager  api-node    8081
  db    (not started)  -             lazy   pg16-golden 5432
  web   running        0b7d4f2a9c1e  eager  web-static  8000
```

Dentro de `web`, `http://api.graph:8081` llega a `api`. Dentro de `api`, `db.graph:5432`
llega a `db` con la contraseña real puesta por el proxy: `api` solo ve un marcador en
`PGPASSWORD` (el attach de [kling db](db.md), por dentro). `db` es `lazy`: no tiene
máquina hasta el primer `psql` de `api`, que espera lo que tarde en arrancar (ms desde
una plantilla) y sigue.

## El fichero

JSON o YAML. El YAML es un subconjunto sin dependencias: mapas y listas por sangría
(espacios), colecciones en línea `{a: b}` y `[x, y]` en una línea, escalares planos o
entre comillas y comentarios `#`. Anclas, etiquetas, bloques `|`/`>` y varios
documentos dan un error con su línea. Un campo que no existe es un error, en YAML y en
JSON.

**Nodos** (por nombre; el nombre es una etiqueta DNS: el nodo es `<nombre>.graph`):

| Campo | Qué es |
|---|---|
| `from` o `image` | plantilla (arranca en ms) o imagen (arranque en frío); uno de los dos |
| `vcpus`, `mem_mib` | como en `run`; 0 = los de la plantilla |
| `egress`, `allow_domains` | como en `run`. Una arista no abre nada de esto |
| `ports` | puertos que el nodo expone a sus aristas (`kling.ports`) |
| `wake` | `eager` (por defecto: arranca con `up`) o `lazy` (en la primera conexión) |
| `idle_freeze` | segundos hasta congelarse solo (el TTL de siempre con `on_ttl: freeze`) |
| `volumes`, `shares`, `allow_exec`, `labels` | como en `run`; `shares` solo con `image` |

**Aristas**:

| Campo | Qué es |
|---|---|
| `from`, `to` | nodos del grafo |
| `kind` | `link`, `credential`, `share` o `depends` (ver [Aristas share y depends](#aristas-share-y-depends)) |
| `port` | puerto de `to` (tiene que estar en sus `ports`); en `credential`, 5432 por defecto; en `depends`, opcional; en `share`, ninguno |
| `env`, `user`, `database` | solo `credential`: la variable del marcador, el rol y la base |
| `mount`, `mode` | solo `share`: la ruta de la carpeta en los dos invitados y cómo la monta `from` (`ro` por defecto, o `rw`) |
| `secret_env` / `secret_file` | solo `credential` y solo en el fichero: de dónde saca `kling` la clave (una variable de SU entorno, o un fichero relativo al del grafo). Con una sola arista `credential` sin fuente, stdin |

La clave nunca va en el fichero ni sale por la API: viaja una vez al daemon, que la
guarda cifrada (`store/graph/<id>.secrets.enc`) para los nodos lazy y los forks.

Límites: 32 nodos, 64 aristas, 16 conexiones a la vez por arista `link`. Todas las
aristas de un nodo llegan por la misma dirección (la del host en su veth en Linux, la
pasarela de su red en macOS), así que un nodo alcanza **un nodo por puerto**, y los
puertos 53, 80 y 443 no valen en un `link` (son el DNS y el proxy de credenciales de
esa dirección).

## Operaciones

| Comando | Qué hace |
|---|---|
| `kling graph up <f>` | crea el grafo y arranca los `eager` (y los nodos de los que dependen), en orden de `depends`; comprueba antes el tope de máquinas y la memoria de todos ellos. Todo o nada |
| `kling graph ls` · `inspect <g>` | estado (`running`, `frozen`, `partial`), nodos, aristas y generación |
| `kling graph freeze <g>` · `thaw <g>` | todos los nodos con máquina (un `lazy` sin máquina sigue sin ella, salvo que dependa de él uno que despierta); `thaw` en orden de `depends`, `freeze` al revés |
| `kling graph snapshot <g> [-name N]` | una plantilla por nodo, `<N>-<nodo>-<gen>`, **todas del mismo instante** |
| `kling graph fork <g> -n N` | N grafos nuevos desde este instante |
| `kling graph rm <g>` | el grafo, sus máquinas y las plantillas temporales de fork que ya no use nadie |

`<g>` es el nombre, el ID o un prefijo único del ID. Las máquinas se llaman
`<grafo>-<nodo>` y se ven en `kling ps`; cada una lleva `kling.graph=<id>` y
`kling.graph.node=<nodo>`, que solo pone el daemon (`run`, `sandbox`, `fork` y
`PUT labels` las rechazan).

**Snapshot**: pausa todos los nodos que corren, corta las sesiones hacia ellos, vuelca
cada uno sin reanudarlo y los reanuda al final: todos los volcados son del instante de
la pausa. Si un volcado falla, se borran las plantillas hechas y se reanuda todo. Un
nodo congelado no se vuelca (despierta el grafo antes) y un nodo con volúmenes
tampoco en esta versión (soltarlos pide hablar con el invitado, y pausado no contesta).

Las plantillas de un snapshot son **persistentes**: no se borran con el grafo (`graph
rm` solo quita las temporales de un fork); se quitan con `kling snapshot rm`. La de un
nodo con aristas `credential` lleva en su memoria los **marcadores** que tenía el
invitado, no las claves: ni la plantilla ni sus instancias reciben el almacén de
credenciales del nodo, y un marcador solo sirve en el proxy de la máquina a la que se
entregó. Una instancia arrancada de esa plantilla con `run -from` tiene marcadores
inertes. Ver [SECURITY.md §15](../SECURITY.md#15-grafos-cada-arista-es-una-autorización-no-una-red).

**Fork**: un snapshot consistente temporal y, por cada copia, un grafo nuevo con otro
ID cuyos nodos arrancan de él. Las aristas se resuelven por (grafo, nodo): la `api`
de una copia llega a la `db` de su copia y nunca a la del original, sin tocar nada
dentro de los invitados. Los invitados despiertan con los marcadores del original en
memoria, así que cada copia recibe los mismos marcadores apuntados a su grafo. Un
nodo con credenciales que no son de sus aristas (`kling machine credential` a mano)
no se ramifica: esas claves no se multiplican sin pedirlo.

**Auditoría**: cada conexión por un `link` es una línea `kind: link` en el registro
del nodo de origen (`kling machine audit <grafo>-<nodo>`): destino, máquina a la que
llegó, bytes, duración y, si no llegó, por qué (`machine_unavailable`, `busy`,
`no_capacity`, `invalidated`). Las de `credential` son las de Postgres de siempre.

## Aristas share y depends

Ninguna de las dos conecta máquinas por red: no abren puertos ni tocan el resolver.

**`depends`**: `{from: api, to: db, kind: depends}` dice que `api` no arranca ni
despierta hasta que `db` está **listo**: corriendo y, si la arista lleva `port` (que
`db` tiene que exponer), con ese puerto contestando (como mucho 2 minutos). De ahí sale
el orden de todo el grafo:

- `up` arranca en orden (cada nodo cuando los suyos están listos). Un `lazy` del que
  depende un `eager` arranca con `up`: sin él, el `eager` no podría.
- `thaw` y el despertar de un nodo por su primera conexión siguen el mismo orden: si
  `api` despierta, antes despierta (o se instancia) `db`.
- `freeze`, y la pausa de un `snapshot`, van al revés: primero quien depende.
- Un ciclo (`a → b → a`) se rechaza al validar el fichero. Si una dependencia no
  arranca, el nodo que depende de ella tampoco (y `up` deshace todo).

Una `depends` vale en todas las plataformas. Con puerto, en Linux el daemon marca a la IP
del netns del destino; en macOS pregunta al `kling-vz` del destino si algo escucha en
ese puerto (`GET /kling/probe`), sin marcar desde el host. El 8080 (el agente) tampoco
vale aquí.

**`share`**: `{from: web, to: files, kind: share, mount: /data}` hace que `web` vea la
carpeta `/data` **de `files`**. De quién es la carpeta:

- Es **del grafo**: la crea el daemon en `$KLING_ROOT/graph-shares/<grafo>/<nodo>/…`
  (0700) al hacer `up`, y `graph rm` la borra con todo lo que tenga. No está bajo
  `daemon.share_roots` ni hace falta: solo la montan los nodos de ese grafo, y ninguna
  petición de `run` puede pedirla (el daemon solo la acepta al arrancar el nodo).
- `to` es el dueño y la monta en lectura y escritura en `mount`. Cada `from` la monta en
  la misma ruta con `mode`: `ro` (por defecto) o `rw`. Varios nodos pueden ver la misma
  carpeta (varias aristas al mismo `to` y `mount`).
- Es una carpeta **viva** (la de `-share ro|rw`, [compartir.md](compartir.md)): lo que
  escribe uno lo ve el otro. `copy` no vale: es una subida hecha una vez para una
  máquina, no una carpeta que ven dos.
- Los dos nodos arrancan en frío (`image`, no `from`), como los `shares` propios: una
  carpeta viva se monta al arrancar. Una misma ruta no puede ser dos carpetas en un
  nodo, ni una estar dentro de otra, y caben 8 por nodo contando las suyas.

**Snapshot y fork de un grafo con `share`: no en esta versión (409).** La memoria
volcada de un nodo lleva montada su carpeta viva y cada instancia restaurada despertaría
con un montaje que no le corresponde; es el mismo motivo por el que `kling commit` no
toma una máquina con `-share`. `freeze` y `thaw` sí funcionan: la carpeta se vuelve a
enganchar al despertar. Cuando se levante, lo definido es que cada copia de un fork
tenga **su propia copia** de la carpeta, nunca la del original: igual que sus nodos, las
copias no se ven entre sí.

## Cómo llega un nodo a otro (Linux)

Nada de red entre máquinas: el FORWARD entre namespaces sigue cerrado.

1. El resolver del nodo (el de allowlist; en `none` e `internet` se arranca uno) contesta
   `api.graph` con la IP del host en el veth del nodo, y **NXDOMAIN a cualquier otro
   `*.graph`**. En `egress none`, todo lo demás es NXDOMAIN también.
2. Un DNAT del netns lleva ese puerto de esa IP a un **proxy de enlace** del daemon, en
   el lado host del veth.
3. En **cada** conexión aceptada, el proxy pregunta al daemon a dónde va: la máquina de
   origen es la del nodo, el grafo tiene la arista, la máquina del destino es la que el
   grafo dice, lleva sus etiquetas y expone el puerto. Solo entonces marca a la IP del
   netns del destino, que es lo que el host hace siempre para hablar con un invitado.
4. Si el destino está congelado, pausado o es un `lazy` sin máquina, la conexión
   espera: un solo despertar por nodo, hasta 64 conexiones esperándolo (la siguiente se
   rechaza, `busy`), y cuando su puerto contesta, sigue. Si no cabe en el host,
   `no_capacity`.
5. Congelar, pausar, parar o borrar un nodo corta las sesiones hacia él.

El tramo del proxy al destino va en claro por el host, como el attach de Postgres; las
credenciales siguen exigiendo SCRAM.

## Cómo llega un nodo a otro (macOS)

Lo mismo, con otra fontanería: allí la red de cada invitado vive dentro de su
`kling-vz` (una pila de red de usuario) y todos los invitados se alcanzan por reenvíos
del loopback. `kling-vz` no conoce a las demás máquinas ni recibe nunca una dirección.

1. El DNS de su `kling-vz` contesta los `<nodo>.graph` de SUS aristas con la pasarela
   (`172.16.0.1`), y NXDOMAIN a cualquier otro `*.graph`, en todos los modos.
2. Una conexión del invitado a la pasarela en el puerto de una arista `link` (o al
   proxy de Postgres, para una `credential`) se convierte en una petición al **broker
   de enlaces** del daemon: un socket Unix en un directorio privado del usuario, el
   único que el sandbox de `kling-vz` le deja abrir. La petición dice qué arista
   (`api.graph:8081`), no a dónde.
3. El daemon sabe qué máquina pregunta por el PID del otro extremo del socket, hace
   las mismas comprobaciones que en Linux bajo su candado (despertando al destino si
   duerme), **marca él mismo** al reenvío del destino, comprueba que sigue siendo el
   mismo y le entrega a `kling-vz` el socket ya conectado.
4. Cortar (congelar, pausar, parar o borrar el destino) es cerrar ese socket en los
   dos lados: el daemon guarda su copia. Si el daemon se reinicia, las sesiones en
   curso se cortan; las nuevas funcionan en cuanto vuelve.

La auditoría es la misma (`kind: link` en el registro del nodo de origen). Hace falta
un `kling-vz` que anuncie `graph-link` en `credential_kinds`: con uno anterior, las
aristas fallan cerradas y el daemon dice que hay que reconstruirlo. Ver
[SECURITY.md §15](../SECURITY.md#15-grafos-cada-arista-es-una-autorización-no-una-red).

Las aristas `share` y `depends` también funcionan igual. Una `depends` con `port` espera
a que ese puerto escuche preguntando al `kling-vz` del destino (`GET /kling/probe`, el
mismo sondeo que usa `exec`), no marcando desde el host: el puerto no tiene por qué
tener reenvío.

## Lo que no está en esta versión

La arista `mcp`, snapshot y fork de un grafo con `share` e `idle_freeze` renovado por
conexión (hoy es el TTL de siempre de la máquina). Los grafos precalentados en el
fondo del sandbox ya están (#57, abajo en "Desde los plugins").

**Por qué no hay arista `mcp`.** El diseño era que un agente llamase a las herramientas
de un servidor MCP del grafo por su puente (`kling-bridge`). Pero el puente escucha solo
en el 8080 y ahí mismo sirve el agente de invitado (`/exec`, `/volume/*`), y ninguna
arista llega nunca al 8080 (abajo). Abrirlo daría a un nodo el control de otro, así que
se rechaza al validar con esta explicación. Hasta que el puente tenga un puerto solo para
MCP, un servidor MCP que hable HTTP en su propio puerto (las imágenes `transport: http`)
se alcanza con una arista `link` a ese puerto.
## Desde los plugins

Los plugins usan el grafo del núcleo (`/graphs`, `kling graph`) para sus entornos; el
núcleo no sabe de ellos.

**kling db: `kling db env`.** `kling db env up <app-template> -golden G` crea el grafo
`app` + `db` con una arista `credential` (el `kling db attach` de siempre, declarado): un
entorno de integración entero en un comando, con la clave por stdin y deshecho entero si
algo falla. `kling db branch -env <app-template>` lo hace por rama de git. `env down` y
`branch -rm` lo borran con la clave. Detalle en
[db.md](db.md#un-entorno-entero-app--base-como-grafo).

**kling-sandbox: grafos precalentados (#57).** Una plantilla con `"kind": "graph"`
declara un grafo (nodos que nacen de plantillas ya construidas, y sus aristas) con un
`pool`. El fondo del gateway levanta cada instancia con `POST /graphs` y la congela con
`graph freeze`; `POST /v1/graphs {"template": ...}` del frontal reclama una (etiqueta
todas sus máquinas con el inquilino, relee, y `graph thaw`) o, si no hay, levanta una
ya a nombre del inquilino. `DELETE /v1/graphs/{id}` la borra entera. Sin nodos `lazy`,
sin aristas `credential` y sin volúmenes ni carpetas del host en los nodos: serían
comunes a todas las instancias. Detalle en
[ext/sandbox/README.md](../ext/sandbox/README.md#grafos-precalentados).

**kling-mcp: agente + servidores MCP.** Un grafo sin aristas ya da lo que interesa:
`agente` eager y `browser`/`memoria` `lazy` (sin RAM hasta que se necesitan), ciclo de
vida atómico, `snapshot` del agente **con** sus herramientas del mismo instante y `fork
-n N` para comparar prompts o modelos partiendo del mismo punto. El fichero de ejemplo
es [`examples/grafos/agente-mcp.yaml`](../examples/grafos/agente-mcp.yaml) (un test lo
valida con el mismo parser). No hay comando nuevo y el gateway no cambia: sigue
llegando a cada servidor desde el host. Lo que **no** se puede hoy es una arista del
agente al servidor: el puente MCP escucha en el 8080 del invitado y una arista no puede
apuntar a ese puerto (ver abajo); es la arista `mcp` del diseño, pendiente. Tampoco el
gateway distingue las copias de un fork: el agente de una copia no llega por él al
navegador de su copia.

**Gateway de IA (Chispa/VON): siguiente paso, sin código.** La cascada es hoy Chispa
(una microVM pequeña que clasifica) y, si duda, VON (modelos mayores en microVMs), cada
una despertada y congelada por `pkg/scheduler` con su pool de réplicas
(`pkg/aigw/guestpool.go`). Como grafo sería `chispa` eager y `von` `lazy`, con una arista
`link` `chispa -> von`: VON solo tiene máquina cuando Chispa escala. No se prototipa
porque tocaría el planificador actual, y hay tres cosas que el grafo aún no cubre y que
el planificador sí: (1) **réplicas por servicio y tope por inquilino** (`MaxInflight`,
`MaxInstances`), que el grafo no modela (un nodo es una máquina); (2) el
**arrendamiento**: el planificador renueva el TTL de lo que despierta e `idle_freeze` no
se renueva por conexión; (3) los **límites del `link`** (una dirección por nodo, 16
conexiones, sin 80, 443 ni 8080). Sustituirlo sin esas tres cosas empeoraría la latencia
y el aislamiento. El camino: `idle_freeze` renovado por conexión, luego un grafo por
inquilino con el planificador solo para las réplicas de cada nodo. Mientras, el grafo
sirve para lo que sí encaja: `snapshot`/`fork` del par entero para evaluar una cascada
nueva contra la anterior partiendo del mismo estado.

## El puerto 8080 no es alcanzable por una arista

El agente de invitado de kindling (y el puente MCP en las imágenes MCP) escucha en el
8080 del invitado y no autentica: confía en que solo el host llega a él, y sirve `exec`,
ficheros y volúmenes. El proxy de enlace marca desde el host, así que una arista al 8080
le daría a otro nodo el control del destino. Por eso ninguna arista (`link` ni
`credential`) puede apuntar al 8080: se rechaza al validar el grafo y otra vez en cada
conexión. Expón el servicio en otro puerto (el ejemplo usa el 8081). Lo encontró el e2e
real en el lab: en la imagen toolchain el 8080 lo ocupa el agente.
