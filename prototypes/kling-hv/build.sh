#!/bin/sh
# Compila kling-hv y lo firma ad-hoc con el entitlement de Hypervisor.framework.
set -e
cd "$(dirname "$0")"
/usr/bin/clang -O2 -g -Wall -Wextra -Wno-unused-parameter -mmacosx-version-min=15.2 \
  -o kling-hv main.c fdt.c devices.c net.c -framework Hypervisor
codesign -s - --entitlements hv.entitlements -f kling-hv
