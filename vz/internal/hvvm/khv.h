// kling-hv: VMM de kindling sobre Hypervisor.framework (arm64), como
// biblioteca. Sale de prototypes/kling-hv: el mismo mapa de memoria, los mismos
// dispositivos y el mismo snapshot con la RAM en un fichero mapeado
// MAP_PRIVATE, pero sin main(): la maneja Go (hvvm_darwin.go) detrás del
// servidor HTTP de kling-vz.
//
// Un proceso aloja UNA VM (Hypervisor.framework solo deja una por proceso), así
// que el estado es global, como en el prototipo.
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
#define RTC_BASE 0x09010000ULL
#define RTC_SIZE 0x1000ULL
#define VIRTIO_BASE 0x0C000000ULL
#define VIRTIO_STRIDE 0x200ULL
#define VIRTIO_MAX 12
#define KHV_MAX_DISKS 8
#define RAM_BASE 0x80000000ULL

#define MAX_CPUS 8
#define PAGINA 16384ULL

// ---- API para Go ----

struct khv_disk {
  const char *path;
  int ro;
};

struct khv_config {
  int ncpus;
  uint64_t mem_size;  // bytes
  const char *kernel;
  const char *cmdline;
  const char *ram_dir;  // dónde crear el fichero disperso de la RAM de un arranque en frío
  int ndisks;
  struct khv_disk disks[KHV_MAX_DISKS];
  int net_fd;  // -1 sin red: socket SOCK_DGRAM, una trama por datagrama
  uint8_t mac[6];
  int rng;         // virtio-rng
  int vsock;       // virtio-vsock (el agente por AF_VSOCK)
  int balloon;     // virtio-balloon: 0 sin globo, 1 con él, 2 con deflate_on_oom
  int console_fd;  // salida de la UART
};

// Bits de pausa: la VM corre solo con todos a cero. El núcleo (PATCH /vm) y el
// regulador de CPU pausan cada uno por su lado.
#define KHV_PAUSA_API 1u
#define KHV_PAUSA_FRENO 2u

const char *khv_error(void);
int khv_create(const struct khv_config *c);
int khv_boot(void);
int khv_restore(const char *mem_path);
int khv_pause(unsigned bit);
int khv_resume(unsigned bit);
int khv_snapshot(const char *mem_out);
int khv_wait(void);  // bloquea hasta que la VM termina: 0 si el invitado se apagó
void khv_stop(void);
uint64_t khv_footprint(void);
uint64_t khv_cpu_ns(void);
void khv_tiempos(double *primer_run_ms, double *restaurado_ms);

// ---- interno ----

double ms_desde_inicio(void);
void fallo(const char *fmt, ...) __attribute__((format(printf, 1, 2)));  // fija khv_error
void fin_vm(int codigo, const char *motivo);  // la VM termina (desde cualquier hilo)

// ---- memoria del invitado ----
extern uint8_t *g_ram;
extern uint64_t g_ram_size;
void *gpa_ptr(uint64_t gpa, uint64_t len);  // NULL si se sale de la RAM
int ram_soltar(uint64_t off, uint64_t len);  // devuelve esas páginas al fichero (globo)

// Pausa de los dispositivos: con g_pausa los hilos de disco y red no tocan la
// RAM (restaurando o haciendo snapshot). La cambia vm.c con mu_dev.
extern volatile bool g_pausa;

// ---- FDT ----
struct fdt_params {
  uint64_t ram_size;
  int ncpus;
  const char *bootargs;
  uint64_t dist_size;
  uint64_t redist_size;  // por vCPU
  uint32_t uart_spi;     // INTID
  uint32_t rtc_spi;
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
void uart_init(uint32_t intid, int fd);
bool uart_mmio(uint64_t off, bool escribir, uint64_t *val);
void uart_flush(void);
void uart_get(struct uart_state *s);
void uart_reafirmar(void);
void uart_set(const struct uart_state *s);

// ---- RTC PL031 ----
struct rtc_state {
  int64_t offset;  // segundos que se suman a la hora del host
  uint32_t mr, imsc, ris, cr;
};
void rtc_init(void);
bool rtc_mmio(uint64_t off, bool escribir, uint64_t *val, int size);
void rtc_get(struct rtc_state *s);
void rtc_set(const struct rtc_state *s);

// ---- virtio-mmio ----
struct vq {
  uint32_t num, ready;
  uint64_t desc, avail, used;
  uint16_t last_avail;
};
struct vdesc {
  uint64_t addr;
  uint32_t len;
  uint16_t flags, next;
};
#define VIRTQ_DESC_F_NEXT 1
#define VIRTQ_DESC_F_WRITE 2
#define QMAX 256
#define VIRTQ_DESC_F_INDIRECT 4
bool vq_ptrs(struct vq *q, uint16_t **avail, uint8_t **used, struct vdesc **desc);
void vq_push(struct vq *q, uint8_t *used, uint16_t head, uint32_t len);
// Cadenas de descriptores, con todos los topes: vq_cadena copia la cadena de
// head en out (como mucho max) y devuelve cuántos tiene, o -1 si es inválida
// (índice fuera de la tabla, más larga que la cola, indirecta). vq_leer junta
// los descriptores de lectura en buf (0 si no caben en cap o alguno se sale de
// la RAM); vq_capacidad suma los de escritura; vq_escribir los llena en orden.
int vq_cadena(const struct vq *q, const struct vdesc *desc, uint16_t head, struct vdesc *out, int max);
uint32_t vq_leer(const struct vq *q, const struct vdesc *desc, uint16_t head, void *buf, uint32_t cap);
uint32_t vq_capacidad(const struct vq *q, const struct vdesc *desc, uint16_t head);
uint32_t vq_escribir(const struct vq *q, const struct vdesc *desc, uint16_t head, const void *src, uint32_t len);

// Registros comunes de virtio-mmio (versión 2), compartidos por los tres
// dispositivos. vmmio_acceso atiende un acceso por debajo de 0x100 y dice qué
// tiene que hacer el dispositivo después.
struct vmmio {
  uint32_t status, drv_feat_sel, dev_feat_sel, queue_sel, isr;
  uint64_t drv_features;
  uint8_t nivel;  // último nivel de la línea de IRQ
};
enum { VM_NADA, VM_NOTIFY, VM_RESET, VM_ISR };
int vmmio_acceso(struct vmmio *m, struct vq *qs, int nq, uint32_t devid, uint64_t feat_lo, uint64_t off,
                 bool escribir, uint64_t *val, uint32_t *cola);
void vmmio_irq(struct vmmio *m, uint32_t intid);  // pasa al GIC el nivel de isr si cambió

// block: una cola, atendida por un hilo propio (fuera del hilo del vCPU)
struct vblk_state {
  struct vmmio m;
  struct vq q;
};
int vblk_init(int idx, const char *ruta, bool ro, uint32_t intid);
bool vblk_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size);
void vblk_get(int idx, struct vblk_state *s);
void vblk_reafirmar(int idx);
void vblk_set(int idx, const struct vblk_state *s);
void vblk_quieto(int idx);     // espera a que el hilo del disco no toque la RAM
void vblk_despertar(int idx);  // vuelve a mirar la cola (tras pausar o restaurar)

// net (tramas por un socket unix SOCK_DGRAM)
struct vnet_state {
  struct vmmio m;
  struct vq q[2];
};
void vnet_init(int fd, const uint8_t mac[6], uint32_t intid);
bool vnet_mmio(uint64_t off, bool escribir, uint64_t *val, int size);
void vnet_get(struct vnet_state *s);
void vnet_reafirmar(void);
void vnet_set(const struct vnet_state *s);
void vnet_quieto(void);
void vnet_despertar(void);

// rng
struct vrng_state {
  struct vmmio m;
  struct vq q;
};
void vrng_init(uint32_t intid);
bool vrng_mmio(uint64_t off, bool escribir, uint64_t *val, int size);
void vrng_get(struct vrng_state *s);
void vrng_reafirmar(void);
void vrng_set(const struct vrng_state *s);

// vsock (virtio-vsock; ver vsock.c)
#define VS_HDR 44
struct vvsock_state {
  struct vmmio m;
  struct vq q[3];
};
void vsock_init(uint32_t intid);
bool vsock_mmio(uint64_t off, bool escribir, uint64_t *val, int size);
void vsock_get(struct vvsock_state *s);
void vsock_reafirmar(void);
void vsock_set(const struct vvsock_state *s);
void vsock_quieto(void);
void vsock_despertar(void);
// khv_vsock_dial abre una conexión al puerto port del invitado y devuelve el
// descriptor de un socket de dominio Unix que la lleva, o -errno.
int khv_vsock_dial(uint32_t port, int plazo_ms);

// balloon (virtio-balloon; ver balloon.c)
struct vballoon_state {
  struct vmmio m;
  struct vq q[2];
  uint32_t num_pages, actual;  // en marcos de 4 KiB
};
int vballoon_init(uint32_t intid, bool deflate_on_oom);
bool vballoon_mmio(uint64_t off, bool escribir, uint64_t *val, int size);
void vballoon_objetivo(uint32_t marcos);
uint32_t vballoon_actual(void);
uint64_t vballoon_sueltas(void);
bool vballoon_inflada(uint64_t pagina);
void vballoon_get(struct vballoon_state *s);
void vballoon_reafirmar(void);
void vballoon_set(const struct vballoon_state *s);
// khv_balloon fija el globo en marcos de 4 KiB; -1 si la VM no tiene globo.
int khv_balloon(uint32_t marcos);

// ---- despertar de vCPU en WFI ----
void vcpu_kick_all(void);

#endif
