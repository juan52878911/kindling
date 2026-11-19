#!/usr/bin/env bash
# Comprueba el chart deploy/helm/kindling-sandbox: que crds/ siga idéntico a
# ext/sandbox/deploy/, `helm lint` y `helm template` con cada fichero de ci/ y,
# si está instalado, `kubeconform` sobre lo renderizado.
#
# Uso: scripts/helm-check.sh        (necesita helm; kubeconform es opcional)
set -euo pipefail

raiz="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="$raiz/deploy/helm/kindling-sandbox"
fallos=0

# 1. El CRD del chart no puede separarse del de ext/sandbox/deploy/.
if ! cmp -s "$chart/crds/crd.yaml" "$raiz/ext/sandbox/deploy/crd.yaml"; then
  echo "FALLO: crds/crd.yaml difiere de ext/sandbox/deploy/crd.yaml (copia byte a byte)" >&2
  diff -u "$raiz/ext/sandbox/deploy/crd.yaml" "$chart/crds/crd.yaml" | head -40 >&2 || true
  fallos=$((fallos + 1))
else
  echo "ok: crds/crd.yaml idéntico a ext/sandbox/deploy/crd.yaml"
fi
# Y en el otro sentido: nada en crds/ que no esté en deploy/.
for f in "$chart"/crds/*; do
  [ -e "$raiz/ext/sandbox/deploy/$(basename "$f")" ] || {
    echo "FALLO: crds/$(basename "$f") no existe en ext/sandbox/deploy/" >&2
    fallos=$((fallos + 1))
  }
done

command -v helm >/dev/null || { echo "FALLO: falta helm en el PATH" >&2; exit 1; }

# 2. lint y template con cada valores de ci/.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for v in "$chart"/ci/*.yaml; do
  n="$(basename "$v" .yaml)"
  echo "== $n"
  if ! helm lint "$chart" -f "$v" --strict; then fallos=$((fallos + 1)); continue; fi
  if ! helm template prueba "$chart" -n kindling-system -f "$v" --include-crds >"$tmp/$n.yaml"; then
    fallos=$((fallos + 1)); continue
  fi
  if command -v kubeconform >/dev/null; then
    # El CRD propio no está en el catálogo por defecto: -ignore-missing-schemas
    # deja pasar solo los kinds sin esquema (Sandbox).
    kubeconform -strict -summary -ignore-missing-schemas "$tmp/$n.yaml" || fallos=$((fallos + 1))
  else
    echo "aviso: kubeconform no está instalado; se salta la validación de esquemas"
  fi
done

# 3. Los casos de error esperados tienen que fallar de verdad.
esperar_fallo() {
  local desc="$1"; shift
  if helm template prueba "$chart" -n kindling-system "$@" >/dev/null 2>&1; then
    echo "FALLO: debería fallar: $desc" >&2
    fallos=$((fallos + 1))
  else
    echo "ok (falla como debe): $desc"
  fi
}
esperar_fallo "operador sin Secret de token" 
esperar_fallo "gateway sin hosts" -f "$chart/ci/operator-solo.yaml" --set gateway.enabled=true --set gateway.image.repository=x
esperar_fallo "gateway sin TLS ni insecureListen" -f "$chart/ci/gateway-tls.yaml" --set gateway.tls.existingSecret=

if [ "$fallos" -ne 0 ]; then
  echo "helm-check: $fallos fallo(s)" >&2
  exit 1
fi
echo "helm-check: todo bien"
