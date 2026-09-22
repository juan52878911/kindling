# vz-mac — prototipo de backend nativo para macOS

Mide si kindling puede correr en un Mac Apple Silicon **sin VM Linux intermedia
ni virtualización anidada**, usando Virtualization.framework en lugar de
Firecracker. Los resultados y la conclusión están en
[`docs/vz-mac-prototipo.md`](../../docs/vz-mac-prototipo.md).

Es un módulo Go aparte a propósito: trae cgo y una dependencia
(`github.com/Code-Hex/vz/v3`), y el binario `kling` sigue sin ninguna.

## Qué necesita

- macOS 14 o superior (guardar y restaurar estado existe desde Sonoma). Medido en
  macOS 26.5 sobre un M4.
- Los artefactos de kindling en `guest/`: el `vmlinux` aarch64 de Firecracker (ya es
  un `Image` arm64, se usa tal cual), `min.ext4`, una plantilla de overlay y, para
  el servicio de node, `seqthink.layer.ext4`. Se sacan de cualquier host con el
  daemon (`/var/lib/kindling/images`).

## Compilar

```sh
./build.sh      # go build + codesign con el entitlement com.apple.security.virtualization
```

## Medir

```sh
# imagen mínima: arranque, guardar, restaurar, densidad, globo
./vzproto boot    -save guest/state/min.vzs
./vzproto restore -state guest/state/min.vzs -n 10 -parallel
./vzproto restore -state guest/state/min.vzs -n 2 -stress 150 -balloon 64

# servicio MCP real (node) con red NAT y sonda HTTP al puente
./vzproto boot    -ip 192.168.64.222 -layer guest/seqthink.layer.ext4 -save guest/state/seq.vzs
./vzproto restore -ip 192.168.64.222 -layer guest/seqthink.layer.ext4 -state guest/state/seq.vzs -n 1
```

```sh
# flota de MCPs reales: arranque con compuerta, initialize, carga con tools/call, ciclos pausa/reanuda
./vzproto fleet -n 4 -gate 4 -layer guest/files.layer.ext4 -ip 192.168.64.100 \
    -call list_directory -args '{"path":"/tmp"}' -duration 15s -concurrency 8 -cycles 3

# cuántas máquinas admite el host antes de que el framework se niegue
./vzproto maxvms -mem 256
```

La memoria se mide sumando `phys_footprint` de este proceso y de los auxiliares
`com.apple.Virtualization.VirtualMachine` que crea el framework: el invitado vive
ahí, no en el proceso que configura la VM.
