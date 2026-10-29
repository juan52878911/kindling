#!/bin/sh
# Compila hvcow y lo firma ad-hoc con el entitlement de Hypervisor.framework.
set -e
cd "$(dirname "$0")"
/usr/bin/clang -O2 -Wall -o hvcow hvcow.c -framework Hypervisor
codesign -s - --entitlements hv.entitlements -f hvcow
