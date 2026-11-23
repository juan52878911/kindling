#!/usr/bin/env bash
# Las órdenes de docs/quickstart-sandbox.md, en el mismo orden y con las mismas
# palabras. `--dry-run` las imprime («+ orden») y no ejecuta nada; `--run` las
# ejecuta una a una (instala con sudo y deja dos servicios en segundo plano:
# léelo antes). scripts/quickstart_test.go compara el documento con esta lista.
# Compatible con bash 3.2 (macOS).
set -euo pipefail

modo=""
case "${1:-}" in
  --dry-run) modo=dry ;;
  --run) modo=run ;;
  *) echo "usage: $0 --dry-run | --run" >&2; exit 2 ;;
esac

# Una orden por línea; las líneas en blanco y los comentarios no cuentan.
ordenes() {
  cat <<'EOF'
# 1. Install, start the daemon, build the image the template starts from
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sudo sh -s -- --tag v1.0.0-rc.3 --with sandbox
sudo kling up -install
kling image toolchain
# 2. A token and the gateway
export KLING_SANDBOX_TOKEN=$(openssl rand -hex 24)
kling sbx gateway &
# 3. The template (this repository ships the recipe)
git clone --depth 1 https://github.com/juan52878911/kindling
cd kindling
kling sbx template apply -f examples/sandbox/templates/python.json
# 4. First sandbox, with curl
ID=$(curl -s -H "Authorization: Bearer $KLING_SANDBOX_TOKEN" -d '{"template":"python","ttl_seconds":600}' http://127.0.0.1:8090/v1/sandboxes | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -s -H "Authorization: Bearer $KLING_SANDBOX_TOKEN" -d '{"cmd":["python3","-c","print(6*7)"]}' "http://127.0.0.1:8090/v1/sandboxes/$ID/exec?wait=1" | python3 -c 'import base64,json,sys; print(base64.b64decode(json.load(sys.stdin)["stdout"]).decode(), end="")'
curl -s -X DELETE -H "Authorization: Bearer $KLING_SANDBOX_TOKEN" "http://127.0.0.1:8090/v1/sandboxes/$ID"
# 5. With the Python SDK
pip install ./sdks/python
python3 -c 'from kindling_sandbox import Sandbox; sb = Sandbox.create(template="python", ttl=600); print(sb.exec(["python3", "-c", "print(6*7)"]).stdout, end=""); sb.delete()'
# 6. With the TypeScript SDK
export KINDLING=$PWD
cd "$(mktemp -d)"
npm install "$KINDLING/sdks/typescript"
node --input-type=module -e 'import { Sandbox } from "@kindling/sandbox"; const sb = await Sandbox.create({ template: "python", ttl: 600 }); const r = await sb.exec(["python3", "-c", "print(6*7)"]); console.log(r.stdout); await sb.delete();'
cd "$KINDLING"
# 7. With an E2B SDK, through the shim
pip install e2b
kling sbx e2b -listen 127.0.0.1:3000 &
E2B_API_KEY=$KLING_SANDBOX_TOKEN E2B_API_URL=http://127.0.0.1:3000 E2B_SANDBOX_URL=http://127.0.0.1:3000 python3 -c 'from e2b import Sandbox; sb = Sandbox.create("python"); print(sb.commands.run("python3 -c \"print(6*7)\"").stdout, end=""); sb.kill()'
EOF
}

while IFS= read -r linea; do
  case "$linea" in ''|'#'*) continue ;; esac
  echo "+ $linea"
  if [ "$modo" = run ]; then
    eval "$linea"
    # Un servicio en segundo plano necesita un momento antes de la orden siguiente.
    case "$linea" in *'&') sleep 2 ;; esac
  fi
done < <(ordenes)
