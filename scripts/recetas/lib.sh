#!/usr/bin/env bash
# Funciones comunes de las recetas. Se carga con `source`, no se ejecuta.
#
# Contrato: la clave real solo viaja por stdin hacia `kling machine
# credential` (nunca por argv ni por el entorno de un comando) y este fichero
# no la imprime: en --dry-run se ve «<clave: $VAR>» en su lugar.

DRY=0
KLING=${KLING:-kling}

# analizar_args lee --dry-run/-n y -h; deja el resto en ARGS.
analizar_args() {
  ARGS=()
  for a in "$@"; do
    case "$a" in
      --dry-run|-n) DRY=1 ;;
      -h|--help) uso; exit 0 ;;
      *) ARGS+=("$a") ;;
    esac
  done
}

# mostrar imprime un comando citado para que se pueda copiar.
mostrar() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
}

# ejecutar corre el comando, o solo lo muestra con --dry-run.
ejecutar() {
  mostrar "$@"
  if [ "$DRY" -eq 0 ]; then "$@"; fi
}

# exigir_clave comprueba que la variable $1 existe, sin mostrarla. En
# --dry-run no hace falta.
exigir_clave() {
  if [ "$DRY" -eq 0 ] && [ -z "${!1:-}" ]; then
    echo "falta la variable $1 con la clave (no se imprime nunca)" >&2
    exit 2
  fi
}

# entregar_credencial <variable-con-la-clave> <ref> <dominio> <env-del-invitado> [flags...]
# La clave entra por stdin; el comando mostrado no la lleva.
entregar_credencial() {
  local var=$1 ref=$2 dominio=$3 env=$4
  shift 4
  printf '+ printf %%s "<clave: $%s>" | %s machine credential %q -domain %q -env %q' \
    "$var" "$KLING" "$ref" "$dominio" "$env"
  printf ' %q' "$@"
  printf '\n'
  if [ "$DRY" -eq 0 ]; then
    printf '%s' "${!var}" | "$KLING" machine credential "$ref" -domain "$dominio" -env "$env" "$@"
  fi
}

# crear_sandbox <nombre> <-from|-image> <valor> <dominio>
# Imprime el id (en --dry-run, el nombre).
crear_sandbox() {
  local nombre=$1 modo=$2 origen=$3 dominio=$4
  mostrar "$KLING" sandbox create -name "$nombre" "$modo" "$origen" \
    -egress allowlist -allow "$dominio" -ttl 30m -wait-ready -q >&2
  if [ "$DRY" -eq 0 ]; then
    "$KLING" sandbox create -name "$nombre" "$modo" "$origen" \
      -egress allowlist -allow "$dominio" -ttl 30m -wait-ready -q
  else
    echo "$nombre"
  fi
}
