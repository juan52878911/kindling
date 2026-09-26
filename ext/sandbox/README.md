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
kling plugins install sandbox         # kling-sandbox en tu máquina (o: install.sh --with sandbox)
kling plugins ls                      # debería salir sandbox con estado ok
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
kling sbx rm <id>

kling sbx hosts       # qué daemons hay detrás y cuánto les queda
```

Inquilinos, con sus cuotas, se dan de alta por entorno antes de arrancar el
gateway:

```sh
export KLING_SANDBOX_TENANTS="ana:tok1:10:5,bob:tok2:5"   # nombre:token[:max sandboxes[:max shells]]
kling sbx gateway
```

Y lo que ve un operador, con cualquier token:

```sh
curl -H "Authorization: Bearer $TOK" http://gateway:8090/v1/metrics    # Prometheus
curl -H "Authorization: Bearer $TOK" http://gateway:8090/v1/templates  # qué hay construido, y dónde
```

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
