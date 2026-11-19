#!/usr/bin/env bash
# Prepara el dorado de delega para kindling: Go (la versión de CI) y las
# dependencias de todos los módulos, para que los trabajadores compilen y
# pasen tests sin red general.
set -euo pipefail
GO_VER=1.27.4
GO_SHA=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840
if ! "$HOME/sdk/go/bin/go" version 2>/dev/null | grep -q "go$GO_VER "; then
	tmp=$(mktemp -d)
	curl -fsSL -o "$tmp/go.tgz" "https://go.dev/dl/go$GO_VER.linux-amd64.tar.gz"
	echo "$GO_SHA  $tmp/go.tgz" | sha256sum -c -
	rm -rf "$HOME/sdk/go" && mkdir -p "$HOME/sdk"
	tar -C "$HOME/sdk" -xzf "$tmp/go.tgz"
	rm -rf "$tmp"
fi
for f in "$HOME/.profile" "$HOME/.bashrc"; do
	grep -q 'sdk/go/bin' "$f" 2>/dev/null || echo 'export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$PATH"' >>"$f"
done
export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$PATH"
export GOTOOLCHAIN=local
for m in . ext/db ext/mcp ext/phone ext/sandbox; do (cd "$m" && go mod download); done
go build ./... >/dev/null
go install honnef.co/go/tools/cmd/staticcheck@v0.8.2
