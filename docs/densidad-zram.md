# Densidad con swap comprimido en RAM (zram) — opción operativa del host

**Esto es opt-in y vive fuera de kindling.** No cambia ni una línea de Go: es
configuración del HOST donde corren las microVMs. Kindling arranca y funciona
exactamente igual con esto activado o sin él. Entrega:

- `packaging/zram-kindling.conf` — parámetros, comentados.
- `packaging/zram-kindling.sh` — script que monta/desmonta el zram (sysfs puro).
- `packaging/zram-kindling.service` — unidad `oneshot` que lo activa al arrancar.

## Qué es y por qué puede ayudar

Kindling levanta muchas instancias de un mismo **snapshot dorado**. Por
copy-on-write comparten las páginas que no tocan; las páginas de page cache
compartidas son justo lo que da la densidad medida (ver
[hallazgos.md](hallazgos.md), "Medida de la compartición de páginas": 10
máquinas costaron +68 MiB reales, no 258).

Lo que **no** se comparte es lo que cada invitado modifica tras despertar: sobre
todo el **heap de node**, que es anónimo y muy repetitivo. Esas páginas
divergen copia a copia y engordan la RAM real. Pero comprimen de sobra —3x-4x es
habitual en heaps de JS—, y ahí entra zram: mueve esas páginas anónimas frías a
un dispositivo de swap **que vive en RAM y guarda comprimido**, en vez de
empujarlas al NVMe. Ganas sitio para más microVMs co-residentes sin pagar la
latencia de un swap a disco.

Encaja con lo que ya sabemos del laboratorio: bajar el `mem_mib` configurado es
la palanca de densidad (ver MEMORY: "densidad: mem_mib es el límite"), y con el
`mem_mib` apretado el kernel del host tiene más presión de memoria — justo el
escenario donde un swap comprimido rinde en vez de estorbar.

## Cuándo ayuda y cuándo no

**Ayuda cuando:**

- Hay **muchas copias divergentes** del mismo snapshot y su parte anónima
  (heaps de node) domina el crecimiento de RAM.
- El host está limitado por RAM, no por CPU: sobran ciclos para comprimir.
- Quieres subir densidad sin tocar disco ni cambiar `mem_mib` aún más.

**No ayuda (o estorba) cuando:**

- Las páginas que divergen comprimen mal (binario ya comprimido, cifrado): pagas
  CPU y latencia sin recuperar RAM.
- El host está limitado por CPU: el compresor le quita ciclos a los invitados.
- La carga es sensible al p99 y el trabajo diverge poco: metes latencia de
  descompresión a cambio de un ahorro pequeño.

Regla honesta: **es una optimización opcional, no un cambio de arquitectura.**
Si al medir no ves densidad extra clara, quítala.

## zram vs zswap

Dos formas de comprimir en RAM; elige una, no las dos.

| | zram | zswap |
|---|---|---|
| Qué es | Un **disco RAM comprimido** dedicado, usado como swap | Una **caché comprimida delante del swap real** en disco |
| Necesita disco de swap | No | Sí (es su respaldo cuando la caché se llena) |
| Techo | La RAM que le dejes (`mem_limit`) | Ilimitado: rebosa al swap de disco |
| Se activa con | Módulo + sysfs (esta unidad) | Parámetro de kernel |
| Cuándo | No quieres tocar disco; techo acotado y predecible | Quieres que el exceso caiga a disco sin frenar |

**Recomendación para kindling: zram.** El objetivo es densidad en RAM sin
latencia de disco, y aquí no hay (ni queremos) un swap en NVMe en el camino
crítico. zram da un techo claro y predecible con `mem_limit`. zswap tiene sentido
si prefieres una válvula de escape a disco para cargas que rebasan la RAM, pero
eso reintroduce la latencia de disco que zram evita.

**Cómo se activaría zswap** (mención, por si se prefiere; NO lo activa esta
entrega): por línea de comandos del kernel en el bootloader —

```
zswap.enabled=1 zswap.compressor=zstd zswap.max_pool_percent=25 zswap.zpool=zsmalloc
```

y hace falta un swap en disco de respaldo ya configurado. Es una decisión de
arranque del host, no un servicio.

## Cómo activarlo (opt-in, en un host de verdad NO aquí)

> No lo actives en tu máquina de desarrollo. Esto es para el host del laboratorio.

```bash
# 1. Instala la config, el script y la unidad
sudo install -m600 packaging/zram-kindling.conf   /etc/kling/zram-kindling.conf
sudo install -m755 packaging/zram-kindling.sh     /usr/local/bin/zram-kindling.sh
sudo install -m644 packaging/zram-kindling.service /etc/systemd/system/

# 2. (Opcional) ajusta parámetros
sudo $EDITOR /etc/kling/zram-kindling.conf

# 3. Habilítalo. Es lo único que lo enciende: nada más lo arrastra.
sudo systemctl daemon-reload
sudo systemctl enable --now zram-kindling.service

# 4. Comprueba
zramctl                 # o: cat /sys/block/zram*/mm_stat
swapon --show           # el zram debe salir con prioridad alta
```

Para quitarlo: `sudo systemctl disable --now zram-kindling.service` (el ExecStop
hace swapoff y libera el dispositivo).

Parámetros en `zram-kindling.conf`: algoritmo (`zstd` por defecto; `lz4` si el
p99 sufre), tamaño lógico como % de RAM, tope físico del pool y prioridad de
swap. Cada uno explicado en el fichero.

## El riesgo: hay que MEDIRLO, no asumirlo

**La descompresión añade latencia.** Cuando el kernel necesita una página que
mandó a zram, la descomprime en el camino crítico. Eso puede aparecer como cola
en el **p99 del descongelado (thaw)** de una microVM que llevaba rato fría —
justo cuando un agente la vuelve a llamar. Y el compresor consume CPU que quizá
haga falta para los invitados. Ninguna de las dos cosas se da por buena: se mide.

### Medir densidad, antes y después

Compara el mismo experimento con el servicio parado y con él activo:

```bash
# Línea base (zram apagado)
sudo systemctl stop zram-kindling.service
# ... levanta N instancias del MISMO snapshot dorado ...
free -m                 # crecimiento real de RAM del sistema (NO el RSS sumado)
kling top               # PSS por microVM y PSS total — la cifra que suma de verdad

# Con zram
sudo systemctl start zram-kindling.service
# ... repite: mismo snapshot, misma forma de subir instancias ...
free -m
kling top
zramctl                 # orig_data_size vs compr_data_size = ratio real conseguido
```

La pregunta concreta a responder: **¿cuántas microVMs de ese snapshot caben
antes de que el host empiece a sufrir, con y sin zram?** El delta es la ganancia.
Ojo con dos trampas ya documentadas en hallazgos.md:

- **RSS no mide densidad** (las copias COW cuentan varias veces la misma página);
  usa `kling top` (PSS) y el crecimiento real de `free -m`.
- **La caché de disco infla el panel del hipervisor** ("El 81% de memoria del
  panel era caché de disco"): mira el desglose del invitado, no el porcentaje.

### Qué vigilar mientras corre

- **Latencia de thaw (p99).** Cronometra el descongelado de máquinas que llevan
  rato frías, que son las que más probablemente tengan páginas en zram. Si el
  p99 sube de forma que moleste al gateway, baja a `lz4` o reduce el tamaño.
- **CPU del compresor.** `zramctl` y el uso de CPU del host bajo carga: si el
  compresor compite con los invitados, limita `ZRAM_STREAMS` o cambia de algo.
- **Ratio real.** `orig_data_size / compr_data_size` en `mm_stat`/`zramctl`. Si
  ronda 1x, tus páginas divergentes no comprimen y zram no aporta: quítalo.
- **Que no rebase el tope.** `mem_limit` evita que el pool se coma la RAM;
  confirma que no lo tocas de forma constante (señal de haberte quedado corto de
  RAM de verdad, no un problema de zram).

## Resumen honesto

Optimización **operativa, opcional y del host**. No cambia kindling. Puede subir
la densidad de microVMs co-residentes cuando muchas copias de un snapshot
divergen en páginas anónimas comprimibles (heaps de node). Recomendación: zram
sobre zswap, por techo predecible y cero disco en el camino crítico. Su coste es
latencia de descompresión y CPU: por eso la entrega incluye un plan de medición
en vez de una promesa.
