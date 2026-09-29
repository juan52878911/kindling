# kling db en CI: base desechable por PR

`kling db` en CI proporciona a cada PR su propia base de datos Postgres, creada
desde una plantilla golden y destruida al terminar. Sin servicios externos, sin
compartir estado entre PRs, sin contaminación. La base y el agente (test,
migración, aplicación) viven en la misma microVM.

## Modo de uso

### Fase 1: plantilla golden

Antes de correr CI, construir una plantilla golden una sola vez:

```sh
kling db golden -script ext/db/scripts/db-golden.sh build -seed-mb 20 pg
# Resultado: plantilla 'pg' (Postgres 16 caliente, schema inicializado)
```

La plantilla vive en el daemon y se reutiliza por todos los PRs. Ver
[db.md](db.md) para detalles.

### Fase 2: CI por PR

En el job de CI:

```bash
# El script ci-pr-db.sh:
#   1. Crea (o resetea si existe) una copia de la plantilla
#   2. Lee el DSN de kling db connect -dsn a una variable (traza apagada)
#   3. Exporta DATABASE_URL por entorno
#   4. Corre el comando pasado
#   5. Destruye la copia (trap)

ext/db/scripts/ci-pr-db.sh \
  -golden pg \
  -pr "$CI_MERGE_REQUEST_IID" \
  -- \
  pytest tests/ --tb=short
```

## Características

### Idempotencia

Si el job falla y se reintenta, `ci-pr-db.sh` resetea la copia (mismo nombre,
otra máquina, otra contraseña, el ttl que tenía la copia: `-ttl` solo cuenta al
crearla). No hay duplicados, no hay restos de intentos fallidos.

### Seguridad

- La contraseña se rota en cada copia (nueva contraseña, verificador SCRAM)
- Nunca en argv, nunca en logs, nunca en stdout: `ci-pr-db.sh` guarda la salida de
  `kling db connect -dsn` directamente en una variable, con `set +x` mientras tanto, y
  `ci-load.sh` descompone el DSN en `PGHOST`/`PGPASSWORD`/... sin procesos externos
  (nunca `psql "$dsn"`, que pondría la clave en argv)
- Solo va el verificador SCRAM al invitado (`ALTER ROLE app PASSWORD '<verificador>'`)
- La clave vive solo en `~/.local/state/kling-db/copies/<id>/password` (0600)
- DATABASE_URL se pasa solo por el entorno del comando; ningún fichero intermedio
- La trampa de salida borra con `kling db rm` (máquina **y** fichero de la clave) y
  con el mismo daemon: `-H` se exporta como `KLING_HOST`

### Paralelo seguro

Cada PR tiene su propio nombre (`pr-N`), su propia máquina (id único) y su propia
contraseña. 20 PRs en paralelo no interfieren: `kling db up` es atómico.

### Recursos

- Setup: ~1.5s por copia (boot del kernel + Postgres + rotate)
- TTL: default 2h (puede ajustarse con `-ttl`)
- Congelada por TTL: no consume CPU, los 100 MiB de RAM caben en swap

## Scripts

### ci-pr-db.sh

Script de entrada para CI. Uso:

```bash
ci-pr-db.sh -golden GOLDEN -pr N [-ttl D] [-keep] -- comando args...
```

Flags:
- `-golden G`: plantilla (requerida, puede ir en env GOLDEN)
- `-pr N`: número de PR (requerida, puede ir en env PR)
- `-ttl D`: TTL de la copia (defecto `2h`, puede ir en env TTL)
- `-H HOST`: endpoint del daemon (puede ir en env KLING_HOST)
- `-keep`: no borra la copia (debugging, puede ir en env KEEP=1)

Env:
- `DATABASE_URL`: se exporta al comando con el DSN
- `KLING`: binario (defecto `kling`)
- Todos los flags pueden ir en env, de minúsculas: `GOLDEN`, `PR`, `TTL`, etc.

Ejemplos:

```bash
# GitHub Actions
GOLDEN=pg-golden ci-pr-db.sh -pr "${{ github.event.number }}" -- pytest tests/

# GitLab CI
ci-pr-db.sh -golden pg-golden -pr "$CI_MERGE_REQUEST_IID" -- pytest tests/

# Debugging (mantiene la copia)
KEEP=1 GOLDEN=pg-golden ci-pr-db.sh -pr 42 -- bash
# Luego: kling db connect pr-42 -psql
```

### ci-load.sh

Script de prueba de carga: lanza P simulaciones de PR en paralelo.

```bash
ci-load.sh [-golden G] [-p N] [-keep] [-out DIR]
```

Flags:
- `-golden G`: plantilla (requerida, puede ir en env GOLDEN)
- `-p N`: número de simulaciones (defecto 20, puede ir en env P)
- `-keep`: no borra las copias (debugging, puede ir en env KEEP=1)
- `-out DIR`: directorio de salida (defecto `./ci-load-YYYYMMDD-HHMMSS`)
- `-H HOST`: endpoint del daemon (se exporta como `KLING_HOST`)

Necesita `psql` en el host. Cada simulación lee su DSN con `kling db connect -dsn` y lo
descompone en `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD` y `PGDATABASE` para su `psql`;
la clave no aparece en ninguna línea de órdenes.

Cada simulación:
1. Crea una copia `cil_<idx>` de la plantilla
2. Corre migración (CREATE TABLE)
3. 50 iteraciones de INSERT + SELECT
4. Borra la copia
5. Devuelve tiempo de up, tiempo total, éxito/fallo

Genera informes:
- `ci-load.json`: métricas estructuradas
- `ci-load.md`: resumen markdown
- `results.txt`: línea por simulación (status, nombre, tiempos)

Ejemplo:

```bash
GOLDEN=pg-golden P=20 ./ci-load.sh
# Genera ./ci-load-20260928-120000/ci-load.md con p50, p95, etc.
```

Reglas de honestidad (docs/thaw-at-scale.md):
- Toda ronda al informe (sin seleccionar números bonitos)
- DEGRADED si overhead > 1% en alguna métrica
- Celdas que no caben en la tabla = skipped
- Nada de repetir hasta que salga bonito

## Ejemplos de CI

### GitHub Actions

Ver `ext/db/examples/github-actions.yml`. Requiere:
- Runner self-hosted con acceso al daemon de kindling
- Plantilla golden preexistente

```yaml
- name: Run tests with database
  run: |
    ext/db/scripts/ci-pr-db.sh \
      -golden "${{ env.GOLDEN }}" \
      -pr "${{ github.event.number }}" \
      -- pytest tests/
```

### GitLab CI

Ver `ext/db/examples/gitlab-ci.yml`. Requiere:
- Runner self-hosted con acceso al daemon de kindling
- Plantilla golden preexistente

```yaml
test:
  extends: .with_db
  script:
    - ci-pr-db.sh -golden "$GOLDEN" -pr "$CI_MERGE_REQUEST_IID" -- pytest tests/
```

## Modelo de seguridad en CI

### Linux

En Linux, el namespace de red de cada máquina DNATea todos los puertos, así que
**cualquier proceso del host llega al 5432 de la copia** (`IP:5432`). La
protección es:

1. Cada copia tiene su propia contraseña (rotada, nueva en cada nueva copia)
2. La contraseña **solo está en el host** (fichero 0600 por id de máquina)
3. Postgres rechaza `app` sin contraseña, acepta al superusuario solo por socket local
4. El agente (tests, migración) accede como el rol `app` con la contraseña

Así, otro usuario del host que llegue al puerto 5432 de `pr-42` no puede entrar
sin su contraseña, que no está en ningún lado excepto el fichero 0600 de su id.

### macOS

En macOS, el backend abre un reenvío en loopback (`localhost:29000-29999`) para
`kling.ports=5432`, con comprobación de `peercred` (credenciales del proceso del
otro extremo). Postgres ve llegar desde `172.16.0.1`.

La frontera es más fuerte:
1. Solo el usuario que lanzó el daemon puede hablar al reenvío (peercred)
2. La clave sigue siendo única por copia
3. Nadie desde Internet llega al reenvío

## Límites de este MVP

- No hay TLS entre host y copia (el camino es veth o loopback, propio del equipo)
- El agente de la microVM ve la base entera (comparten máquina)
- En Linux, la clave es la única barrera de red; en macOS se suma peercred del reenvío

## Debugging

### Una copia quedó en `preparing`

```bash
# Bórrala
kling db rm pr-42

# kling db rm también vale sin contraseña en el host (reset fallido): borra la
# máquina y lo que quedara de su fichero.
```

### Quiero reutilizar una copia para debugging

```bash
KEEP=1 GOLDEN=pg-golden ci-pr-db.sh -pr 42 -- pytest tests/
# No borra pr-42. Conéctate:
kling db connect pr-42 -psql
# O con el DSN:
kling db connect pr-42 -dsn | pbcopy
```

### Quiero ver el log de ci-pr-db.sh

Todos los log van a stderr (lineas con timestamp); stdout es el del comando.
DATABASE_URL no se imprime nunca.
En CI, stderr está disponible en la salida del job.

## Línea de tiempo de un PR

```
job start
  ↓
ci-pr-db.sh checks if pr-N exists
  ↓
  no: creates pr-N from golden (1-2s)
  yes: resets pr-N (destroy + create)
  ↓
(inside up/reset) waits for postgres, drops inherited kling db role roles,
rotates the password (ALTER ROLE app PASSWORD '<verifier>')
  ↓
kling db connect pr-N (ready probe), then -dsn into DATABASE_URL
  ↓
exports DATABASE_URL by environment
  ↓
runs the test command
  ↓
trap: destroys pr-N
  ↓
job end
```

TTL (default 2h): si el job tarda más, la copia se congela automáticamente.
Si el job se cuelga, el TTL la destruye en 2h.

## Performance

### En lab Linux (CT 105, Proxmox, vz backend)

- `kling db up` desde golden: 0.8-1.2s (veth + boot de kernel + Postgres listen)
- Rotate: 0.2s (ALTER ROLE + verificación)
- Total setup por PR: ~1.5s
- 20 PRs en paralelo: ~2s (los primeros) + ~200ms por extra

### En macOS (M4, vz)

- Similar a Linux (~1.5s setup)
- Reenvío de loopback overhead: < 10ms por consulta

## Próximos pasos

Fase 4 planeada (fuera de scope en v0.12):
- Reclonación vía COW (copy-on-write) de la plantilla en el mismo VM
- TLS entre host y copia
- Proxy de credenciales en el host (agente fuera de la copia)
- Métricas de CPU/RAM por copia (kling exec con cgroupv2)

## Ver también

- [db.md](db.md): `kling db` (subcomandos, etiquetas, credenciales, modelo de seguridad)
- [db-golden.md](db-golden.md): construcción de la plantilla
- [thaw-at-scale.md](thaw-at-scale.md): reglas de honestidad de benchmarks
- [exec-sandbox.md](exec-sandbox.md): `kling exec` (cómo corre código dentro de una copia)
