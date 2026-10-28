// Package hvvm es la VM de kling-hv: un VMM propio sobre Hypervisor.framework
// (macOS en Apple Silicon) escrito en C (vm.c, virtio.c, net.c, devices.c,
// fdt.c) y envuelto aquí para el servidor de vz/internal/server, que lo ve
// como una server.VM más.
//
// Lo que lo distingue de vz/internal/vzvm es la memoria: la RAM del invitado
// es un fichero mapeado MAP_PRIVATE, así que restaurar un snapshot no copia
// nada (las páginas entran cuando el invitado las toca y las limpias se
// comparten entre réplicas). Ver prototypes/kling-hv/README.md para las
// medidas y docs/backend-hv.md para el contrato.
//
// Solo compila en darwin/arm64; en el resto el paquete está vacío.
package hvvm
