#!/usr/bin/env bash
# Envuelve un servidor MCP en una imagen de kindling.
#
# DOS MODOS, según cómo hable el servidor:
#
#   HTTP nativo — el servidor ya habla Streamable HTTP y escucha él mismo.
#   No lleva puente: el gateway le habla directamente.
#     sudo ./80-mcp-image.sh http <nombre> [-p "apk"] [-n "npm"] [-d dir] -- <comando>
#     sudo ./80-mcp-image.sh http <nombre> <directorio> [paquetes apk]
#
#     El servidor debe escuchar en el puerto que le diga $PORT (8080) y servir
#     el protocolo en /mcp, que es donde el gateway llama. La segunda forma
#     espera un `entrypoint` ejecutable en el directorio.
#
#   stdio — el servidor habla JSON-RPC por tuberías, que es el caso MAYORITARIO
#   entre los servidores MCP open source:
#     sudo ./80-mcp-image.sh stdio <nombre> [-p "apk"] [-n "npm"] [-P "pip"] [-d dir] [-bundle] -- <comando>
#
#     -bundle (servidores node): empaqueta el servidor en UN fichero con esbuild al
#        construir. Acelera mucho el arranque en frío en la microVM (sobre todo arm64/Mac):
#        carga 1 fichero en vez de cientos de node_modules. Medido: ~7 s -> ~2.5 s.
#
#     -n PREINSTALA paquetes npm en la imagen. Es obligatorio, no una comodidad:
#        las microVMs arrancan sin salida a internet, así que un `npx -y` en
#        tiempo de ejecución fallaría al intentar descargar.
#
#     -P hace lo mismo con pip. Media biblioteca de servidores MCP es Python
#        —incluido el de semgrep— y sin esto quedaba fuera de alcance.
#
#     Se le añade kling-bridge, que lanza el servidor como hijo y expone su
#     protocolo por HTTP. El servidor no se entera de nada.
#
# Ejemplos:
#   sudo ./80-mcp-image.sh stdio files -p "nodejs npm" \
#        -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data
#   sudo ./80-mcp-image.sh stdio eco -d ./examples/stdio-server-bin -- /opt/mcp/server
#   sudo ./80-mcp-image.sh http everything -p "nodejs npm" \
#        -n "@modelcontextprotocol/server-everything" -- mcp-server-everything streamableHttp
#   sudo ./80-mcp-image.sh http legacy ./mi-servidor-http
set -euo pipefail

MODE="${1:?uso: $0 <http|stdio> <nombre> ...}"; shift
NAME="${1:?falta el nombre de la imagen}"; shift

ROOT="${KLING_ROOT:-/var/lib/kindling}"
BASE="${BASE_IMAGE:-min}"
GROW="${GROW:-0}"
BRIDGE="${BRIDGE:-./kling-bridge}"

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
[ -f "$ROOT/images/$BASE.ext4" ] || {
  echo "falta la imagen base '$BASE'. Constrúyela con 70-build-minimal-image.sh" >&2; exit 1; }

PKGS=""; NPM=""; PIP=""; EXTRA_DIR=""; EXTRA_ENV=(); CMD=(); BUNDLE=0

# Los dos modos se instalan igual; solo cambia quién habla HTTP al final.
parse_build_opts() {
  while [ $# -gt 0 ]; do
    case "$1" in
      -p) PKGS="$2"; shift 2 ;;
      -n) NPM="$2"; shift 2 ;;
      -P) PIP="$2"; shift 2 ;;
      -d) EXTRA_DIR="$2"; shift 2 ;;
      # -e KEY=VAL hornea una variable de entorno en el entrypoint (repetible).
      # Sirve para apagar comportamientos de red del servidor que, con egress
      # restringido, cuelgan el arranque: p. ej. semgrep hace phone-home de
      # métricas/versión al arrancar y, con la salida en DROP, espera ~2 min al
      # timeout. Con `-e SEMGREP_SEND_METRICS=off -e SEMGREP_ENABLE_VERSION_CHECK=0`
      # el arranque cae de ~124 s a ~10 s.
      -e) EXTRA_ENV+=("$2"); shift 2 ;;
      # -bundle empaqueta un servidor node (-n) en UN fichero con esbuild al
      # construir la imagen. El arranque de node dentro de la microVM no lo frena
      # el CÓMPUTO (es ~ms) sino la tormenta de stat/open/mmap al cargar cientos
      # de ficheros de node_modules, que bajo KVM anidado (arm64/Mac) se amplifica
      # ~25-60×. Colapsarlos en un fichero mata esa tormenta: medido en seqthink
      # (1205 ficheros → 1), el initialize en frío cae de ~7 s a ~2.5 s.
      -bundle) BUNDLE=1; shift ;;
      --) shift; CMD=("$@"); break ;;
      *)  echo "opción desconocida: $1" >&2; exit 1 ;;
    esac
  done
  [ ${#CMD[@]} -gt 0 ] || { echo "falta el comando tras --" >&2; exit 1; }
  # Node y sus dependencias piden bastante más sitio que un binario suelto, y
  # las ruedas de Python bastante más que node: semgrep solo son ~400 MB con
  # sus binarios de OCaml dentro.
  if [ "$GROW" -eq 0 ]; then
    if   [ -n "$PIP" ]; then GROW=1536
    elif [ -n "$NPM" ]; then GROW=768
    else                     GROW=256
    fi
  fi
  # npm implica node; pip implica python. Se añaden si no se pidieron.
  if [ -n "$NPM" ] && ! echo " $PKGS " | grep -q " nodejs "; then
    PKGS="$PKGS nodejs npm"
  fi
  if [ -n "$PIP" ] && ! echo " $PKGS " | grep -q " py3-pip "; then
    PKGS="$PKGS python3 py3-pip"
  fi

  # DETECCIÓN AUTOMÁTICA de dependencia de navegador. Playwright y Puppeteer
  # traen —o son— un servidor MCP que lanza Chromium. Si se ve en los paquetes
  # npm, se hornea Chromium en la imagen y se marca el modo COMPARTIDO (ver
  # kling-bridge/browser.go): un solo navegador y un contexto por sesión. Así el
  # usuario no tiene que añadir chromium ni los flags a mano.
  #
  # Va aquí, ANTES de crear el filesystem, porque Chromium pide sitio (~500 MB)
  # y el disco se dimensiona a partir de GROW más abajo.
  BROWSER=""
  case " $NPM " in
    *playwright*) BROWSER="playwright" ;;
    *puppeteer*)  BROWSER="puppeteer" ;;
  esac
  if [ -n "$BROWSER" ]; then
    echo "dependencia de navegador detectada ($BROWSER) → Chromium + modo compartido"
    if ! echo " $PKGS " | grep -q " chromium "; then
      PKGS="$PKGS chromium chromium-swiftshader nss freetype harfbuzz ttf-freefont font-noto dbus"
    fi
    [ "$GROW" -lt 2560 ] && GROW=2560
  fi
}

# HTTP_PROXY marca el modo http servido POR EL PUENTE (proxy). Lo activa la forma
# con `--` (un comando de servidor HTTP), no la forma antigua (un directorio con
# su propio entrypoint, que sigue hablándole directo al gateway sin puente).
HTTP_PROXY=0
# Puerto donde escucha el servidor HTTP hijo DENTRO del invitado. Distinto del
# 8080 que ve el gateway: en modo proxy el puente ocupa el 8080 y reversa aquí.
CHILD_PORT=8090

case "$MODE" in
  http)
    # Forma antigua: un directorio que ya trae su propio entrypoint. Se queda
    # como estaba: sin puente, el gateway le habla directo al puerto 8080.
    if [ $# -gt 0 ] && [ -d "$1" ]; then
      EXTRA_DIR="$1"; shift
      PKGS="${1:-}"
      [ -x "$EXTRA_DIR/entrypoint" ] || { echo "falta $EXTRA_DIR/entrypoint ejecutable" >&2; exit 1; }
    else
      # Forma con `--`: el servidor HTTP se envuelve en el PUENTE en modo proxy.
      # Así recupera lo que el HTTP directo no tenía —/reset, montaje de
      # volúmenes, reaper de ociosas— a cambio de un hijo COMPARTIDO por todas
      # las sesiones (sin el aislamiento proceso-por-sesión del modo stdio).
      parse_build_opts "$@"
      HTTP_PROXY=1
      [ -f "$BRIDGE" ] || { echo "no encuentro $BRIDGE (compílalo con: make bridge)" >&2; exit 1; }
    fi
    ;;
  stdio)
    parse_build_opts "$@"
    [ -f "$BRIDGE" ] || { echo "no encuentro $BRIDGE (compílalo con: make bridge)" >&2; exit 1; }
    ;;
  *) echo "modo desconocido '$MODE': usa http o stdio" >&2; exit 1 ;;
esac

# ── Imagen por CAPAS ────────────────────────────────────────────────────────
# En vez de copiar la base entera (~110-130 MiB reales duplicados por servicio),
# se construye SOLO el delta como upperdir de un overlay sobre la base (patrón
# OCI). La base queda compartida por todos los servicios; esta imagen guarda
# únicamente lo que cambia. Validado en el kernel de destino — docs/three-layers.md.
#
# La capa se dimensiona con GROW (el tamaño del delta esperado). Es dispersa: el
# coste real en disco es solo lo que se escriba, y al final se encoge al mínimo.
LAYER="$ROOT/images/$NAME.layer.ext4"
BASE_IMG="$ROOT/images/$BASE.ext4"
[ "$GROW" -gt 0 ] || GROW=256
rm -f "$LAYER"
truncate -s "${GROW}M" "$LAYER"
mkfs.ext4 -q -F -O '^has_journal' -E nodiscard "$LAYER"

mnt="$(mktemp -d)"; base_mnt="$(mktemp -d)"; layer_mnt="$(mktemp -d)"

# ov_up/ov_down: montan y desmontan el overlay del BUILD. La base va de lower en
# solo lectura; la capa aporta upperdir y workdir (deben compartir fs y no
# anidarse, por eso /upper y /work son hermanos dentro de la capa). Todo lo que
# el resto del script escribe en $mnt cae en /upper de la capa.
ov_up() {
  mount -o loop,ro "$BASE_IMG" "$base_mnt"
  mount -o loop "$LAYER" "$layer_mnt"
  mkdir -p "$layer_mnt/upper" "$layer_mnt/work"
  mount -t overlay overlay \
    -o "lowerdir=$base_mnt,upperdir=$layer_mnt/upper,workdir=$layer_mnt/work" "$mnt"
}
ov_down() {
  umount "$mnt/proc" 2>/dev/null || true
  umount "$mnt"       2>/dev/null || true
  umount "$layer_mnt" 2>/dev/null || true
  umount "$base_mnt"  2>/dev/null || true
}
cleanup() { ov_down; rmdir "$mnt" "$layer_mnt" "$base_mnt" 2>/dev/null || true; }
trap cleanup EXIT
ov_up

# install_bridge deja el puente donde el entrypoint lo invoca, pero SIN escribirlo
# en la capa cuando la base ya trae ese mismo binario.
#
# Copiarlo igualmente funcionaría —el overlay lo pondría en el upper— pero son
# ~8 MiB de delta por servicio para acabar con una copia byte a byte de algo que
# ya está en la lower de debajo. Y sobre todo: si no está en la capa, actualizar
# el puente de TODOS los servicios por capas es un solo fichero (la base) en vez
# de N. Ver RefreshBridges.
#
# Si la base no lo trae, o trae otro, se copia: la imagen del servicio tiene que
# ser correcta por sí misma, y lo que esté en la capa gana por ir de lower
# delante.
install_bridge() {
  if cmp -s "$BRIDGE" "$mnt/usr/local/bin/kling-bridge"; then
    echo "puente: ya viene en la base '$BASE', no se duplica en la capa"
    return 0
  fi
  install -m755 "$BRIDGE" "$mnt/usr/local/bin/kling-bridge"
}

[ -n "$EXTRA_DIR" ] && cp -a "$EXTRA_DIR"/. "$mnt"/

if [ -n "$PKGS" ]; then
  echo "instalando en la imagen: $PKGS"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  chroot "$mnt" /sbin/apk add --no-cache $PKGS
  umount "$mnt/proc" 2>/dev/null || true
fi

if [ -n "$NPM" ]; then
  echo "preinstalando npm: $NPM"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  # --ignore-scripts NO es opcional.
  #
  # Esto es un chroot sobre el HOST, como root, con /proc del host montado. Sin
  # este flag, los `postinstall` de cualquier paquete de npm se ejecutan aquí:
  # un paquete malicioso del registro no necesita escapar de ninguna microVM,
  # porque corre en el host con privilegios ANTES de que exista ninguna. Sería
  # contradictorio con la premisa del proyecto —"el invitado es hostil"— dejar
  # que el empaquetado ocurra fuera de esa frontera.
  #
  # Rompe los paquetes que compilan binarios nativos en la instalación. Que esos
  # fallen ruidosamente es preferible a ejecutar código arbitrario como root.
  #
  # --omit=dev recorta lo que no hace falta en ejecución; el espacio dentro de la
  # imagen es el que más pesa en el snapshot.
  chroot "$mnt" /usr/bin/npm install -g --ignore-scripts --omit=dev --no-fund --no-audit $NPM
  chroot "$mnt" /bin/sh -c 'rm -rf /root/.npm /usr/lib/node_modules/npm/man' 2>/dev/null || true
  umount "$mnt/proc" 2>/dev/null || true
fi

# -bundle: colapsa node_modules en UN fichero con esbuild. El cuello del arranque de
# node en la microVM no es el cómputo (ms) sino cargar cientos de ficheros; bajo KVM
# anidado cada open/mmap se amplifica muchísimo. Un solo fichero mata esa tormenta.
if [ "$BUNDLE" = 1 ]; then
  if [ -z "$NPM" ]; then
    echo "AVISO: -bundle solo aplica a servidores node (-n); se ignora" >&2
  else
    # Resolver el fichero JS que arranca el servidor: o `node <fichero>`, o un bin de
    # /usr/local/bin (symlink al dist/index.js del paquete).
    case "${CMD[0]}" in
      node|/usr/bin/node|/usr/local/bin/node) ENTRY="${CMD[1]}"; REST=("${CMD[@]:2}") ;;
      *) ENTRY="$(chroot "$mnt" /usr/bin/readlink -f "/usr/local/bin/$(basename "${CMD[0]}")" 2>/dev/null)"; REST=("${CMD[@]:1}") ;;
    esac
    if [ -n "$ENTRY" ] && chroot "$mnt" /usr/bin/test -f "$ENTRY"; then
      echo "empaquetando con esbuild: $ENTRY -> /opt/$NAME.bundle.mjs"
      cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
      mount --bind /proc "$mnt/proc" 2>/dev/null || true
      # esbuild se baja por npx (una vez) y corre en musl; --ignore-scripts ya se
      # aplicó al instalar el paquete, esto solo transforma JS ya presente.
      if chroot "$mnt" /bin/sh -c "cd /opt && npx --yes esbuild '$ENTRY' --bundle --platform=node --format=esm --outfile=/opt/$NAME.bundle.mjs"; then
        chroot "$mnt" /bin/sh -c 'rm -rf /root/.npm' 2>/dev/null || true
        # El entrypoint pasa a ejecutar el bundle; se conserva node_modules por si el
        # servidor lee assets en runtime por ruta absoluta (el bundle no los inlinea).
        CMD=(node "/opt/$NAME.bundle.mjs" ${REST[@]+"${REST[@]}"})
        echo "  ✓ bundle listo; el arranque cargará 1 fichero en vez de node_modules entero"
      else
        echo "AVISO: esbuild falló; se deja el servidor SIN empaquetar (arranca por node_modules)" >&2
      fi
      umount "$mnt/proc" 2>/dev/null || true
    else
      echo "AVISO: -bundle no pudo resolver el entry JS de '${CMD[0]}'; sin empaquetar" >&2
    fi
  fi
fi

# Marcador del modo navegador COMPARTIDO. Lo lee kling-bridge al arrancar: pone en
# marcha un solo Chromium con puerto de depuración y añade `session_args` a cada
# sesión para que conecte a él por CDP con su propio contexto. Ver browser.go.
if [ -n "${BROWSER:-}" ]; then
  echo "modo navegador compartido: escribiendo /etc/kling/browser.json"
  # --isolated NO es opcional en el modo compartido: sin él, cada sesión conecta
  # por CDP y cae en el CONTEXTO POR DEFECTO del Chromium común, así que todas
  # verían la misma página y se pisarían. Con él, cada sesión crea su propio
  # contexto (cookies/almacenamiento/páginas aislados). Verificado: dos sesiones
  # a URLs distintas mantienen cada una la suya.
  case "$BROWSER" in
    playwright) SESSION_ARGS='["--cdp-endpoint","http://127.0.0.1:9222","--isolated"]' ;;
    puppeteer)  SESSION_ARGS='["--browser-url","http://127.0.0.1:9222"]' ;;
    *)          SESSION_ARGS='[]' ;;
  esac
  mkdir -p "$mnt/etc/kling"
  cat > "$mnt/etc/kling/browser.json" <<BJSON
{
  "sidecar": ["/usr/bin/chromium-browser","--headless=new","--no-sandbox","--disable-dev-shm-usage","--disable-gpu","--disable-software-rasterizer","--remote-debugging-address=127.0.0.1","--remote-debugging-port=9222","about:blank"],
  "ready_url": "http://127.0.0.1:9222/json/version",
  "session_args": $SESSION_ARGS
}
BJSON
fi

# DETECCIÓN DE CAPACIDADES. Inspecciona el árbol de dependencias npm ya instalado
# y deja /etc/kling/capabilities.json, que el import lee para configurar el egress
# solo y avisar de módulos nativos. El patrón (marcadores de paquete) sale de
# caracterizar servidores MCP reales:
#   navegador  → playwright* / puppeteer* / chrome-launcher  (ya baja Chromium)
#   internet   → clientes HTTP (axios, undici, got, node-fetch…) o SDKs de
#                servicio (openai, @anthropic-ai, exa-js, @mendable, dotenv…)
#   nativo     → sqlite3 / better-sqlite3 / sharp / canvas / bcrypt (node-gyp)
if [ -n "$NPM" ]; then
  NM="$mnt/usr/lib/node_modules"
  # Sin -maxdepth: el árbol ya instalado es finito y los módulos nativos se
  # anidan a profundidad arbitraria (sharp dentro de @kazuph/mcp-fetch cae más
  # hondo que 4 niveles). Se admite además nombre con scope (@scope/paquete),
  # que hay que casar como PATH y no como -name de un solo componente.
  hasdep() {
    case "$1" in
      */*) find "$NM" -type d -path "*/$1" 2>/dev/null | grep -q . ;;
      *)   find "$NM" -type d -name "$1" 2>/dev/null | grep -q . ;;
    esac
  }
  CAP_EGRESS=none
  [ -n "${BROWSER:-}" ] && CAP_EGRESS=internet   # un navegador navega la web
  for m in axios undici got node-fetch cross-fetch ky openai cohere-ai exa-js \
           firecrawl-js "@anthropic-ai" "@mendable" "@aws-sdk" "@azure" dotenv; do
    hasdep "$m" && CAP_EGRESS=internet
  done
  # Fallback: los que usan el fetch global de Node ≥18 no traen cliente HTTP en
  # deps; se infiere del nombre y de las keywords del paquete principal.
  if [ "$CAP_EGRESS" = none ]; then
    for pkg in $NPM; do
      case "$pkg" in
        *fetch*|*scrape*|*crawl*|*search*|*web*|*http*|*browser*|*firecrawl*|*exa*) CAP_EGRESS=internet ;;
      esac
      pj=$(find "$NM" -maxdepth 3 -name package.json -path "*$pkg*" 2>/dev/null | head -1)
      [ -n "$pj" ] && grep -qiE '"(fetch|scrape|crawl|search|web|http|api-client)"' "$pj" 2>/dev/null && CAP_EGRESS=internet
    done
  fi
  # ── Módulos nativos: detección ESTRUCTURAL (no por lista cerrada) ───────────
  #
  # Antes se buscaba una lista fija de nombres con -maxdepth 4 y fallaba doble:
  # un nombre nuevo no estaba, y sharp anidado en las deps de otro paquete caía
  # fuera del alcance. Ahora se detecta por la ESTRUCTURA del árbol ya instalado:
  #   - binding.gyp                -> el paquete se compila con node-gyp
  #   - marcadores de instaladores de prebuilds en package.json
  #     (node-pre-gyp / prebuild-install / node-gyp-build / node-gyp rebuild /
  #      "gypfile":true). Se anclan a la INVOCACIÓN, no al mero nombre
  #      "node-gyp", que aparece como devDependency en medio ecosistema y daría
  #      falsos positivos (y con ellos, falsos native_missing que romperían el
  #      import). El caso node-gyp puro ya lo cubre binding.gyp.
  #   - ficheros *.node            -> binario nativo YA presente en el árbol
  # El nombre del módulo se deriva del path bajo el ÚLTIMO node_modules/.

  # pkg_of_path deriva "@scope/name" o "name" del primer segmento tras el último
  # node_modules/ del path que se le pasa.
  pkg_of_path() {
    local rel="${1##*/node_modules/}"
    case "$rel" in
      @*/*) local tail="${rel#*/}"; echo "${rel%%/*}/${tail%%/*}" ;;
      *)    echo "${rel%%/*}" ;;
    esac
  }

  # HAVE: paquetes que ya tienen algún *.node (binario nativo presente). Incluye
  # tanto el propio módulo (better-sqlite3 con su build/Release/*.node) como los
  # paquetes de plataforma que otros usan (sharp≥0.33 -> @img/sharp-linuxmusl-*).
  HAVE=$( { find "$NM" -name '*.node' -type f 2>/dev/null \
    | while IFS= read -r f; do pkg_of_path "$f"; done | sort -u; } || true)

  # CAND: paquetes que DECLARAN necesitar un binario nativo (lo compilan o lo
  # descargan en un script de instalación). Son los candidatos a quedarse sin él.
  CAND=$( { {
      find "$NM" -name binding.gyp -type f 2>/dev/null
      grep -rlE '(node-pre-gyp|prebuild-install|node-gyp-build|node-gyp rebuild|"gypfile"[[:space:]]*:[[:space:]]*true)' \
        --include=package.json "$NM" 2>/dev/null
    } | while IFS= read -r f; do pkg_of_path "$f"; done | sort -u; } || true )

  # native = todo lo que toca código nativo (candidatos + los que traen *.node),
  # para el aviso informativo. native_missing = candidatos SIN binario: ni ellos
  # traen *.node ni existe un paquete de plataforma que lo aporte.
  #
  # La verificación evita el FALSO-FALTANTE de sharp≥0.33: sharp declara el
  # binario (binding.gyp), pero su .node vive en @img/sharp-linuxmusl-*, que se
  # instala como optionalDependency normal (sin postinstall) y sobrevive a
  # --ignore-scripts. Se da por satisfecho si el nombre base del módulo (sin
  # scope) aparece en algún paquete que sí trae *.node.
  NATIVE=""; NATIVE_MISSING=""
  for m in $CAND; do
    base="${m##*/}"
    if echo "$HAVE" | grep -qxF "$m" || echo "$HAVE" | grep -qF "$base"; then
      : # tiene binario propio o de plataforma
    else
      NATIVE_MISSING="$NATIVE_MISSING $m"
    fi
  done
  NATIVE=$(printf '%s\n%s\n' "$CAND" "$HAVE" | sed '/^$/d' | sort -u | paste -sd' ' -)
  NATJSON="[]"
  [ -n "$NATIVE" ] && NATJSON="[\"$(echo $NATIVE | sed 's/ /","/g')\"]"
  NATMISSJSON="[]"
  NATIVE_MISSING="$(echo $NATIVE_MISSING | tr ' ' '\n' | sed '/^$/d' | sort -u | paste -sd' ' -)"
  [ -n "$NATIVE_MISSING" ] && NATMISSJSON="[\"$(echo $NATIVE_MISSING | sed 's/ /","/g')\"]"

  # ── Binarios del SISTEMA (no-npm) que el servidor invoca ────────────────────
  #
  # Un MCP de node puede llamar a binarios que NO vienen de npm: git, ffmpeg,
  # ripgrep, pandoc, python… Si no están en la imagen, la tool falla en caliente
  # con "command not found", síntoma que no se parece a su causa. Se detectan por
  # DOS vías, ambas de SOLO LECTURA (no se ejecuta nada del paquete):
  SYS=""
  add_sys() { case " $SYS " in *" $1 "*) ;; *) SYS="$SYS $1" ;; esac; }
  #   (1) Tabla curada npm->apk: dependencias cuya razón de ser es envolver un
  #       binario del sistema. Que exista la dep es señal fuerte de que se usa.
  for m in simple-git isomorphic-git; do hasdep "$m" && add_sys git; done
  for m in fluent-ffmpeg ffmpeg-static;  do hasdep "$m" && add_sys ffmpeg; done
  hasdep "@vscode/ripgrep" && add_sys ripgrep
  hasdep node-pandoc       && add_sys pandoc
  hasdep python-shell      && add_sys python3
  #   (2) Escaneo estático de invocaciones: literales de spawn/exec/execFile/
  #       execa en el código ya instalado. Se INTERSECTA con una allow-list de
  #       nombres que Alpine empaqueta, para no convertir un literal cualquiera
  #       (un nombre de variable, un "cat") en un `apk add`. Solo lectura.
  SYS_ALLOW=" git ffmpeg rg ripgrep pandoc python python3 pdftotext pdfinfo soffice libreoffice convert magick imagemagick tesseract gs ghostscript dot graphviz yt-dlp exiftool "
  bin2apk() {
    case "$1" in
      rg)              echo ripgrep ;;
      python)          echo python3 ;;
      convert|magick)  echo imagemagick ;;
      soffice)         echo libreoffice ;;
      dot)             echo graphviz ;;
      gs)              echo ghostscript ;;
      *)               echo "$1" ;;
    esac
  }
  SCAN_RE='(spawn|execFile|exec|execa)\([[:space:]]*["'"'"']([a-z0-9._/-]+)'
  for raw in $( { grep -rhoE "$SCAN_RE" "$NM" 2>/dev/null \
      | sed -E 's/.*["'"'"']//' | sed -E 's#.*/##' | sort -u; } || true); do
    case "$SYS_ALLOW" in
      *" $raw "*) add_sys "$(bin2apk "$raw")" ;;
    esac
  done
  # Descartar los que ya están en la imagen. El escaneo caza también binarios
  # base ya presentes (node, sh…) que no hay que reinstalar. Se usa `command -v`
  # (builtin de sh) en vez de `which`, que la imagen mínima puede no traer.
  SYS_FINAL=""
  for b in $SYS; do
    chroot "$mnt" /bin/sh -c "command -v '$b'" >/dev/null 2>&1 || SYS_FINAL="$SYS_FINAL $b"
  done
  SYS="$(echo $SYS_FINAL | tr ' ' '\n' | sed '/^$/d' | sort -u | paste -sd' ' -)"
  SYSJSON="[]"
  [ -n "$SYS" ] && SYSJSON="[\"$(echo $SYS | sed 's/ /","/g')\"]"

  # apk add de los binarios de sistema detectados. El disco ya se dimensionó
  # ANTES de montar (no se sabía esta lista entonces), así que se agranda EN
  # CALIENTE: desmontar, crecer, reparar el journal y remontar. GROW sube según
  # el peso esperado —libreoffice pesa lo suyo— y solo si la lista no está vacía.
  if [ -n "$SYS" ]; then
    echo "binarios de sistema detectados: $SYS → apk add"
    SYS_GROW=256
    case " $SYS " in *" libreoffice "*|*" imagemagick "*) SYS_GROW=768 ;; esac
    ov_down
    truncate -s "+${SYS_GROW}M" "$LAYER"
    e2fsck -fp "$LAYER" >/dev/null 2>&1 || true
    resize2fs "$LAYER" >/dev/null 2>&1
    ov_up
    cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
    mount --bind /proc "$mnt/proc" 2>/dev/null || true
    chroot "$mnt" /sbin/apk add --no-cache $SYS \
      || echo "AVISO: 'apk add' de binarios de sistema falló ($SYS); revisa la lista" >&2
    umount "$mnt/proc" 2>/dev/null || true
  fi

  BROWSERJSON=false; [ -n "${BROWSER:-}" ] && BROWSERJSON=true

  # SEMILLA de dominios permitidos, para el modo egress=allowlist. Se extraen los
  # hosts de los literales de URL del árbol npm ya instalado. Es una PISTA
  # EDITABLE, no exhaustiva ni autoritativa: un servidor puede construir URLs en
  # tiempo de ejecución que no aparecen como literales, y aquí se cuelan hosts de
  # documentación o de badges. El import solo la usa si se elige -egress allowlist
  # (con -allow se sustituye entera); en none/internet se ignora. Solo se siembra
  # cuando el egress detectado apunta a un servicio concreto (internet), no en el
  # caso vacío 'none'.
  ALLOWJSON="[]"
  if [ "$CAP_EGRESS" = internet ]; then
    DOMAINS=$(grep -rhoE 'https?://[a-zA-Z0-9._-]+' "$NM" 2>/dev/null \
      | sed -E 's#^https?://##' \
      | grep -viE '^(localhost|127\.|0\.0\.0\.0|example\.(com|org|net)|schemas?\.|www\.w3\.org|json-schema\.org|registry\.npmjs\.org|nodejs\.org|github\.com)$' \
      | sort -u | head -40)
    if [ -n "$DOMAINS" ]; then
      ALLOWJSON="[\"$(echo "$DOMAINS" | paste -sd, - | sed 's/,/","/g')\"]"
    fi
  fi

  mkdir -p "$mnt/etc/kling"
  cat > "$mnt/etc/kling/capabilities.json" <<CJSON
{
  "browser": $BROWSERJSON,
  "egress": "$CAP_EGRESS",
  "native": $NATJSON,
  "native_missing": $NATMISSJSON,
  "system": $SYSJSON,
  "allow_domains": $ALLOWJSON
}
CJSON
  echo "capacidades detectadas: browser=$BROWSERJSON egress=$CAP_EGRESS native='${NATIVE:-ninguno}' native_missing='${NATIVE_MISSING:-ninguno}' system='${SYS:-ninguno}' allow_domains=$ALLOWJSON"
fi

# Igual que npm, y por la misma razón: el invitado arranca SIN salida a
# internet, así que un `pip install` en tiempo de ejecución fallaría.
#
# --break-system-packages hace falta desde PEP 668: Alpine marca su python como
# "externally managed" y pip se niega a tocarlo sin esto. Aquí es inofensivo —
# la imagen es de un solo uso y no hay nada más que romper.
if [ -n "$PIP" ]; then
  echo "preinstalando pip: $PIP"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  # --only-binary=:all: por la misma razón que --ignore-scripts en npm: un
  # sdist ejecuta su setup.py durante la instalación, aquí como root sobre el
  # host. Con ruedas precompiladas no se ejecuta nada del paquete.
  chroot "$mnt" /usr/bin/pip install --no-cache-dir --break-system-packages \
    --only-binary=:all: $PIP
  chroot "$mnt" /bin/sh -c 'rm -rf /root/.cache /usr/lib/python3*/site-packages/pip/_vendor/certifi/*.pem.orig' 2>/dev/null || true
  umount "$mnt/proc" 2>/dev/null || true
fi

if [ "$MODE" = "stdio" ]; then
  install_bridge
  # El entrypoint es PID 1 de la microVM: si muere, el kernel entra en pánico.
  # `exec` evita dejar un shell intermedio que no aporta nada.
  {
    echo '#!/bin/sh'
    echo '# Generado por 80-mcp-image.sh — envoltorio stdio -> Streamable HTTP.'
    echo '#'
    echo '# El entrypoint es PID 1 y el kernel no le pasa PATH, así que hay que'
    echo '# fijarlo: sin él no se encuentran los binarios que instala npm.'
    echo 'export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
    echo 'export HOME=/root'
    for kv in ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}; do echo "export $kv"; done
    printf 'exec /usr/local/bin/kling-bridge -listen :8080 --'
    for a in "${CMD[@]}"; do printf ' %q' "$a"; done
    echo
  } > "$mnt/entrypoint"
elif [ "$HTTP_PROXY" = 1 ]; then
  # HTTP NATIVO TRAS EL PUENTE (modo proxy). El servidor MCP habla Streamable
  # HTTP/SSE de fábrica; el puente lo arranca UNA vez, espera a su puerto y le
  # reversa las peticiones del gateway (ver cmd/kling-bridge/proxy.go).
  #
  # Frente al HTTP directo de la "forma antigua", esto recupera lo que el puente
  # ya sabía hacer: POST /reset (que resuelve el bug del snapshot dorado sin el
  # hack de KLING_HTTP_RESET_AFTER), el montaje de kling.volume dentro del
  # invitado, y el reaper de sesiones ociosas.
  #
  # PROPIEDAD QUE SE PIERDE: el hijo es ÚNICO y COMPARTIDO por todas las
  # sesiones, así que NO hay el aislamiento proceso-por-sesión que sí da el modo
  # stdio. Si el servidor mezclara estado entre sesiones, el puente ya no lo
  # impediría. Es el precio de correr un servidor HTTP multi-sesión tal cual.
  install_bridge
  # Marcador que lee el puente al arrancar para entrar en modo proxy.
  mkdir -p "$mnt/etc/kling"
  cat > "$mnt/etc/kling/service.json" <<SJSON
{
  "transport": "http",
  "port": $CHILD_PORT
}
SJSON
  {
    echo '#!/bin/sh'
    echo '# Generado por 80-mcp-image.sh — servidor HTTP nativo tras el puente.'
    echo '#'
    echo '# El entrypoint es PID 1 y el kernel no le pasa PATH, así que hay que'
    echo '# fijarlo: sin él no se encuentran los binarios que instala npm.'
    echo 'export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
    echo 'export HOME=/root'
    for kv in ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}; do echo "export $kv"; done
    echo '# El servidor hijo escucha en este puerto; el puente ocupa el 8080 (lo'
    echo '# que ve el gateway) y le reversa las peticiones. service.json le dice'
    echo '# al puente el mismo número.'
    echo "export PORT=$CHILD_PORT"
    printf 'exec /usr/local/bin/kling-bridge -listen :8080 --'
    for a in "${CMD[@]}"; do printf ' %q' "$a"; done
    echo
  } > "$mnt/entrypoint"
fi

[ -f "$mnt/entrypoint" ] || { echo "la imagen se queda sin entrypoint" >&2; exit 1; }
chmod +x "$mnt/entrypoint"
sync
ov_down                       # desmonta overlay, capa y base

# La capa ya contiene solo el delta en /upper. Se quita /work (scratch de
# overlayfs), se encoge al mínimo y se comprueba: es lo único que viaja por
# servicio, así que cuanto más pequeña, mejor.
mount -o loop "$LAYER" "$layer_mnt"
rm -rf "$layer_mnt/work"
umount "$layer_mnt"

# Comprobar el sistema de ficheros de la capa ANTES de que nadie la arranque. El
# invitado la monta en SOLO LECTURA como lower del overlay, y un ext4 con el
# journal a medias no se puede montar así: el kernel entra en pánico y el fallo
# aparece como "el servidor no abrió el puerto", que no se parece a la causa.
sync
resize2fs -M "$LAYER" >/dev/null 2>&1 || true
if ! e2fsck -fp "$LAYER" >/dev/null 2>&1; then
  echo "AVISO: el sistema de ficheros de la capa necesitaba reparación" >&2
  e2fsck -fy "$LAYER" >/dev/null 2>&1 || {
    echo "ERROR: '$NAME' quedó con una capa irreparable; bórrala y repite" >&2
    exit 1; }
fi

chmod a+r "$LAYER"

echo "imagen '$NAME' lista — capa sobre base '$BASE' ($(du -h "$LAYER" | cut -f1) reales; la base se comparte)"
if [ "$HTTP_PROXY" = 1 ]; then
  echo "modo http (proxy): el servidor MCP habla HTTP nativo y lo envuelve el puente."
  echo "  AISLAMIENTO: el hijo es COMPARTIDO por todas las sesiones (un solo proceso"
  echo "  para todos los Mcp-Session-Id). No hay el aislamiento proceso-por-sesión del"
  echo "  modo stdio; a cambio funcionan /reset, los volúmenes y el reaper de ociosas."
fi
echo
echo "Congélala como servicio del gateway:"
echo "  kling run -name ${NAME}-tmpl -image $NAME -service $NAME"
echo "  kling commit ${NAME}-tmpl $NAME && kling stop ${NAME}-tmpl"
echo
echo "Y a partir de ahí el gateway la despierta sola:"
echo "  curl -X POST http://127.0.0.1:8080/mcp/$NAME/ -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}'"
