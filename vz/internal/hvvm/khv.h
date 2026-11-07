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
// virtio-fs (vfs.c): un dispositivo por carpeta viva, detrás de los demás. La
// cabecera del snapshot guarda los INTID de los VIRTIO_MAX primeros: los de
// estos salen del mismo spi_base, así que no hace falta.
#define KHV_MAX_VFS 8
#define VIRTIO_SLOTS (VIRTIO_MAX + KHV_MAX_VFS)

// Versión del estado del volcado (la cabecera del pie de mem.file, KLNGHV02).
// La lee Go (hvvm.VersionVolcado) para `kling-hv -snapshot-formats`: `kling
// upgrade` la pregunta al kling-hv viejo antes de volver atrás con lo que el
// nuevo congeló.
#define KHV_SNAP_VERSION 6
#define RAM_BASE 0x80000000ULL

// Tope de vCPU de kling-hv. Hypervisor.framework da 64 en el M4
// (hv_vm_get_max_vcpu_count) y el GIC reserva redistribuidores para 256;
// khv_max_vcpus lo baja a lo que diga el framework en este Mac. El kernel de
// kindling se compila con CONFIG_NR_CPUS=32: con más, el invitado ve 32.
#define MAX_CPUS 64
// MPIDR (afinidad) del vCPU i: grupos de 8 en Aff0 y el grupo en Aff1, como
// los clústeres de un SoC. Hasta 8 vCPU es el MPIDR de siempre (Aff0 = i), el
// que llevan los snapshots de antes. Aff0 cabe así en la TargetList de 16 bits
// de ICC_SGI1R_EL1 sin el Range Selector, que el GIC de HVF no anuncia.
#define KHV_CPUS_GRUPO 8
static inline uint64_t khv_mpidr_aff(int i) {
  return ((uint64_t)(i / KHV_CPUS_GRUPO) << 8) | (uint64_t)(i % KHV_CPUS_GRUPO);
}
// khv_mpidr_idx: el índice del vCPU con esa afinidad (la de PSCI CPU_ON y
// AFFINITY_INFO: Aff3 en los bits 39:32, Aff2-0 en 23:0), o -1 si no es de
// ninguno.
static inline int khv_mpidr_idx(uint64_t aff, int n) {
  uint64_t a0 = aff & 0xff, a1 = (aff >> 8) & 0xff;
  if ((aff >> 16) != 0 || a0 >= KHV_CPUS_GRUPO) return -1;  // Aff2 o Aff3, o fuera del grupo
  uint64_t i = a1 * KHV_CPUS_GRUPO + a0;
  return i < (uint64_t)n ? (int)i : -1;
}
// khv_max_vcpus: los vCPU que admite khv_create en este Mac (MAX_CPUS o lo
// que diga Hypervisor.framework, si es menos).
int khv_max_vcpus(void);
#define PAGINA 16384ULL

// ---- API para Go ----

struct khv_disk {
  const char *path;
  int ro;
  int sinc;  // KHV_SINC_*: qué hace un FLUSH del invitado (spec.Drive.SyncMode)
};

// Modos de sincronización de un disco (docs/backend-hv.md): full, F_FULLFSYNC
// en cada FLUSH (por defecto); barrier, F_BARRIERFSYNC (orden, sin vaciar la
// caché del SSD); none, el FLUSH no hace nada (overlay desechable o volumen
// de desarrollo que se puede perder).
enum { KHV_SINC_FULL = 0, KHV_SINC_BARRIER = 1, KHV_SINC_NONE = 2 };

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
  int mtu;         // MTU del enlace (VIRTIO_NET_F_MTU); 0 = sin anunciar (1500)
  int rng;         // virtio-rng
  int vsock;       // virtio-vsock (el agente por AF_VSOCK)
  int balloon;     // virtio-balloon: 0 sin globo, 1 con él, 2 con deflate_on_oom
  int console_fd;  // salida de la UART
  // Virtualización anidada (docs/backend-hv.md): la VM se crea con EL2
  // (hv_vm_config_set_el2_enabled) y el kernel empieza en EL2h, así que puede
  // llevar KVM. Sin snapshot: khv_snapshot la rechaza.
  int nested;
  int nvfs;        // virtio-fs: cuántos, con su tag (lo que monta el invitado)
  const char *vfs_tags[KHV_MAX_VFS];
};

// Bits de pausa: la VM corre solo con todos a cero. El núcleo (PATCH /vm) y el
// regulador de CPU pausan cada uno por su lado.
#define KHV_PAUSA_API 1u
#define KHV_PAUSA_FRENO 2u

const char *khv_error(void);
void khv_error_copia(char *buf, size_t n);  // copia de khv_error (con su cerrojo)
int khv_create(const struct khv_config *c);
// khv_el2_soportado: 1 si este Mac da EL2 a sus VM (M3 o posterior con
// macOS 15 o posterior), 0 si no; no crea nada.
int khv_el2_soportado(void);
bool khv_vm_tocada(void);  // si khv_create llegó a crear la VM (el proceso ya no sirve para otra)
int khv_boot(void);
int khv_restore(const char *mem_path);
int khv_pause(unsigned bit);
int khv_resume(unsigned bit);
int khv_snapshot(const char *mem_out);
int khv_wait(void);  // bloquea hasta que la VM termina: 0 si el invitado se apagó o se reinició
void khv_stop(void);
uint64_t khv_footprint(void);
void khv_cpu_lectura(uint64_t out[4]);  // CPU de los vCPU: total y en P (ns), instrucciones y en P
void khv_tiempos(double *primer_run_ms, double *restaurado_ms);
// Disposición del mem.file (para TestCabeceraKLNGHV): magia, versión y
// tamaños de la cabecera (la actual y la de la versión 1) y del pie.
void khv_formato_snap(uint64_t *magia, uint32_t *version, uint64_t *hdr, uint64_t *hdr_v1, uint64_t *hdr_v2,
                      uint64_t *hdr_v3, uint64_t *pie, uint64_t *cpu, uint32_t *cpus_v4, uint64_t *vfs_cab,
                      uint64_t *vfs);
// Por qué terminó la VM (khv_wait ya volvió): el invitado se apagó
// (SYSTEM_OFF), pidió reiniciarse (SYSTEM_RESET: kling-hv no reinicia en el
// sitio, la VM termina igual pero no es un fallo), la paró el host o falló.
#define KHV_FIN_NADA 0
#define KHV_FIN_APAGADO 1
#define KHV_FIN_REINICIO 2
#define KHV_FIN_PARADA 3
#define KHV_FIN_FALLO 4
int khv_fin_motivo(void);
// khv_fin: lo mismo y, en buf, el motivo con que terminó ("" si sigue): el
// texto del fallo, que khv_error pierde con el siguiente.
int khv_fin(char *buf, size_t n);
// khv_filtrar_id quita SVE y SME de los registros de identificación que ve el
// invitado (issue #183): en el M4 HVF anuncia sve2, svei8mm y svebf16 en
// ID_AA64ZFR0_EL1 sin SVE, y OpenSSL moría con SIGILL al usarlas.
void khv_filtrar_id(uint64_t *pfr0, uint64_t *pfr1, uint64_t *zfr0, uint64_t *smfr0);
// khv_prueba_cpu_on comprueba sin VM el estado que se guarda de un vCPU con
// un CPU_ON pendiente (vm.c): 0 si cuadra, o el número de la comprobación
// que falló.
int khv_prueba_cpu_on(void);
// khv_sysreg_iss: el hv_sys_reg_t del ISS de un MSR/MRS atrapado (EC 0x18).
uint16_t khv_sysreg_iss(uint64_t iss);

// ---- interno ----

double ms_desde_inicio(void);
void fallo(const char *fmt, ...) __attribute__((format(printf, 1, 2)));  // fija khv_error
void khv_log(const char *fmt, ...) __attribute__((format(printf, 1, 2)));  // a la consola, con "kling-hv: "
void fin_vm(int codigo, const char *motivo);  // la VM termina (desde cualquier hilo)

// ---- memoria del invitado ----
extern uint8_t *g_ram;
extern uint64_t g_ram_size;
void *gpa_ptr(uint64_t gpa, uint64_t len);  // NULL si se sale de la RAM
int ram_soltar(uint64_t off, uint64_t len);   // devuelve esas páginas al fichero (globo)
int ram_reparar(uint64_t off, uint64_t len);  // vuelve a dar a HVF lo que ram_soltar no pudo

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
  uint32_t virtio_spi[VIRTIO_SLOTS];
  // VM con EL2: PSCI por SMC (un HVC desde EL2 se queda en el propio
  // invitado) y el GIC anuncia su interrupción de mantenimiento (PPI 9).
  int nested;
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

// Supresión de avisos (virtio 1.x, 2.7.7 y 2.7.10): VIRTIO_RING_F_EVENT_IDX,
// que ofrecen la red y los discos. vq_interrumpir dice si hay que interrumpir
// al driver tras pasar el used de viejo a nuevo (con EVENT_IDX, según su
// used_event; sin él, según VRING_AVAIL_F_NO_INTERRUPT). vq_pedir_aviso, con
// EVENT_IDX, le pide al driver que avise (notify) en cuanto añada algo más
// allá de lo consumido, y devuelve si ya lo había (hay que volver a mirar
// antes de esperar el aviso). Sin EVENT_IDX no hace nada y devuelve false.
#define VIRTIO_RING_F_EVENT_IDX 29
// khv_event_idx: el bit que ofrecen red y discos (0 con KLING_HV_EVENT_IDX=0).
extern uint64_t khv_event_idx;
bool vq_interrumpir(const struct vmmio *m, const struct vq *q, uint16_t *avail, uint16_t viejo, uint16_t nuevo);
bool vq_pedir_aviso(const struct vmmio *m, const struct vq *q, uint16_t *avail, uint8_t *used);
// vq_pedir_aviso_ya: con EVENT_IDX, que el siguiente añadido avise aunque
// queden entradas sin atender (se cortó una tanda por el tope de vueltas).
void vq_pedir_aviso_ya(const struct vmmio *m, const struct vq *q, uint16_t *avail, uint8_t *used);

// Contadores de avisos (pruebas y medidas): notify del invitado e
// interrupciones al invitado, por dispositivo.
enum { KHV_N_NET_TX, KHV_N_NET_RX, KHV_N_NET_IRQ, KHV_N_BLK, KHV_N_BLK_IRQ, KHV_N_MAX };
extern uint64_t khv_avisos[KHV_N_MAX];
void khv_contadores(uint64_t out[KHV_N_MAX]);

// block: una cola, atendida por un hilo propio (fuera del hilo del vCPU)
struct vblk_state {
  struct vmmio m;
  struct vq q;
};
int vblk_init(int idx, const char *ruta, bool ro, int sinc, uint32_t intid);
bool vblk_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size);
void vblk_get(int idx, struct vblk_state *s);
void vblk_reafirmar(int idx);
void vblk_set(int idx, const struct vblk_state *s);
void vblk_quieto(int idx);     // espera a que el hilo del disco no toque la RAM
void vblk_despertar(int idx);  // vuelve a mirar la cola (tras pausar o restaurar)
void vblk_prueba(int idx);     // atiende la cola aquí mismo (pruebas)
void vblk_prueba_modo(int idx, bool ro, int sinc);  // cambia ro y sinc del disco (pruebas)
int64_t vblk_prueba_bloques(int idx);              // st_blocks del fichero del disco (pruebas)

// Pruebas (prueba.c): un invitado hostil contra los dispositivos, sin VM.
int khv_prueba(const uint8_t *datos, size_t n);
int khv_prueba_carrera(int iters);  // QueueNum cambiando a mitad de tanda (pruebas)
int khv_prueba_flush(void);          // estado de un FLUSH al disco de pruebas

// net (tramas por un socket unix SOCK_DGRAM)
struct vnet_state {
  struct vmmio m;
  struct vq q[2];
};
void vnet_init(int fd, const uint8_t mac[6], int mtu, uint32_t intid);
bool vnet_mmio(uint64_t off, bool escribir, uint64_t *val, int size);
void vnet_get(struct vnet_state *s);
void vnet_reafirmar(void);
void vnet_set(const struct vnet_state *s);
void vnet_quieto(void);
void vnet_despertar(void);
int vnet_prueba_rx(int red);  // rx sin hilo sobre el socket red (pruebas, ver net.c)

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
// khv_vsock_prueba_reuso: solo para las pruebas (ver vsock.c).
int khv_vsock_prueba_reuso(int *coincide_fd);
int khv_vsock_prueba_cierre(int medio);

// balloon (virtio-balloon; ver balloon.c)
struct vballoon_state {
  struct vmmio m;
  // inflate, deflate, stats e informe de páginas libres, con los índices que
  // negoció el driver: sin la de estadísticas, el informe es la 2.
  struct vq q[4];
  uint32_t num_pages, actual;  // en marcos de 4 KiB
  // El búfer de estadísticas que el driver dejó y el dispositivo retiene
  // (se le devuelve para pedir otras): su head, y si lo hay.
  uint16_t stats_head;
  uint8_t stats_pend, pad;
};
// La versión 3 del snapshot no tenía la cola del informe de libres:
// khv_restore la lee con esta forma y la convierte (el invitado no la vio).
struct vballoon_state_v3 {
  struct vmmio m;
  struct vq q[3];
  uint32_t num_pages, actual;
  uint16_t stats_head;
  uint8_t stats_pend, pad;
};
// La versión 2 del snapshot no tenía la cola de estadísticas: khv_restore
// la lee con esta forma y la convierte.
struct vballoon_state_v2 {
  struct vmmio m;
  struct vq q[2];
  uint32_t num_pages, actual;
};
// KHV_BALLOON_NSTATS: etiquetas de virtio-balloon (VIRTIO_BALLOON_S_*).
#define KHV_BALLOON_NSTATS 16
// khv_balloon_stats devuelve las últimas estadísticas del invitado (bytes,
// por etiqueta) y el globo actual en marcos, y le pide otras nuevas; si no
// hay ninguna todavía (recién restaurado) o tienen más de un segundo espera
// hasta espera_ms a que lleguen. -1 sin globo o con la VM en pausa; -2 si el driver nunca ha
// dado estadísticas (no negoció la cola).
int khv_balloon_stats(uint64_t out[KHV_BALLOON_NSTATS], uint32_t *actual, int espera_ms);
int vballoon_init(uint32_t intid, bool deflate_on_oom);
bool vballoon_mmio(uint64_t off, bool escribir, uint64_t *val, int size);
void vballoon_objetivo(uint32_t marcos);
uint32_t vballoon_actual(void);
void vballoon_vaciar(void);  // suelta lo pendiente del globo (al pausar)
uint64_t vballoon_sueltas(void);
bool vballoon_inflada(uint64_t pagina);
void vballoon_get(struct vballoon_state *s);
void vballoon_reafirmar(void);
void vballoon_set(const struct vballoon_state *s);
// khv_balloon fija el globo en marcos de 4 KiB; -1 si la VM no tiene globo.
int khv_balloon(uint32_t marcos);
// khv_balloon_cuentas: páginas del host (16 KiB) soltadas ahora por el globo,
// devueltas por el informe de libres (acumulado), tramos soltados, ns que
// costaron y tramos que fallaron (acumulados).
#define KHV_BALLOON_NCUENTAS 5
void khv_balloon_cuentas(uint64_t out[KHV_BALLOON_NCUENTAS]);

// ---- trazas (traza.c) ----
// Intervalos de os_signpost (subsistema dev.kindling.hv, categoría «fases»)
// para las fases del arranque, la restauración y el freeze, e hitos (eventos)
// la primera vez que pasan. Sin nadie trazando (Instruments, xctrace) cuestan
// una lectura de mach_absolute_time por fase. KLING_HV_TRACE=1 escribe además
// en la consola de kling-hv una línea con los tiempos de cada fase.
//
// X(enumerado, nombre del intervalo): el nombre tiene que ser un literal. Las
// del freeze van de KHV_F_PAUSA en adelante (traza.c las separa así).
#define KHV_FASES(X)                          \
  X(KHV_F_VM, "vm+gic")                       \
  X(KHV_F_DISP, "dispositivos")               \
  X(KHV_F_RAM, "ram")                         \
  X(KHV_F_KERNEL, "kernel+fdt")               \
  X(KHV_F_VCPUS, "vcpus")                     \
  X(KHV_F_LEER, "leer-estado")                \
  X(KHV_F_DISP_ESTADO, "estado-dispositivos") \
  X(KHV_F_GIC, "estado-gic")                  \
  X(KHV_F_CPU, "estado-cpu")                  \
  X(KHV_F_PAUSA, "pausa")                     \
  X(KHV_F_SNAP_ESTADO, "snap-estado")         \
  X(KHV_F_SNAP_CLON, "snap-clon")             \
  X(KHV_F_SNAP_PAGINAS, "snap-paginas")       \
  X(KHV_F_SNAP_ESCRIBIR, "snap-cabecera")
#define KHV_HITOS(X)                          \
  X(KHV_H_SOLTAR, "soltar")                   \
  X(KHV_H_PRIMER_RUN, "primer-run")           \
  X(KHV_H_UART, "primer-uart")                \
  X(KHV_H_VIRTIO, "primer-virtio")            \
  X(KHV_H_AGENTE, "agente")
#define KHV_ENUM_(e, s) e,
enum { KHV_FASES(KHV_ENUM_) KHV_F_N };
enum { KHV_HITOS(KHV_ENUM_) KHV_H_N };
#undef KHV_ENUM_
// khv_traza_iniciar: al principio de khv_create (lee KLING_HV_TRACE).
void khv_traza_iniciar(uint64_t t_inicio);
uint64_t khv_traza_empieza(int fase);       // devuelve la marca para khv_traza_acaba
void khv_traza_acaba(int fase, uint64_t t0);
// khv_traza_hito apunta la primera vez que pasa h (las siguientes, una
// lectura). KHV_H_AGENTE escribe el resumen del arranque o la restauración.
extern volatile uint8_t khv_hitos_vistos[KHV_H_N];
void khv_traza_hito_(int h);
static inline void khv_traza_hito(int h) {
  if (__builtin_expect(!khv_hitos_vistos[h], 0)) khv_traza_hito_(h);
}
void khv_traza_restaurando(void);  // el resumen es de una restauración
// Salidas de vCPU caras: con alguien trazando, un evento por cada salida que
// tardó más de KHV_SALIDA_CARA_NS en atenderse.
#define KHV_SALIDA_CARA_NS 50000ULL
bool khv_traza_activa(void);
void khv_traza_salida(int cpu, unsigned ec, uint64_t ipa, uint64_t ns);
// khv_traza_resumen escribe en buf la línea del resumen (la del log); para
// las pruebas y para quien no lea la consola.
void khv_traza_resumen(char *buf, size_t n);
void khv_traza_freeze(void);  // resumen del freeze (con KLING_HV_TRACE=1)
// khv_traza_prueba: un arranque de mentira (fases y hitos seguidos, sin VM)
// desde cero y su resumen en buf. Solo para las pruebas.
void khv_traza_prueba(char *buf, size_t n);

// virtio-fs (ver vfs.c): la cola de alta prioridad y una de peticiones.
#define VFS_NQ 2
#define VFS_TAG_MAX 36
struct vvfs_state {
  struct vmmio m;
  struct vq q[VFS_NQ];
  // Las peticiones que Go tenía leídas sin contestar, por cola y head: al
  // restaurar se le vuelven a dar.
  uint8_t vuelo[VFS_NQ][QMAX / 8];
};
int vvfs_init(int idx, const char *tag, uint32_t intid);
bool vvfs_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size);
void vvfs_get(int idx, struct vvfs_state *s);
void vvfs_reafirmar(int idx);
void vvfs_set(int idx, const struct vvfs_state *s);
void vvfs_quieto(int idx);
void vvfs_despertar(int idx);
void vvfs_prueba(int idx, const uint8_t *resp, size_t nresp);  // atiende las colas aquí (pruebas)
// API para Go. khv_vfs_leer espera una petición FUSE del dispositivo idx y la
// copia a buf (como mucho cap bytes): devuelve su tamaño, o -1 si el
// dispositivo se cerró (khv_vfs_cerrar) o no existe. khv_vfs_responder
// escribe la respuesta (una entera, con su cabecera FUSE) en la cadena de la
// petición que dice su unique: 0 si fue a alguna parte (o era un aviso, que
// se tira), -1 si nadie la esperaba.
int khv_vfs_leer(int idx, void *buf, uint32_t cap);
int khv_vfs_responder(int idx, const void *src, uint32_t len);
void khv_vfs_cerrar(int idx);

// ---- despertar de vCPU en WFI ----
void vcpu_kick_all(void);

// ---- QoS de los hilos (vm.c) ----
// khv_qos_dispositivo pone el hilo que llama (disco, red, vsock) en
// QOS_CLASS_UTILITY. khv_qos_impulso avisa de que el host le manda trabajo al
// invitado (datos por vsock): los vCPU suben a USER_INITIATED en su siguiente
// salida, sin esperar a medir que están ocupados.
void khv_qos_dispositivo(void);
void khv_qos_impulso(void);

#endif
