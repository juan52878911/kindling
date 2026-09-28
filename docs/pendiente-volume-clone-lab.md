# Pendiente: validar `kling volume clone` en el lab

> Documento de trabajo de la rama `claude/funny-cannon-mgxcbj`. Es el prompt para el
> agente local que tiene acceso al host del lab (KVM). **Bórralo antes de fusionar la
> rama**: los resultados que valgan la pena van a `docs/benchmarks.md`.

---

Eres un agente trabajando en el repo **kindling** (`juan52878911/kindling`), en local, con
acceso al host del lab por SSH (el mismo que usa `make deploy HOST=ssh://…`). Trabaja **solo
en la rama `claude/funny-cannon-mgxcbj`**: no abras PR, no fusiones, no empujes a `main`.
Todo documento que generes va en esa rama.

## Contexto

La rama añade `kling volume clone <origen> <destino> [-copy]` (PR-A del plan del plugin
`db`, ramificación de bases de datos):

- Clona con `FICLONE` (reflink) en Linux y `clonefile` en macOS: el destino comparte
  bloques con el origen hasta que uno escribe. Sin reflink (ext4) el daemon da **409**;
  `-copy` hace copia completa dispersa. Nunca copia sin pedirlo.
- Rechaza clonar un volumen con un escritor vivo; durante el clon, el origen queda
  reservado (no se puede montar RW ni borrar).
- `GET /info` → `clone`: sonda real por directorio (`volumes/`, `machines/`,
  `snapshots/`, `jails/`). `kling status -v` la resume en la línea `clone:`.
- Código: `internal/machine/clon*.go`, `internal/daemon/volumes.go`,
  `cmd/kling/volume.go`, `pkg/api/types.go`. API en `docs/api.md` (§Volúmenes).
- Ya verificado: tests unitarios con `-race`; CI verde con clon REAL sobre XFS y btrfs en
  loop y APFS (jobs `clon real (xfs|btrfs|apfs)`, run 36364292569).
- `pkg/transport` `TestSSHMultiplexArgsSinDirectorioCaeAlModoDeSiempre` falla al correr
  como root, y ya fallaba antes de esta rama: no es tuyo, no lo "arregles" aquí.

**Lo que falta** es lo que solo se puede hacer en el host real: el daemon desplegado,
volúmenes de verdad con datos, medidas y la prueba e2e.

## Reglas

- **No toques el sistema de ficheros de `/var/lib/kindling` del lab** (no reformatear,
  no montar nada encima, no mover directorios) sin preguntarme antes. Si el root está en
  ext4, mídelo tal cual y usa un XFS en loop **aparte** solo para el micro-benchmark del
  paso 4.
- **No arranques un segundo daemon** sobre otro root en el mismo host: la reconciliación
  al arrancar limpia namespaces y cgroups que cree huérfanos, y podría llevarse los del
  daemon real.
- Nombres de todo lo que crees con prefijo `clon-e2e-`, y bórralo al terminar (salvo
  `KEEP=1`).
- Antes de cada push: `gofmt -l .` vacío, `go vet ./...` y los tests de los paquetes
  tocados con `-race`. Nada de commits vacíos ni de saltarse tests.
- Si algo falla y la causa está en el código de la rama, arréglalo con un test que lo
  reproduzca y explícalo en el commit. Si no está claro, **para y pregúntame**.

## Pasos

1. **Preparar.** `git fetch origin claude/funny-cannon-mgxcbj && git checkout
   claude/funny-cannon-mgxcbj && git pull`. Ejecuta `make test`; anota el resultado
   (espera solo el fallo conocido de `pkg/transport` si corres como root).

2. **Desplegar** en el lab: `make deploy HOST=ssh://<usuario>@<host>` (con `GOARCH=arm64`
   si el host es arm64) y reinicia el servicio como indica el Makefile. Comprueba:
   - `kling -H ssh://… status -v` → la línea `capabilities` incluye `volume-clone`, y la
     línea `clone:` existe. Copia la línea tal cual.
   - En el host: `findmnt -T /var/lib/kindling` y, si es XFS, `xfs_info` (busca
     `reflink=1`). Anota sistema de ficheros, disco y si `volumes/`, `machines/` y
     `snapshots/` están en el mismo montaje.
   - Si la sonda dice algo que no casa con `findmnt` (p. ej. XFS con `reflink=0` y la
     sonda en ✓), es un bug: repórtalo con la salida exacta.

3. **Prueba funcional con datos reales** (vía CLI contra el lab):
   1. `kling volume create clon-e2e-src -size 8G`.
   2. Llénalo con ~4 GiB desde una microVM:
      `kling volume populate clon-e2e-src -- sh -c 'dd if=/dev/urandom of=/data/blob bs=1M count=4096 && sync && sha256sum /data/blob > /data/blob.sha'`
      (si `populate` no admite ese comando o tamaño, usa `kling run -allow-exec -volume
      clon-e2e-src` + `kling exec`, y para la máquina al terminar).
   3. **Clon:** `time kling volume clone clon-e2e-src clon-e2e-c1`. Anota método, ms y la
      diferencia de `df -B1 /var/lib/kindling` antes/después en el host.
      - Con reflink: debe ser `reflink`, < 1 s, y `df` casi sin cambio.
      - En ext4: debe dar 409 con la pista de `-copy`. Entonces
        `time kling volume clone clon-e2e-src clon-e2e-c1 -copy` y anota tiempo y `df`.
   4. **Integridad:** arranca una microVM con `clon-e2e-c1` en solo lectura y verifica
      `sha256sum -c /data/blob.sha` dentro. Debe pasar.
   5. **Independencia:** con `clon-e2e-c1` en escritura, sobrescribe 100 MiB de
      `/data/blob`; comprueba que en `clon-e2e-src` el sha sigue cuadrando. Con reflink,
      `df` debe crecer ~100 MiB (solo lo que divergió).
   6. **Rechazos:** con una microVM que tenga `clon-e2e-src` montado en **escritura**,
      `kling volume clone clon-e2e-src clon-e2e-c2` debe fallar nombrando la máquina.
      Con el volumen montado en **solo lectura** debe funcionar. Clonar sobre un nombre
      que existe debe fallar. `kling volume rm clon-e2e-src` mientras hay un clon `-copy`
      en curso debe fallar (lanza el clon en segundo plano y borra a la vez).
   7. `kling volume ls` al final: los clones aparecen como volúmenes normales. Anota que
      `ON DISK` cuenta los bloques compartidos dos veces (es lo esperado y documentado).

4. **Micro-benchmark del clon** (independiente del daemon), para la tabla:
   - Si el root es XFS/btrfs: en un directorio de prueba bajo el mismo montaje.
   - Si es ext4: crea un XFS en loop aparte (`truncate -s 20G /tmp/clon.img;
     mkfs.xfs -m reflink=1; mount -o loop` en `/mnt/clon-bench`) y desmóntalo al acabar.
   - Mide clonar ficheros de 1, 10 y 50 GiB **con datos** (no dispersos): `time cp
     --reflink=always` frente a `time cp --sparse=always`, 3 repeticiones, mediana.

5. **E2E en el repo.** Añade a `scripts/90-e2e.sh` un bloque "volume clone" con sus
   convenciones (`step`, `ok`, `bad`, `contiene`, limpieza en el trap, respeta `KEEP=1`),
   con volúmenes pequeños: clonar, comprobar contenido desde una microVM, rechazo con
   escritor, destino existente. Si el host no clona, el bloque comprueba el 409 y el
   camino `-copy` en su lugar (no lo marques como fallo). Ejecuta el e2e completo contra
   el lab y pega el resumen.

6. **Documentar en la rama:**
   - `docs/benchmarks.md`: sección nueva "Volume clone" con hardware, fecha, sistema de
     ficheros, la tabla del paso 4 y los números del paso 3 (método, ms, delta de disco
     del clon y tras escribir 100 MiB). Solo números medidos; lo que no midas, dilo.
   - `README.md` y `README.es.md` (§"Cloning a volume" / "Clonar un volumen"): añade UNA
     cifra medida (p. ej. "4 GiB clonados en N ms en XFS"), igual en los dos.
   - `CHANGELOG.md` (§"Sin publicar"): una línea con la cifra medida.
   - Commit(s) en español, estilo del repo, y `git push -u origin
     claude/funny-cannon-mgxcbj`. No abras PR.

7. **Limpieza:** borra todo lo `clon-e2e-*` (volúmenes y máquinas) y el loop del paso 4.
   Deja el daemon desplegado con esta rama salvo que te diga lo contrario.

## Informe final

Devuélveme, en español y breve:

- Línea `clone:` de `status -v`, y `findmnt` de `/var/lib/kindling`.
- Tabla: operación → tiempo → delta de disco (clon, clon `-copy` si aplica, escritura de
  100 MiB en el clon) y la del micro-benchmark.
- Resultado de cada comprobación del paso 3 (ok/fallo, con la salida si falló).
- Resumen del e2e (`N ok, M fallos`).
- Commits empujados (hash y título) y cualquier bug encontrado/arreglado.
- Qué no pudiste hacer y por qué.
