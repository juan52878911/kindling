// kling-hv: VMM mínimo de kindling sobre Hypervisor.framework (arm64).
// Cabecera común: mapa de memoria del invitado, dispositivos y estado.
#ifndef KHV_H
#define KHV_H

#include <Hypervisor/Hypervisor.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

// Mapa de direcciones físicas del invitado (IPA). Parecido a QEMU virt.
#define GIC_DIST_BASE 0x08000000ULL
#define GIC_REDIST_BASE 0x0A000000ULL  // HVF reserva 32 MiB (256 vCPU)
#define UART_BASE 0x09000000ULL
#define UART_SIZE 0x1000ULL
#define VIRTIO_BASE 0x0C000000ULL
#define VIRTIO_STRIDE 0x200ULL
#define VIRTIO_MAX 4
#define RAM_BASE 0x80000000ULL

#define MAX_CPUS 8
#define PAGINA 16384ULL

#define OK(x)                                                         \
  do {                                                                \
    hv_return_t _r = (x);                                             \
    if (_r != HV_SUCCESS) {                                           \
      fprintf(stderr, "kling-hv: %s falló: 0x%x\n", #x, (unsigned)_r); \
      exit(1);                                                        \
    }                                                                 \
  } while (0)

double ms_desde_inicio(void);
void morir(const char *fmt, ...) __attribute__((noreturn, format(printf, 1, 2)));

// ---- memoria del invitado ----
extern uint8_t *g_ram;
extern uint64_t g_ram_size;
void *gpa_ptr(uint64_t gpa, uint64_t len);  // NULL si se sale de la RAM

// ---- FDT ----
struct fdt_params {
  uint64_t ram_size;
  int ncpus;
  const char *bootargs;
  uint64_t dist_size;
  uint64_t redist_size;  // por vCPU
  uint32_t spi_base;     // primer INTID de SPI
  uint32_t uart_spi;     // INTID
  int nvirtio;
  uint32_t virtio_spi[VIRTIO_MAX];
};
size_t fdt_build(void *buf, size_t cap, const struct fdt_params *p);

// ---- UART 16550 ----
struct uart_state {
  uint8_t ier, lcr, mcr, scr, dll, dlm, fcr;
  uint8_t thr_pend;  // interrupción de THR vacío pendiente
  uint8_t nivel;     // último nivel de la línea de IRQ
};
void uart_init(uint32_t intid);
bool uart_mmio(uint64_t off, bool escribir, uint64_t *val);
void uart_rx_push(const char *s, size_t n);
void uart_start_stdin(void);
void uart_get(struct uart_state *s);
void uart_set(const struct uart_state *s);
// Marcas de consola: cuando la salida termina en la cadena se llama a cb(arg)
// (desde el hilo del vCPU con la UART bloqueada: cb no debe tocar la UART).
void uart_add_marca(const char *marca, void (*cb)(int), int arg);
extern bool g_uart_tiempos;  // anteponer ms a cada línea

// ---- virtio-mmio block ----
struct vblk_queue {
  uint32_t num, ready;
  uint64_t desc, avail, used;
  uint16_t last_avail;
};
struct vblk_state {
  uint32_t status, drv_feat_sel, dev_feat_sel, queue_sel, isr;
  uint64_t drv_features;
  struct vblk_queue q;
  uint8_t nivel;
};
void vblk_init(int idx, const char *ruta, bool ro, uint32_t intid);
bool vblk_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size);
int vblk_count(void);
void vblk_get(int idx, struct vblk_state *s);
void vblk_set(int idx, const struct vblk_state *s);

// ---- despertar de vCPU en WFI ----
void vcpu_kick_all(void);

#endif
