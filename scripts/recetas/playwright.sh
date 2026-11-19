#!/usr/bin/env bash
# Receta: Playwright (Chromium) en un sandbox; captura de una página.
# Doc: docs/recetas/playwright.md
#
# No lleva credencial: el navegador no necesita ninguna clave. Lo que sí
# lleva es egress allowlist con un solo dominio, para que una página hostil no
# pueda sacar datos a otro sitio.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
# shellcheck source=lib.sh
. ./lib.sh

uso() {
  cat <<'U'
uso: playwright.sh [--dry-run] [dominio]
  dominio: el único sitio al que el sandbox puede salir (por defecto example.org).
  --dry-run  imprime los comandos sin ejecutarlos.
U
}

analizar_args "$@"
DOMINIO=${ARGS[0]:-example.org}
PLANTILLA=${PLANTILLA:-playwright}
NOMBRE=${NOMBRE:-receta-playwright}
SALIDA=${SALIDA:-captura.png}

# 1. Plantilla: imagen oficial de Playwright (Chromium ya dentro) + el paquete.
ejecutar "$KLING" image import mcr.microsoft.com/playwright:v1.49.1-noble \
  -warm -template "$PLANTILLA" -egress internet -mem 2G \
  -step 'mkdir -p /work && cd /work && npm init -y >/dev/null && npm install playwright@1.49.1'

# 2. Sandbox: solo sale a $DOMINIO.
ID=$(crear_sandbox "$NOMBRE" -from "$PLANTILLA" "$DOMINIO")

# 3. El script de captura entra por stdin y se ejecuta dentro.
GUION='const { chromium } = require("playwright");
(async () => {
  const b = await chromium.launch({ args: ["--no-sandbox"] });
  const p = await b.newPage({ viewport: { width: 1280, height: 800 } });
  await p.goto(process.argv[2], { waitUntil: "load" });
  await p.screenshot({ path: "/work/captura.png" });
  await b.close();
})();'
printf '+ printf %%s <guion.js> | %s cp - %s:/work/captura.js\n' "$KLING" "$ID"
if [ "$DRY" -eq 0 ]; then printf '%s' "$GUION" | "$KLING" cp - "$ID:/work/captura.js"; fi
ejecutar "$KLING" exec -w /work -timeout 3m "$ID" -- node captura.js "https://$DOMINIO"

# 4. Resultado: el PNG sale a un fichero del host.
printf '+ %s cp %s:/work/captura.png - > %q\n' "$KLING" "$ID" "$SALIDA"
if [ "$DRY" -eq 0 ]; then "$KLING" cp "$ID:/work/captura.png" - > "$SALIDA"; fi

ejecutar "$KLING" sandbox rm -f "$ID"
