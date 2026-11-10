# kling-sandbox

> Parte de **kindling v0.13.0** (sin publicar). Hasta v0.2.2 esto era el
> repositorio aparte kindling-sandbox (archivado; sus releases siguen allí); desde
> kindling v0.13.0 vive en `ext/sandbox` del repositorio de kindling y sale en la
> misma release que el núcleo, con la misma versión. Las novedades van al
> [CHANGELOG de la raíz](../../CHANGELOG.md); el [antiguo](CHANGELOG.md) está
> congelado.

Sandboxes para agentes de código sobre las microVMs Firecracker de
[kindling](../../README.es.md), servidos por un frontal con
plantillas, inquilinos y varios hosts detrás.

kindling ya sabe crear un sandbox: `kling sandbox create` levanta una microVM de
usar y tirar, con ejecución dentro, sin red y con un plazo de vida. Lo que añade
esto es lo que el núcleo no debe llevar:

- **Un extremo en la red con autenticación.** El daemon de kindling escucha solo
  en un socket Unix y equivale a root en su host; no se le pone un token, se le
  pone un frontal delante. Es el mismo patrón del gateway de [kling-mcp](../mcp/README.es.md).
- **Plantillas como receta.** Se declara de qué imagen partir y qué instalar; de
  ahí sale un snapshot dorado del que los sandboxes nacen en milisegundos. Es una
  receta y no un artefacto porque los snapshots están atados a su host: un
  reinicio los invalida y hay que poder rehacerlos sin que nadie recuerde nada.
- **Inquilinos con cuota y propiedad.** Cada token es un inquilino, con un tope de
  sandboxes vivos y de sesiones de shell abiertas, y solo ve los suyos.
- **Varios hosts.** kindling es de un host. El reparto entre daemons vive aquí:
  se elige por hueco libre y se reintenta en otro cuando uno dice que no cabe
  o cuando su dorado quedó invalidado por un reinicio (ver más abajo).
- **Autocuración tras un reinicio del host.** Un reinicio invalida los
  snapshots dorados de kindling; el frontal lo detecta, reconstruye la
  plantilla en segundo plano con la receta que quedó anotada en el propio
  snapshot, y mientras tanto sirve desde cualquier otro host sano.
- **Métricas de Prometheus** (`GET /v1/metrics`) y catálogo de plantillas
  (`GET /v1/templates`), las dos detrás del mismo token que el resto de la API.

No es aislamiento entre inquilinos: comparten daemon y host. Es reparto y
contabilidad. Quien necesite aislamiento fuerte, hosts separados.

## Instalación

Primero kindling, y esta extensión encima, de la misma release:

```sh
kling plugin install sandbox         # kling-sandbox en tu máquina (o: install.sh --with sandbox)
kling plugin ls                      # debería salir sandbox con estado ok
```

El frontal corre como servicio en el host del daemon: la unidad viene en
`kindling-sandbox-host.tar.gz` de cada release o, desde un clon de kindling:

```sh
cd ext/sandbox
make install                          # compila e instala kling-sandbox desde fuentes
make deploy HOST=ssh://juan@lab       # el frontal, como servicio, en el host del daemon
```

Los objetivos de `make` y las rutas `cmd/…`, `deploy/…` de esta guía son
relativos a `ext/sandbox`.

## Uso

```sh
# Una plantilla: de qué imagen parte y qué lleva dentro.
cat > node.json <<'JSON'
{"name":"node","image":"toolchain","build_egress":"internet","mem_mib":1024,
 "steps":[{"cmd":["npm","install","-g","typescript"]}],"pool":1}
JSON
kling sbx template apply -f node.json

# Sandboxes desde esa plantilla: ~300 ms, o inmediato si hay uno precalentado.
kling sbx new -template node -ttl 30m
kling sbx ls
kling sbx exec <id> -- tsc --version
kling sbx shell <id>          # una terminal de verdad dentro
kling sbx cp ./a.ts <id>:/work/a.ts   # y kling sbx cp <id>:/work/out.json . de vuelta
kling sbx renew <id> -ttl 1h  # otro plazo de inactividad desde ahora
kling sbx rm <id>

kling sbx template rebuild node   # rehace el dorado con la receta que guardó (-host H: solo uno)

kling sbx hosts       # qué daemons hay detrás y cuánto les queda (token de admin)
```

En `kling sbx new`, `-ttl` es tiempo de **inactividad** y al vencer el sandbox se
congela (`-on-ttl freeze`, el defecto): el siguiente `exec` lo despierta en ms.
`-on-ttl remove` lo destruye. Es el defecto contrario al de `kling sandbox create`
del núcleo, que destruye salvo que se pida `-on-ttl freeze`.

Inquilinos, con sus cuotas, se dan de alta por entorno antes de arrancar el
gateway:

```sh
export KLING_SANDBOX_TENANTS="ana:tok1:10:5,bob:tok2:5"   # nombre:token[:max sandboxes[:max shells]]
export KLING_SANDBOX_ADMINS="ana"                         # quién ve los hosts y las métricas de todos
kling sbx gateway                                         # 127.0.0.1:8090, HTTP
kling sbx gateway -listen 0.0.0.0:8090 -tls-cert gw.pem -tls-key gw.key   # hacia la red, HTTPS
```

Los tokens van en cada petición, así que el gateway solo sirve HTTP en claro por
loopback (el defecto es `127.0.0.1:8090`). Para escuchar en otra interfaz necesita
certificado (`-tls-cert`/`-tls-key`, o `sandbox.tls_cert`/`sandbox.tls_key`) o
`-insecure-listen`, que acepta a sabiendas que los tokens viajen en claro (detrás de un
proxy TLS en la misma red de confianza, por ejemplo). Los clientes usan entonces
`kling config set sandbox.url https://gateway:8090`, con un certificado en el que confíen.

Y lo que ve un operador:

```sh
curl -H "Authorization: Bearer $TOK" https://gateway:8090/v1/metrics    # Prometheus (sin admin, solo las series propias)
curl -H "Authorization: Bearer $TOK" https://gateway:8090/v1/templates  # qué hay construido, y dónde
curl -H "Authorization: Bearer $ADMIN" https://gateway:8090/v1/hosts    # los daemons y sus endpoints (solo admin)
```

## Grafos precalentados

Un entorno entero —el sandbox del agente con su Postgres, por ejemplo— se precalienta
y se reclama como una máquina. La plantilla lleva `"kind": "graph"` y declara un
[grafo de kindling](../../docs/grafos.md) cuyos nodos nacen de plantillas o imágenes que
ya están en cada host:

```sh
cat > agente-pg.json <<'JSON'
{"kind":"graph","name":"agente-pg","pool":2,
 "nodes":{"agent":{"from":"sbx-node","allow_exec":true},
          "db":{"from":"pg16-golden","ports":[5432]}},
 "edges":[{"from":"agent","to":"db","kind":"link","port":5432}]}
JSON
kling sbx template apply -f agente-pg.json   # la guarda en el store de cada host
kling sbx template ls                        # INSTANCES = instancias libres

curl -H "Authorization: Bearer $TOK" -d '{"template":"agente-pg"}' https://gateway:8090/v1/graphs
# {"id":"h1/5f0c…","template":"agente-pg","state":"running",
#  "nodes":{"agent":{"id":"h1/9a3e…"},"db":{"id":"h1/77b1…","ports":[5432]}}}
curl -H "Authorization: Bearer $TOK" -d '{"cmd":["psql","-h","db.graph","-c","select 1"]}' \
  "https://gateway:8090/v1/sandboxes/h1/9a3e…/exec?wait=1"
curl -H "Authorization: Bearer $TOK" -X DELETE https://gateway:8090/v1/graphs/h1/5f0c…
```

- **Fondo.** El gateway levanta cada instancia con `POST /graphs` del daemon y la
  congela con `graph freeze`: en el fondo no cuesta RAM. Si no llega a congelarse se
  borra; nunca queda una a medias.
- **Reclamar** es etiquetar todas las máquinas del grafo con el inquilino, releerlas y
  despertar el grafo con `graph thaw`. Si otro gateway la reclamó a la vez, se queda
  con ella quien la tenga entera; una que quede mezclada se borra. Si no hay ninguna
  libre, se levanta una nueva ya a nombre del inquilino. Un grafo cuenta como un
  sandbox en la cuota (`MaxSandboxes`, y `MaxPorPlantilla` por su plantilla).
- **Entrega.** `GET /v1/graphs` y `GET /v1/graphs/{id}` solo enseñan los grafos
  enteros del inquilino (lo demás es el mismo 404). El id de cada nodo vale para
  `/v1/sandboxes/{id}/exec`, `/files` y `/shell`, no para ver, renovar ni borrar el
  nodo suelto: se suelta el grafo entero con `DELETE /v1/graphs/{id}`.
- **Limpieza.** Un grafo reclamado sin usarse más del plazo de abandono (24 h) se
  borra entero; uno roto (a medias o mezclado) se borra si sigue roto en dos vueltas
  seguidas. `kling sbx template rm <nombre>` quita la plantilla y sus instancias
  libres; las reclamadas siguen hasta que su inquilino las suelte.
- **Lo que no se admite en la plantilla:** nodos `lazy` (una instancia congelada tiene
  que estar entera), aristas `credential` (la clave viajaría con la plantilla y la
  verían todas las instancias), y volúmenes o carpetas del host en los nodos (serían los
  mismos en todas las instancias, un canal entre inquilinos; una arista `share` sí vale,
  su carpeta es de cada grafo). Las etiquetas `kind`, `tenant`, `template` y las
  `kling.*` y `sandbox.*` están reservadas.
- **La red es la de la plantilla**: la petición no la cambia. Declárala `none` salvo
  que el entorno necesite salir.
- **Pendiente:** `kling sbx` no tiene aún un comando cliente para grafos (se usa el API),
  y `/v1/metrics` no cuenta sus reclamaciones.

## Kubernetes

`kindling-operator` (`cmd/kindling-operator`) deja pedir sandboxes con
`kubectl apply` en vez de con el CLI: un CRD `Sandbox` declara qué se quiere,
y un operador fino y sin dependencias externas lo crea, lo mantiene y lo
borra hablando con este mismo frontal por HTTP. Las microVMs siguen sin vivir
dentro de ningún clúster; Kubernetes solo hace de plano de control. Ver
[`docs/kubernetes.md`](../../docs/kubernetes.md). La imagen es
`ghcr.io/juan52878911/kindling-operator:<versión de kindling>` y los manifiestos
salen también como `kindling-operator-deploy.tar.gz` en cada release.

## Compatibilidad

Todos los binarios de una release de kindling son compatibles entre sí: usa
`kling-sandbox` y `kindling-operator` de la misma versión que tu `kling`. La
tabla histórica, de cuando eran repos aparte, está en el
[CHANGELOG congelado](CHANGELOG.md).
