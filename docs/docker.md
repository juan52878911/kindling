# kindling y las imágenes de Docker: qué se puede hacer, y qué resuelve que Docker no

Una imagen de Docker en kindling es una microVM con su propio núcleo, sin
Docker en el host, que se congela y despierta en milisegundos y que, una vez
hecha plantilla, se multiplica por el coste de lo que cada copia cambia. Esta
página es el mapa: qué hay hoy, cómo se usa y un caso medido de punta a punta.
Lo que hay por debajo está en [imagenes.md](imagenes.md) (el constructor `oci`,
las medidas) y [cow.md](cow.md) (el almacén de copia al escribir).

## Lo que hay

| Quieres | Cómo |
|---|---|
| Correr una imagen tal cual | `kling run -image redis:7-alpine -mem 256M -wait-ready` (se importa la primera vez) |
| Con entorno (`-e` de Docker) | `kling run -image postgres:17-alpine -e POSTGRES_PASSWORD` (el valor, del entorno; o `-e K=V`, `-env-file F`): el entorno es de la máquina, llega por MMDS y no entra en la imagen; una imagen por referencia ([imagenes.md](imagenes.md#el-entorno-es-de-la-máquina)) |
| Importar con nombre, usuario, entrypoint o comando propios | `kling image import <ref> -name N [-user U] [-entrypoint E] [-- cmd...]` |
| Datos que sobrevivan a la máquina | `-volume nombre:/ruta` (un `VOLUME` de la imagen no crea nada solo) |
| Más disco para la propia máquina | `-disk 4G` (disperso: cuesta lo que se escribe) |
| Fijar la imagen | la etiqueta se resuelve una vez a digest y queda en la receta; `kling image recipe N` |
| Saber que está lista | el `HEALTHCHECK` de la imagen, o el primer puerto de `EXPOSE`; `-wait-ready`, `kling machine ready N` |
| Ver qué dice el servicio | `kling logs -service N` |
| Pararla como Docker | `kling stop` manda la `STOPSIGNAL` de la imagen antes de apagar; el disco de la máquina se queda |
| Arrancarla otra vez (`docker start`) | `kling start N`: en frío, sobre su disco, con la misma imagen, memoria, volúmenes y red. El entorno de `-e` no se guarda: hay que volver a darlo (`kling start -e POSTGRES_PASSWORD N`), y si falta una clave lo dice |
| Una plantilla caliente y copias | `kling save N plantilla` y `kling run -from plantilla`: copias listas en decenas de milisegundos con el estado de la plantilla |
| Dormir y despertar copias | `kling freeze`, `kling thaw` (o `-ttl 10m`): una copia dormida cuesta en disco solo lo que cambió |
| Salir a internet, o no | `-egress none` (por defecto), `allowlist -allow dominio`, `internet`; nunca a redes privadas |
| Secretos que no entren en la máquina | el proxy de credenciales (`kling machine credential`): el invitado ve un marcador |
| Una imagen tuya sin registro | aún no: `image import` baja de un registro (ver "Lo que falta") |

Lo que la imagen no decide: la CPU (un núcleo por vCPU, como un contenedor;
`-cpu-pct` manda), la memoria (`-mem`), la red (cerrada) y el disco propio.

## Lo que Docker no hace: copias que duermen

Un contenedor es un proceso. Si no corre, no existe: su memoria se pierde al
pararlo, y arrancarlo otra vez es arrancar el servicio desde cero. Si quieres
tenerlo "listo" tiene que estar corriendo, con toda su RAM, aunque nadie lo
use. Docker puede pausarlo (`docker pause`), pero pausado ocupa la misma RAM.

Una microVM de kindling tiene memoria propia, y esa memoria se puede volcar y
restaurar. De ahí salen tres cosas que no tienen equivalente en Docker:

1. **Una plantilla con el servicio ya caliente**, y copias de ella que nacen
   en milisegundos con todo lo que la plantilla tenía en memoria: conexiones
   abiertas, modelos cargados, cachés calientes.
2. **Copias que duermen**: congeladas no gastan RAM ni CPU, en disco cuestan
   lo que escribieron desde la plantilla, y despiertan en décimas de segundo
   donde se quedaron.
3. **Aislamiento de núcleo**, sin puertos en el host y sin salida a internet
   salvo que se pida, para un proceso que Docker correría en el núcleo del
   host con la red del host.

### Ejemplo medido: memoria de agentes por proyecto (Hindsight)

[Hindsight](https://github.com/vectorize-io/hindsight) es una memoria para
agentes de IA: una API en Python con un Postgres embebido y dos modelos
locales (embeddings y reranker). Se distribuye como imagen de Docker de 2,6
GiB y en reposo ocupa unos 900 MiB de RAM. Es la pieza que un agente consulta
en cada turno (`recall`) y en la que guarda lo que aprende (`retain`).

El problema: se quiere **una memoria por proyecto** (o por cliente, o por
agente), con aislamiento real entre ellas, y la mayoría de los proyectos no
están activos a la vez. Con Docker, veinte proyectos son veinte contenedores
corriendo (unos 18 GiB de RAM fijos) o arrancar cada uno cuando toca (18 s de
arranque en frío, sin la memoria del proyecto hasta que se cargue de un
volumen).

Con kindling (medido en el laboratorio, un i7 con 2 vCPU por máquina,
`/root` en ext4 con el almacén en Btrfs; la tabla completa está en
[imagenes.md](imagenes.md#imágenes-grandes-ram-cpu-y-disco)):

| | Docker | kindling |
|---|---|---|
| Imagen lista para usar | `docker pull` 50 s | `kling run -image ghcr.io/vectorize-io/hindsight:0.10.2` importa en 97 s, una vez |
| Servicio arrancado y listo | 17,6 s | 18 s en frío; **0,55 s** una copia de la plantilla, ya con sus memorias |
| Respuesta a una consulta | 0,33 s | 0,37 s |
| RAM por proyecto activo | 825–925 MiB | ~200 MiB propios + ~440 MiB compartidos entre todos los activos |
| Un proyecto inactivo | 900 MiB de RAM, o nada (parado) | **0 de RAM, 60–300 MiB de disco** (lo que escribió) |
| Volver a un proyecto inactivo | 17,6 s (arranque) | **1,1 s** (despierta donde estaba, con lo que aprendió) |
| Puertos en el host | 8888 y 9999, sin autenticación por defecto | ninguno; se entra por el gateway |

Veinte proyectos de los que tres están activos: Docker, 18 GiB de RAM o
arranques de 18 s; kindling, unos 2 GiB de RAM, 17 copias dormidas que suman
unos 2 GiB de disco, y 1,1 s para despertar cualquiera. La memoria de cada
proyecto es un diff sobre la plantilla, así que **hacer una plantilla nueva
(una versión nueva de Hindsight, un modelo distinto) no toca lo aprendido**:
se congela la copia, se cambia la base y se despierta.

Lo que hubo que hacer para llegar ahí, y que vale para cualquier imagen
grande, está en [imagenes.md](imagenes.md#imágenes-grandes-ram-cpu-y-disco):
un núcleo por vCPU, apretar el globo antes de volcar, el diferencial, el
espejo de la memoria en el almacén y `-disk`.

### Otros casos del mismo molde

- **Bases de datos por rama o por alumno** ([db.md](db.md)): un Postgres
  caliente por plantilla y una copia por rama de git o por alumno, en
  milisegundos; dormidas no cuestan nada.
- **Entornos de prueba efímeros**: una imagen de Docker de la aplicación,
  plantilla con los datos de prueba cargados, y una copia por ejecución de la
  batería que se tira al acabar.
- **Herramientas de agentes (MCP) que esperan**: un servidor que atiende una
  llamada cada diez minutos no necesita estar corriendo: duerme entre
  llamadas y despierta con la primera ([mcp](../ext/mcp)).

## Lo que falta

- **Importar una imagen local** (`docker save`, un directorio OCI): hoy
  `image import` solo baja de un registro.
- **`-p` de Docker**: no hay publicación de puertos en el host a propósito;
  se entra por el gateway o con `kling exec`.
- **Imágenes sin `sh`** (distroless): el init es un script y se rechazan.
- **`docker start` sin volver a dar el entorno**: `kling start` lo exige,
  porque el daemon solo guarda los nombres de las variables. Docker guarda los
  valores con el contenedor.
- **`docker restart`**: es `kling stop` y `kling start`.
- **Despertar aún más rápido con diffs grandes**: con cientos de MiB de
  diff, despertar cuesta ~40 µs por tramo de páginas (24 000 tramos en
  Hindsight, 1,1 s). Un servidor de páginas (UFFD) que sirviera base + diff
  bajo demanda lo dejaría en milisegundos.
