## 1. Qué es un grafo en kindling

Hoy kindling ya tiene grafos implícitos, pero cada plugin los inventa a su manera: el gateway MCP agrupa réplicas por `service` (`api.LabelService`), `kling db` ata agente↔copia con `kling.db.owner` y resuelve la copia en cada conexión (`internal/machine/copias_db.go:resolverCopia`), el sandbox marca `template`/`tenant` (`ext/sandbox/internal/pool`). Lo que falta es el **sustantivo en el núcleo**: un conjunto de máquinas con nombre, aristas declaradas y ciclo de vida atómico.

**Nodos**: una máquina nacida de una plantilla (`from`) o imagen, con su memoria, vCPUs, egress, `kling.ports`, volúmenes y política de despertar (`eager` o `lazy`). Se identifica dentro del grafo por nombre (`db`, `api`, `cache`); el daemon le pone `kling.graph=<id>` y `kling.graph.node=<nombre>`.

**Aristas** (todas dirigidas, todas explícitas, ninguna abre "la red"):

| Tipo | Qué es | Sobre qué se apoya hoy |
|---|---|---|
| `link` | A puede abrir TCP al puerto P de B, por el nombre `b.graph` | el proxy Postgres del modelo A generalizado (`pkg/credproxy/maquina.go:ResolveMachineFunc`, `internal/net/credproxy.go:SetCredentials`) |
| `credential` | A recibe un marcador cuya clave real la pone el proxy hacia B (Postgres) o hacia un dominio externo | `POST /machines/{ref}/credentials` con `upstream_machine`, tal cual |
| `share` | A y B ven la misma carpeta (`copy`/`ro`/`rw`) | `internal/machine/shares.go:resolveShares` |
| `depends` | A no arranca hasta que B está `ready` | etiquetas de estado tipo `kling.db.state` + `waitPort` del sandbox |
| `mcp` | A (agente) llama herramientas de B (servidor MCP) por el gateway | `ext/mcp/internal/gateway/aggregate.go:callLink` (ya existe `mcp.Link`) |

**Operaciones sobre el grafo entero**: `up` (arranca los `eager`, deja los `lazy` como plantilla-sin-instancia hasta la primera conexión), `freeze`/`thaw` (todos, en orden inverso a `depends`), `snapshot` (instante consistente: pausar todos → commit de todos → reanudar todos), `fork` (N copias del entorno completo, cada una un grafo nuevo), `rm`, `inspect` (nodos, estado, aristas, quién despertó a quién), `audit` (cada conexión por arista, en el registro que ya existe: `internal/machine/credaudit.go`).

## 2. Casos por plugin

**MCP** — hoy un servidor MCP no puede hablar con otro: el gateway los aísla por sesión (`aislamiento.go`). Con un grafo, "agente + navegador + memoria + fetch" es una unidad: el agente arranca, el navegador está `lazy` y despierta al primer `tools/call` que lo necesite; `snapshot` del grafo guarda el agente **con** su contexto y sus herramientas a medias; `fork` da 10 agentes idénticos partiendo del mismo punto (evaluaciones A/B de prompts). Frente a docker compose: 0 RAM congelada y fork de estado vivo, no reinicio.

**Sandbox** — un sandbox de agente de código con su Postgres, su Redis y un mock de S3: hoy el frontal reclama una máquina; con grafo reclama una **plantilla de grafo** precalentada (el `pool` fabrica grafos enteros congelados). El agente rompe la base → `kling graph snapshot`/undo devuelve las tres máquinas al mismo instante. Testcontainers arranca contenedores por test en segundos; aquí el grafo restaura en decenas de ms desde dorados.

**db** — "app + base + caché" por rama: `kling graph fork integ-main -n 1 -name integ-pr45` da un entorno de integración entero con IDs nuevos; las aristas se resuelven por `(grafo, nodo)`, así que la copia de `app` habla con la copia de `db`, nunca con la original (mismo mecanismo que `kling.db.owner`, sin reescribir nada dentro del invitado). `kling db attach` pasa a ser una arista `credential` más.

**IA** — pipeline Chispa→VON→herramienta: hoy `pkg/scheduler` despierta VON al llegar la petición; con `lazy` genérico, el gateway de IA deja de necesitar su propio planificador de thaw, y una cascada es un grafo `chispa → von-1.5b → von-3b` que solo enciende lo que la duda exige.

**Sin programar** — "mi tienda de prueba": web + base + pasarela en modo test, un YAML de 20 líneas, `kling graph up tienda`, y `snapshot` antes de tocar nada. Lo que hoy exige compose y saber de puertos se reduce a nombres.

Frente a Kubernetes: sin red plana, sin CNI, sin DNS de clúster; cada arista es una autorización. Frente a compose: el grafo se congela a coste cero, se forkea vivo y cada conexión queda auditada.

## 3. Diseño en el núcleo

**Modelo (`pkg/api/graph.go`)**:

```go
type Graph struct {
    ID, Name string
    Nodes map[string]GraphNode   // por nombre
    Edges []GraphEdge
    State string                 // running|frozen|partial
    Generation int               // sube en cada fork/snapshot
}
type GraphNode struct {
    From, Image string; VCPUs, MemMiB int; Egress string; AllowDomains []string
    Ports []int; Wake string /* eager|lazy */; IdleFreezeSeconds int
    Volumes []VolumeAttachment; Shares []ShareSpec; Labels map[string]string
    MachineID string // vacío si lazy aún sin instanciar
}
type GraphEdge struct {
    From, To, Kind string        // kind: link|credential|share|depends
    Port int; Env, User, Database string; Secret string `json:"-"`
}
```

**Declarativo** (`graph.yaml`, JSON equivalente):

```yaml
name: tienda
nodes:
  db:    {from: pg16-golden, ports: [5432], wake: lazy, idle_freeze: 120}
  api:   {from: api-node, ports: [8080], egress: allowlist, allow_domains: [api.stripe.com]}
  web:   {from: web-static, ports: [80]}
edges:
  - {from: api, to: db, kind: credential, port: 5432, user: app, database: shop, env: PGPASSWORD}
  - {from: web, to: api, kind: link, port: 8080}
```

**API del daemon** (capacidad `graphs` en `GET /info`): `POST /graphs` (cuerpo `Graph`), `GET /graphs`, `GET /graphs/{ref}`, `POST /graphs/{ref}/freeze|thaw|snapshot|fork`, `DELETE /graphs/{ref}`. Persistencia en el store del daemon (`$KLING_ROOT/store/graph/<id>.json`, `internal/daemon/store.go`) más las etiquetas en cada máquina, que son la fuente de verdad para resolver aristas tras un reinicio (`reconcile.go`).

**CLI**: `kling graph up f.yaml | ls | inspect <g> | freeze <g> | thaw <g> | snapshot <g> [-name] | fork <g> -n N | rm <g> | audit <g>`. Cuelga bajo `tree.go` como sustantivo nuevo.

**Cómo se conectan dos invitados sin abrir nada** (Linux). Cada máquina vive en su netns con un veth `172.30.a.b/30` y el host llega a ella por DNAT a `172.16.0.2` (`internal/net/netns_fc.go:Setup`); el FORWARD entre netns sigue cerrado (`firewall.go:HostEgressRules`). Un `link` no toca eso: en el lado host del veth de A se arranca un **proxy de enlace** TCP (mismo sitio que `credPort`/`pgPort`, `internal/net/credproxy.go:startCredProxy`), el resolver de A contesta `b.graph → n.HostIP` (`dnsresolver.go:setCredHosts`) y un DNAT lleva `n.HostIP:P` al proxy. El proxy, **en cada `accept`**, llama a un `ResolveMachineFunc` del manager que comprueba bajo `m.mu`: B existe con ese ID exacto, lleva el mismo `kling.graph` que A, `kling.graph.node=b`, expone P en `kling.ports`, y la arista `(a→b,P)` está en el grafo; entonces devuelve la `NSIP:P` de B (copia literal de `comprobarCopiaLocked`/`direccionCopiaLocked`). Nada se fija al crear; borrar o reetiquetar B corta sus sesiones con `credproxy.Proxy.Invalidar` (`maquina.go:199`). Novedad mínima en `SetCredentials`: hoy exige resolver propio (allowlist); con aristas, el daemon arranca el resolver también en `egress none` sirviendo **solo** nombres `*.graph`.

**Despertar perezoso**: en ese mismo `accept`, si B está `frozen` o `paused`, el resolvedor lanza `Manager.Thaw` (`manager.go:1954`) o `reanudarLocked` (`pausa.go:84`) y bloquea la conexión hasta que el puerto de B contesta (`waitPort`, el que hoy usa el sandbox). El SYN del invitado queda retenido en el proxy: el cliente solo ve latencia (~ms desde dorado, 0,3 ms si estaba pausado). `IdleFreezeSeconds` reutiliza el vigilante de TTL con `on_ttl: freeze`, y cada conexión aceptada llama a `Renew` (`exec.go:85`). Un nodo `lazy` sin instancia se crea con `Run(from)` en ese momento.

**Consistencia de snapshot/fork**: `Commit` (`snapshot.go:40`) pausa, vuelca y reanuda una máquina. Para el grafo: (1) `Pause` de todos (`pausa.go:30`) en orden `depends` inverso; (2) `Invalidar` de todas las sesiones de enlace del grafo (las TCP en vuelo no sobreviven a una restauración y es mejor que mueran limpias que zombis); (3) `Commit` de cada nodo con nombre `<grafo>-<nodo>-<gen>` (Commit sobre una máquina ya pausada: variante `commitLocked(reanudar=false)`); (4) reanudar todos. Si un commit falla, se reanuda todo y se borran los snapshots parciales. **Fork** = snapshot temporal del grafo (marca `forkMarca`, `fork.go`) + `Run(from)` de cada nodo con `kling.graph=<id nuevo>` y `kling.fork-of`; como las aristas se resuelven por `(grafo, nodo)`, las copias se ven entre sí sin reescribir nada, y no ven al original. Secretos de aristas `credential` se reentregan a cada copia (`PUT /snapshots/{name}/credentials` ya hace esto por plantilla).

**Límites**: ≤32 nodos, ≤64 aristas, ≤16 conexiones concurrentes por arista, `up` calcula la suma de `mem_mib` de los `eager` contra la admisión antes de arrancar el primero (`admision.go`; hoy fallaría a medias con 507), tope global `KLING_MAX_MACHINES`. Un despertar perezoso que no cabe devuelve conexión rechazada y una línea de auditoría `reason: no_capacity`.

**Qué NO hacer**: nada de bridge L2 ni subred compartida entre invitados; nada de UDP ni ICMP; sin DNS general (`*.graph` y punto); sin reescritura de IPs o configs dentro del invitado; sin grafos de grafos ni aristas entre grafos; sin resolver una vez y cachear; sin TLS "de mentira" entre nodos (el tramo es el veth del host, como en attach); sin mesh, retries ni balanceo: eso es del plugin.

## 4. MVP (≤ 2 semanas, Linux)

**Entra**: `api.Graph` + validación; `POST/GET/DELETE /graphs`, `freeze`, `thaw`, `snapshot`, `fork`; nodos `eager` y `lazy`; arista `link` (TCP, con despertar perezoso) y arista `credential` (envoltorio de attach); persistencia por etiquetas + store; `kling graph up|ls|inspect|freeze|thaw|snapshot|fork|rm`; auditoría `kind: link` en `credaudit.jsonl`.

**No entra**: `share` y `depends` como aristas (se aceptan en el YAML solo si son `shares` del propio nodo), arista `mcp`, enlaces en macOS, `IdleFreeze` por conexión (solo TTL global), grafos precalentados en el pool.

**Tests unitarios**: `pkg/api/graph_test.go` (validación: ciclos en `depends`, puertos no expuestos, arista a nodo inexistente, tamaño); `internal/machine/grafo_test.go` con el Firecracker falso (`fcfalso_test.go`): resolvedor rechaza otro grafo, otro nodo, nodo parado, puerto ausente; lazy crea instancia una sola vez bajo 10 `accept` concurrentes; snapshot pausa-todos/commit-todos/reanuda-todos y deshace en fallo; fork produce grafo nuevo cuyas aristas no resuelven al original. `pkg/credproxy/enlace_test.go`: proxy TCP crudo con `ResolveMachine` por conexión e `Invalidar`.

**e2e**, sección nueva `step "8. Grafos"` en `scripts/90-e2e.sh` tras 7f: `graph up` de 3 nodos (`web→api→db`, `db` lazy); desde `web`, `exec curl api.graph:8080` responde y `db` pasa `frozen→running` al primer `psql`; desde `web`, `db.graph` da NXDOMAIN (sin arista); `graph freeze` deja los tres `frozen`, `graph thaw` los devuelve; `graph snapshot` crea tres plantillas con la misma generación; `graph fork -n 2` produce dos grafos cuya `api` llega a su propia `db` y no a la original (comprobado con un marcador escrito en la base); `graph rm` limpia máquinas y plantillas temporales; daemon reiniciado (bloque 4) conserva el grafo y sus enlaces.

**Riesgos de seguridad y cierre**: (a) TOCTOU de dirección — resuelto en cada conexión, nunca cacheado, mismo diseño que attach; (b) un invitado no puede fabricar aristas: los IDs y etiquetas `kling.graph*` los pone el daemon y `SetLabels` (`manager.go:2269`) rechaza cambiarlas; (c) tormenta de despertares — un solo thaw en vuelo por nodo (candado `lifecycle.tomar`), cola acotada, 503 a partir de ahí; (d) fuga entre grafos forkeados — el ID de grafo va en la comprobación, e2e negativo obligatorio; (e) el tramo proxy→B va en claro por el host, igual que hoy con Postgres: se documenta y las credenciales siguen exigiendo SCRAM.

**macOS vs Linux**: en macOS la red vive dentro de cada `kling-vz` y todos los invitados comparten IP con reenvíos de loopback (`api.Machine.Addr`); no hay lugar donde resolver bajo el candado del daemon en cada conexión, por eso attach está prohibido allí (`copias_db.go:errModeloASoloLinux`). El MVP hace lo mismo: `up/freeze/thaw/snapshot/fork/rm` funcionan en macOS, y una arista `link` o `credential` a otra máquina devuelve `501` con el mismo texto. Segunda fase: `kling-vz` pregunta al daemon por socket antes de cada dial (la `credential_kinds` de `vz/internal/server/server.go:786` ya es el sitio para anunciar `graph-link`).

## 5. Mensaje de producto

Un grafo de kindling es un entorno completo —app, base, caché, herramientas del agente— descrito en un fichero, donde cada máquina solo llega a la que el grafo dice y se despierta cuando alguien la llama. Se congela entero a coste cero, se guarda en un instante consistente y se ramifica en N copias vivas que no se ven entre sí. Hoy en Linux, con enlaces TCP y credenciales; en macOS, el mismo grafo sin enlaces entre máquinas.