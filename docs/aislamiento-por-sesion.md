# Aislamiento por sesión: una microVM y un disco por sesión MCP

Un comentario en r/mcp preguntaba si la siguiente llamada o sesión ve los residuos de la
anterior. En los servicios efímeros no: la microVM muere tras cada acción. En los
persistentes la respuesta era **sí, para el disco**. `kling-bridge` lanza un proceso por
sesión y la memoria de cada proceso es suya, pero todas las sesiones de una instancia
escriben en el mismo overlay. Lo que una sesión deja en `/tmp` o en el directorio de datos
del servidor lo lee la siguiente. Medido en el lab con `filesystem-mcp`: la sesión A escribe
`/tmp/cow-….txt` y cierra, la sesión B abre y lo lee (paso 1 de
`ext/mcp/scripts/91-e2e-aislamiento.sh`).

Desde esta versión, un servicio puede pedir **una microVM por sesión**:

```sh
kling mcp import notas -image notas -isolation session   # al importarlo
kling mcp isolation notas session                        # o después, sin reimportar
kling mcp isolation notas                                # ver cuál tiene
```

El valor por defecto sigue siendo `service`, el comportamiento de siempre.

## Cómo funcionaba (el análisis previo)

**Disco.** Una máquina arranca con dos discos: la imagen base (o base + capa de servicio)
en solo lectura, compartida por todas, y un `overlay.ext4` propio que `overlay-init` monta
como upperdir dentro del invitado. Una instancia de un snapshot dorado (`runFrom`) copia el
`overlay.ext4` del dorado con `cp --sparse=always`, restaura el `snap.file` y **mapea**
el `mem.file` en vez de copiarlo, y apunta el overlay a su copia con `PatchDrive` antes de
reanudarla. Como el `mem.file` está mapeado y es el mismo para todas, el kernel comparte
esas páginas: 10 instancias del mismo dorado suman +68 MiB de RAM, frente a +824 MiB
arrancándolas en frío ([guía](guia.md), "Densidad"). Cada una solo paga lo que escribe. `kling save`
es ese mismo Commit con un "hijo caliente" dentro (`/reset?warm=1`), para que el primer
`initialize` tras restaurar no pague el arranque de node.

**El conjunto de discos se fija al congelar.** Firecracker no añade ni quita discos de una
VM restaurada; solo deja reapuntar los que tiene. Cualquier diseño que necesite un disco
más por sesión dentro de una misma VM choca con esto.

**Sesiones.** El puente guarda un proceso hijo por `Mcp-Session-Id` (tope derivado de la
memoria: con 256 MiB cabe **una** sesión, y a partir de 320 MiB, 64 MiB por sesión). El
gateway acuña el id que ve el cliente y guarda el del invitado aparte. El enrutado es
pegajoso: `routes[id] → machineID`. Si todas las instancias están llenas crea réplicas
(`scaleOut`) del dorado, con tope `-max-replicas`. Una sesión termina con `DELETE`, y si
no, el segador borraba su ruta cuando llevaba `idle` sin uso, **la misma vuelta en que
congelaba la instancia**. En la práctica, una sesión no sobrevivía al congelado por
inactividad: la memoria sí se conservaba, pero el gateway ya había olvidado adónde iba.
El TTL de la máquina en el daemon es un arrendamiento que el segador renueva mientras la
instancia está despierta.

## Opciones

| | (a) una microVM por sesión | (b) overlayfs por sesión dentro del invitado | (c) un disco por sesión en la misma VM |
|---|---|---|---|
| Frontera | la de la VM | mount namespace en el mismo kernel | la de la VM, pero una VM compartida |
| Cambios en el invitado | ninguno | el puente (pivot_root por hijo) y reconstruir las bases | ninguno |
| Servidores de HTTP nativo | aislados | no: un solo proceso para todas | no |
| RAM por sesión | ~10 MiB (páginas del dorado compartidas) | lo que escriba en tmpfs | poca |
| Primer `initialize` | restore + hijo caliente del dorado | un proceso nuevo | un proceso nuevo |
| Volúmenes rw | un escritor a la vez: incompatibles, se rechaza | se comparten | se comparten |
| macOS (vz) | funciona igual, más caro por VM | igual | no |
| "Discos fijos al congelar" | no le afecta | no le afecta | **le impide**: habría que reimportar para cada sesión |

(c) muere por la restricción de discos. (b) es la más barata en RAM, pero su frontera no es
la del proyecto: dos procesos en el mismo kernel invitado se ven por `/proc/<pid>/root`
aunque cada uno tenga su mount namespace, y un servidor MCP es código de terceros. Además
obliga a tocar el puente y a reconstruir todas las bases, y no aísla nada en los servidores
que hablan HTTP nativo, donde un solo proceso atiende a todas las sesiones.

**Elegida: (a).** El dorado ya da lo que hace falta: un overlay propio por máquina, páginas
de memoria compartidas y un hijo caliente. El planificador ya sabe congelar, descongelar y
enrutar a una máquina concreta. El coste es el de una instancia más por sesión, y el
dorado lo deja en ~10 MiB.

## Cómo funciona

- `pkg/scheduler/aislada.go`. Una sesión aislada nace en una máquina restaurada del dorado
  con la etiqueta `isolated=true`. `acquire` nunca adopta ni descongela una máquina con esa
  etiqueta para otra cosa, y `entriesLocked` no la ofrece a sesiones nuevas.
- Vive en `g.extra` como una réplica más, así que el segador la congela por inactividad,
  `evictLRU` puede congelarla para hacer sitio (también por otras sesiones del mismo
  servicio), el latido del TTL la renueva y cuenta para la cuota del tenant.
- **Su ruta no caduca con `idle`**, sino con `-session-ttl` (30 min por defecto) sin uso.
  Congelada, la sesión sigue viva. La siguiente petición descongela *su* máquina, no otra.
  Si la máquina ya no existe (el recolector del daemon, un `kling rm`), la sesión se pierde
  con un 404 y el cliente rehace el `initialize`. Darle otra máquina sería darle un disco
  vacío como si fuera el suyo.
- **Final.** El `DELETE` destruye la máquina antes de contestar. La caducidad y el apagado
  del gateway también la destruyen. Un barrido cada 5 minutos, que siempre corre en la
  primera vuelta, recoge las huérfanas de un gateway anterior.
- **Tope.** Cada sesión es una máquina, así que el tope es `-max-replicas` (16 por
  defecto), contando despiertas y congeladas. En el tope se recicla la sesión más ociosa si
  lleva más de 45 s sin uso, como hace el puente con las suyas. Si no, 503.
- **El agregador `_all`** da una microVM del servicio a cada conversación, y la captura del
  catálogo de un servicio sin `mcp.tools` usa una sesión aislada de usar y tirar. Ninguno
  de los dos despierta nunca una instancia compartida de un servicio aislado.
- **La anotación.** `mcp.isolation` es una anotación del snapshot y no una etiqueta, para
  poder cambiarla sin reimportar. El gateway la relee cada 15 s y se aplica a las sesiones
  nuevas. `kling mcp isolation` solo la acepta en servicios MCP. Si el gateway no ha podido
  leer nunca el modo (el daemon no contesta al arrancar), **falla cerrado**: contesta 502
  en vez de suponer el modo compartido, que volvería a mezclar los discos sin avisar.
- **El agregador no cambia de máquina en silencio.** Si la máquina de una conversación
  desaparece (caducó o la borró el recolector), la siguiente llamada de esa conversación
  devuelve un error que lo dice, igual que el 404 del camino directo.

## Lo medido

CT 105 del lab (i7-8700T, Linux, Firecracker con jailer), servicio
`io.github.domdomegg/filesystem-mcp` sobre la base `node`, 256 MiB, gateway con
`-idle 30s -session-ttl 2m`. `91-e2e-aislamiento.sh`: 25 ok, 0 fallos (con el agregador
`_all`, el congelado del segador y la caducidad).

| | |
|---|---|
| RAM por sesión despierta (caída de MemAvailable / 5 sesiones) | **9,7-10,0 MiB** |
| Disco por sesión en marcha (`du` de `machines/<id>`) | **0,3 MiB** |
| Disco por sesión congelada (el volcado de memoria) | 135,8 MiB |
| Primer `initialize`, modo `service`, instancia despierta (mediana de 5) | 765-768 ms |
| Primer `initialize`, modo `session`, microVM nueva (mediana de 6) | **69-70 ms** |

La sesión nueva en su propia microVM es **11 veces más rápida** que una sesión nueva en una
instancia ya despierta. Esa instancia ya gastó su hijo caliente en la primera sesión, así
que cada sesión siguiente paga el arranque de node en frío dentro de ella (~700 ms en esta
CPU). La microVM nueva adopta el hijo caliente del dorado.

Lo que cuesta es el disco de las sesiones **congeladas**: un volcado de memoria por sesión
viva, que dura hasta que se cierra o caduca. Con 16 sesiones abandonadas de este servicio
son ~2,2 GB hasta que venza `-session-ttl`. Si pesa, se baja el TTL o `-max-replicas`.

## Lo que no cubre

- **Un servicio con un volumen rw no puede aislar sus sesiones.** Un volumen tiene un
  solo escritor (`internal/machine/volume.go`), también si su máquina está congelada, y
  cada sesión es otra máquina. La segunda sesión no arrancaría mientras existiera la
  primera. `kling mcp isolation … session` y `import -isolation session` se niegan y lo
  explican. Los volúmenes ro (una biblioteca de paquetes) sí se comparten. Si lo que se
  quiere compartir es un almacén, el camino es un servicio de memoria enlazado, que vive
  fuera de las microVMs.
- **El puente cierra sesiones ociosas a los 10 min** (`-session-idle`), pero en tiempo
  del invitado *en marcha*. Medido: una sesión aislada congelada 11,5 min vuelve con su
  fichero, porque el reloj monótono del invitado no avanza mientras está congelado. Lo que
  sí la cerraría es un `-idle` del gateway de más de 10 min: la máquina seguiría despierta
  y ociosa el tiempo suficiente para que el puente cerrase la sesión por dentro. Con el
  valor por defecto (5 min) no pasa.
- **macOS (vz)**: el código es el mismo (planificador y gateway), pero no está probado allí.
  Allí cada restauración cuesta ~350 MiB (`docs/vz-mac-prototipo.md`) y no comparte páginas
  como en Linux, así que la densidad por sesión es mucho peor.
- **Las sesiones no sobreviven a un reinicio del gateway**, igual que antes: las rutas viven
  en su memoria. Sus máquinas se destruyen al apagarlo o, si murió, en el primer barrido.
- **Dos gateways sobre el mismo daemon sin `MachineLabels` distintas** se barrerían las
  máquinas aisladas el uno al otro pasados 2 min. `kling mcp serve` corre uno por daemon.
