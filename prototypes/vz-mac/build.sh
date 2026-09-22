#!/bin/sh
# Compila el prototipo y lo firma con el entitlement de virtualización.
# La firma ad-hoc (-s -) basta: Virtualization.framework no exige Developer ID,
# solo que el proceso declare com.apple.security.virtualization.
set -e
cd "$(dirname "$0")"
CGO_ENABLED=1 go build -o vzproto .
codesign --entitlements vz.entitlements -s - vzproto
ls -la vzproto
