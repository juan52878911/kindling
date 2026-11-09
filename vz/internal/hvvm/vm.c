//go:build darwin && arm64

// La VM: Hypervisor.framework con el GIC del framework (hv_gic_create), un
// hilo por vCPU, las salidas que hay que atender (MMIO, PSCI por HVC, sysregs,
// WFI), la pausa, el snapshot y la restauración.
//
// La RAM del invitado es un fichero mapeado con mmap(MAP_PRIVATE) y pasado a
// hv_vm_map: perezosa (solo entra lo que el invitado toca), copy-on-write (el
// fichero no cambia) y compartida entre procesos (las páginas limpias son las
// del vnode en la caché unificada). Un arranque en frío mapea un fichero
// disperso recién creado (y ya borrado); una restauración, el fichero de
// memoria del snapshot.
//
// El fichero de memoria de un snapshot es un único fichero, como el mem.file
// de Firecracker:
//
//   [0, ram)      la RAM, página a página (disperso: lo que el invitado no tocó
//                 son huecos)
//   [ram, ...)    estado: cabecera (vCPU, dispositivos) + estado del GIC
//   últimos 32 B  pie: magia, tamaño de la RAM, dónde está el estado
//
// Hacer uno es clonar (APFS, sin copiar datos) el fichero que respalda la RAM
// y escribir encima las páginas que el invitado cambió (mincore), más el
// estado. Restaurarlo es mapearlo: nadie lo escribe después, así que todas las
// réplicas comparten sus páginas limpias.
#include <copyfile.h>
#include <errno.h>
#include <fcntl.h>
#include <libproc.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <stdarg.h>
#include <stddef.h>
#include <string.h>
#include <sys/clonefile.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "khv.h"

// ===================== utilidades =====================

static uint64_t t_inicio;
static mach_timebase_info_data_t tb;
static char errbuf[512];
// errbuf lo escriben hilos distintos (Go en una llamada, un vCPU al terminar
// la VM): err_mu evita que Go lea un mensaje a medio escribir.
static pthread_mutex_t err_mu = PTHREAD_MUTEX_INITIALIZER;

static double abs_a_ms(uint64_t d) { return (double)d * tb.numer / tb.denom / 1e6; }
double ms_desde_inicio(void) { return abs_a_ms(mach_absolute_time() - t_inicio); }

void fallo(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  pthread_mutex_lock(&err_mu);
  vsnprintf(errbuf, sizeof errbuf, fmt, ap);
  pthread_mutex_unlock(&err_mu);
  va_end(ap);
}

const char *khv_error(void) { return errbuf; }

void khv_error_copia(char *buf, size_t n) {
  if (n == 0) return;
  pthread_mutex_lock(&err_mu);
  snprintf(buf, n, "%s", errbuf);
  pthread_mutex_unlock(&err_mu);
}

// sin_recursos: la preparación falló con HV_NO_RESOURCES. Es del Mac entero,
// no del proceso: hay un cupo de VMs de Hypervisor.framework para todo el
// sistema (127 medidas en un M4, compartido con kling-vz), y la VM 128 lo da
// en hv_vm_create. khv_sin_recursos lo dice para que el error sea de
// capacidad y no uno genérico.
static bool sin_recursos;
bool khv_sin_recursos(void) { return sin_recursos; }

static void fallo_hv(const char *que, hv_return_t r) {
  if (r == HV_NO_RESOURCES) {
    sin_recursos = true;
    fallo("%s failed: 0x%x (HV_NO_RESOURCES: this Mac has no room for another Hypervisor.framework VM)", que,
          (unsigned)r);
    return;
  }
  fallo("%s failed: 0x%x", que, (unsigned)r);
}

// TRY: llamada a Hypervisor.framework en la preparación (devuelve -1).
#define TRY(x)                                                \
  do {                                                        \
    hv_return_t _r = (x);                                     \
    if (_r != HV_SUCCESS) {                                   \
      fallo_hv(#x, _r);                                       \
      return -1;                                              \
    }                                                         \
  } while (0)

// khv_prueba_sin_recursos (pruebas, sin VM): un TRY que falla con r; dice si
// quedó marcado como falta de recursos (y lo desmarca).
static int prueba_try(hv_return_t r) {
  TRY(r);
  return 0;
}
bool khv_prueba_sin_recursos(int r) {
  sin_recursos = false;
  prueba_try((hv_return_t)r);
  bool v = sin_recursos;
  sin_recursos = false;
  return v;
}

// VOK: lo mismo en un hilo de vCPU: un fallo termina la VM.
#define VOK(x)                                                \
  do {                                                        \
    hv_return_t _r = (x);                                     \
    if (_r != HV_SUCCESS) {                                   \
      char _m[160];                                           \
      snprintf(_m, sizeof _m, "%s failed: 0x%x", #x, (unsigned)_r); \
      fin_vm(1, _m);                                          \
      pthread_exit(NULL);                                     \
    }                                                         \
  } while (0)

static int log_fd = 2;
static void vlogf_(const char *fmt, va_list ap) {
  char b[512];
  int n = snprintf(b, sizeof b, "kling-hv: ");
  n += vsnprintf(b + n, sizeof b - (size_t)n, fmt, ap);
  if (n > (int)sizeof b - 2) n = (int)sizeof b - 2;
  b[n++] = '\n';
  uart_flush();
  (void)!write(log_fd, b, (size_t)n);
}

static void logf_(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
static void logf_(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  vlogf_(fmt, ap);
  va_end(ap);
}

void khv_log(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  vlogf_(fmt, ap);
  va_end(ap);
}

uint8_t *g_ram;
uint64_t g_ram_size;
static int base_fd = -1;  // fichero que respalda la RAM
volatile bool g_pausa = true;

void *gpa_ptr(uint64_t gpa, uint64_t len) {
  if (gpa < RAM_BASE || len > g_ram_size || gpa - RAM_BASE > g_ram_size - len) return NULL;
  return g_ram + (gpa - RAM_BASE);
}

// ===================== estado guardable =====================

// Registros de sistema EL0/EL1 que se guardan (sin ID_*, MIDR ni EL2).
static const hv_sys_reg_t SYSREGS[] = {
    HV_SYS_REG_MDSCR_EL1,   HV_SYS_REG_MDCCINT_EL1,  HV_SYS_REG_MPIDR_EL1,    HV_SYS_REG_SCTLR_EL1,
    HV_SYS_REG_ACTLR_EL1,   HV_SYS_REG_CPACR_EL1,    HV_SYS_REG_TTBR0_EL1,    HV_SYS_REG_TTBR1_EL1,
    HV_SYS_REG_TCR_EL1,     HV_SYS_REG_APIAKEYLO_EL1, HV_SYS_REG_APIAKEYHI_EL1, HV_SYS_REG_APIBKEYLO_EL1,
    HV_SYS_REG_APIBKEYHI_EL1, HV_SYS_REG_APDAKEYLO_EL1, HV_SYS_REG_APDAKEYHI_EL1, HV_SYS_REG_APDBKEYLO_EL1,
    HV_SYS_REG_APDBKEYHI_EL1, HV_SYS_REG_APGAKEYLO_EL1, HV_SYS_REG_APGAKEYHI_EL1, HV_SYS_REG_SPSR_EL1,
    HV_SYS_REG_ELR_EL1,     HV_SYS_REG_SP_EL0,       HV_SYS_REG_AFSR0_EL1,    HV_SYS_REG_AFSR1_EL1,
    HV_SYS_REG_ESR_EL1,     HV_SYS_REG_FAR_EL1,      HV_SYS_REG_PAR_EL1,      HV_SYS_REG_MAIR_EL1,
    HV_SYS_REG_AMAIR_EL1,   HV_SYS_REG_VBAR_EL1,     HV_SYS_REG_CONTEXTIDR_EL1, HV_SYS_REG_TPIDR_EL1,
    HV_SYS_REG_SCXTNUM_EL1, HV_SYS_REG_CNTKCTL_EL1,  HV_SYS_REG_CSSELR_EL1,   HV_SYS_REG_TPIDR_EL0,
    HV_SYS_REG_TPIDRRO_EL0, HV_SYS_REG_SCXTNUM_EL0,  HV_SYS_REG_CNTV_CTL_EL0, HV_SYS_REG_CNTV_CVAL_EL0,
    HV_SYS_REG_SP_EL1,      HV_SYS_REG_CNTP_CTL_EL0, HV_SYS_REG_CNTP_CVAL_EL0,
};
#define NSYS (sizeof SYSREGS / sizeof SYSREGS[0])

static const hv_gic_icc_reg_t ICCREGS[] = {
    HV_GIC_ICC_REG_PMR_EL1,  HV_GIC_ICC_REG_BPR0_EL1, HV_GIC_ICC_REG_AP0R0_EL1, HV_GIC_ICC_REG_AP1R0_EL1,
    HV_GIC_ICC_REG_BPR1_EL1, HV_GIC_ICC_REG_CTLR_EL1, HV_GIC_ICC_REG_SRE_EL1,   HV_GIC_ICC_REG_IGRPEN0_EL1,
    HV_GIC_ICC_REG_IGRPEN1_EL1,
};
#define NICC (sizeof ICCREGS / sizeof ICCREGS[0])

// Registros EL2 de una VM anidada que un CPU_ON vuelve a poner como al crear
// el vCPU (ver reiniciar_sysregs): los que un kernel en EL2 deja a su gusto
// (MMU de EL2, vectores, HCR) y que el siguiente encendido no debe heredar.
// No van en el snapshot: una VM anidada no se congela (khv_snapshot).
static const hv_sys_reg_t SYSREGS_EL2[] = {
    HV_SYS_REG_SCTLR_EL2, HV_SYS_REG_HCR_EL2,   HV_SYS_REG_CPTR_EL2,  HV_SYS_REG_MDCR_EL2,
    HV_SYS_REG_TCR_EL2,   HV_SYS_REG_TTBR0_EL2, HV_SYS_REG_TTBR1_EL2, HV_SYS_REG_MAIR_EL2,
    HV_SYS_REG_VBAR_EL2,  HV_SYS_REG_CNTHCTL_EL2, HV_SYS_REG_CNTHP_CTL_EL2, HV_SYS_REG_VTTBR_EL2,
    HV_SYS_REG_VTCR_EL2,
};
#define NSYS_EL2 (sizeof SYSREGS_EL2 / sizeof SYSREGS_EL2[0])

struct cpu_state {
  uint64_t x[31], pc, cpsr, fpcr, fpsr;
  hv_simd_fp_uchar16_t q[32];
  uint64_t sys[NSYS];
  uint8_t sys_ok[NSYS];
  uint64_t icc[NICC];
  uint8_t icc_ok[NICC];
  uint64_t vtimer_offset;
  uint8_t vtimer_masked;
  uint8_t on;
};

#define SNAP_MAGIC 0x4b4c4e4748563032ULL  // "KLNGHV02"
// 2 añade vsock y el globo al final de la cabecera; 3 le da al globo su cola
// de estadísticas (struct vballoon_state crece), 4 la del informe de páginas
// libres. Una cabecera de la versión 1 se lee como un prefijo; una de la 2 o
// la 3, igual salvo el globo, que se convierte (vballoon_state_v2 y _v3).
//
// La 5 es la 4 con los vCPU que no caben en cpu[] (más de SNAP_CPUS_V4)
// detrás, solo los que tiene la máquina: hdr_len = SNAP_HDR_V5(ncpus). Una
// máquina de hasta SNAP_CPUS_V4 vCPU se sigue guardando como 4, byte a byte
// igual que antes, y la lee un kling-hv anterior (kling upgrade y su vuelta
// atrás miran la versión de cada fichero).
//
// La 6 es la 4 (o la 5, con más de SNAP_CPUS_V4 vCPU) con la sección de
// virtio-fs detrás (struct snap_vfs, abajo). Solo la escribe una máquina con
// carpetas por virtio-fs: las demás siguen siendo 4 o 5, igual que antes.
//
// La 7 es la 6 con la sección de virtio-fs siempre, aunque no haya carpetas
// (nvfs = 0), y la escribe solo una máquina cuyo invitado aceptó
// VIRTIO_NET_F_MRG_RXBUF (ver rx_una_mrg en net.c). Un kling-hv anterior
// pondría cada trama en un solo búfer de rx, y con los pequeños que pone el
// invitado tiraría las grandes: la red quedaría a medias sin decir nada. Con
// la 7 la rechaza y `kling upgrade -rollback` lo ve antes de volver atrás.
#define SNAP_VERSION KHV_SNAP_VERSION
#define SNAP_CPUS_V4 8
struct snap_hdr {
  uint64_t magic;
  uint32_t version, ncpus, ndisks;
  // flags va en el relleno que había delante de ram_size: los ficheros de
  // antes lo tienen a cero (la cabecera se escribe tras un memset) y un
  // kling-hv anterior no lo mira.
  uint32_t flags;
  uint64_t ram_size;
  uint32_t uart_intid, rtc_intid, virtio_intid[VIRTIO_MAX];
  uint8_t disk_ro[KHV_MAX_DISKS];
  uint64_t t_pausa;  // mach_absolute_time() al pausar
  struct uart_state uart;
  struct rtc_state rtc;
  struct vblk_state vblk[KHV_MAX_DISKS];
  uint8_t hay_red, mac[6];
  struct vnet_state vnet;
  uint8_t hay_rng;
  struct vrng_state vrng;
  struct cpu_state cpu[SNAP_CPUS_V4];
  uint64_t gic_size;
  // versión 2
  uint8_t hay_vsock;
  struct vvsock_state vsock;
  uint8_t hay_globo;
  struct vballoon_state globo;
  // versión 5: del vCPU SNAP_CPUS_V4 en adelante (en el fichero, solo los
  // que hay)
  struct cpu_state cpu_mas[MAX_CPUS - SNAP_CPUS_V4];
};
#define SNAP_HDR_V1 offsetof(struct snap_hdr, hay_vsock)
_Static_assert(offsetof(struct snap_hdr, ram_size) == 24, "flags must stay in the old padding");
// SNAP_F_USED_IDX: cada struct vq de la cabecera lleva su used_idx (en su
// relleno de antes, ver khv.h). Sin él (ficheros de antes de que el
// dispositivo llevara la cuenta), used_idx se toma del anillo used de la
// RAM del snapshot, que es lo que el dispositivo había publicado. Como la
// forma no cambia, la versión tampoco: un kling-hv anterior lee el fichero
// igual (y toma used.idx de la RAM, como hacía).
#define SNAP_F_USED_IDX 1u
// La 4 acaba donde empieza cpu_mas: los dos van alineados a 16 (cpu_state),
// y es el sizeof(struct snap_hdr) que tenía (lo comprueba el _Static_assert).
#define SNAP_HDR_V4 offsetof(struct snap_hdr, cpu_mas)
#define SNAP_HDR_V5(n) (SNAP_HDR_V4 + (size_t)((n) - SNAP_CPUS_V4) * sizeof(struct cpu_state))
_Static_assert(MAX_CPUS > SNAP_CPUS_V4, "cpu_mas would be empty");

// La versión 2 era sizeof(struct snap_hdr) con el globo de entonces: el
// final redondeado a la alineación de la cabecera (16, por los registros
// SIMD de cpu_state).
#define SNAP_HDR_GLOBO(t)                                                                    \
  ((offsetof(struct snap_hdr, globo) + sizeof(t) + _Alignof(struct snap_hdr) - 1) & \
   ~(_Alignof(struct snap_hdr) - 1))
#define SNAP_HDR_V2 SNAP_HDR_GLOBO(struct vballoon_state_v2)
#define SNAP_HDR_V3 SNAP_HDR_GLOBO(struct vballoon_state_v3)
_Static_assert(SNAP_HDR_V4 == SNAP_HDR_GLOBO(struct vballoon_state), "v4 changed");

// hdr_cpu: el estado del vCPU i en la cabecera (cpu[] o cpu_mas[]).
static struct cpu_state *hdr_cpu(struct snap_hdr *h, int i) {
  return i < SNAP_CPUS_V4 ? &h->cpu[i] : &h->cpu_mas[i - SNAP_CPUS_V4];
}

// snap_hdr_len: los bytes de cabecera de un fichero de versión v con n vCPU
// (0 si esa combinación no existe). Cada versión con su tamaño y ningún otro:
// una v3 con el hdr_len de la 2 haría leer el globo de la v2 con la forma de
// la v3 (y con el de la 1, ceros donde van el vsock y el globo); una de hasta
// la 4 con más de SNAP_CPUS_V4 vCPU dejaría a los demás a cero.
static size_t snap_hdr_len(uint32_t v, uint32_t n) {
  if (n > MAX_CPUS) return 0;  // n = 0 pasa: lo rechaza la comparación con la máquina
  if (v == 5) return n > SNAP_CPUS_V4 ? SNAP_HDR_V5(n) : 0;
  // La 6: la cabecera de la 4 o de la 5, según los vCPU; la sección de
  // virtio-fs va aparte (SNAP_VFS_LEN).
  if (v == 6 || v == 7) return n > SNAP_CPUS_V4 ? SNAP_HDR_V5(n) : SNAP_HDR_V4;
  if (n > SNAP_CPUS_V4) return 0;
  switch (v) {
    case 1: return SNAP_HDR_V1;
    case 2: return SNAP_HDR_V2;
    case 3: return SNAP_HDR_V3;
    case 4: return SNAP_HDR_V4;
  }
  return 0;
}

// versión 6: virtio-fs. Va detrás de la cabecera de la 4 (hasta SNAP_CPUS_V4
// vCPU) o de la 5 (con los demás vCPU), así que se combina con las dos: el
// número de dispositivos y el estado de cada uno, solo los que hay
// (hdr_len = snap_hdr_len(6, ncpus) + SNAP_VFS_LEN(nvfs)).
struct snap_vfs {
  uint32_t nvfs, pad[3];
  struct vvfs_state vfs[KHV_MAX_VFS];
};
#define SNAP_VFS_CAB offsetof(struct snap_vfs, vfs)
#define SNAP_VFS_LEN(n) (SNAP_VFS_CAB + (size_t)(n) * sizeof(struct vvfs_state))

// snap_forma dice si len es el hdr_len de alguna versión con algún número de
// vCPU y de dispositivos virtio-fs (antes de leer la versión: lo que no tiene
// ninguna forma no es de kling-hv). La que le toca a cada versión la mira
// después snap_hdr_len.
static bool snap_forma(uint64_t len) {
  if (len == SNAP_HDR_V1 || len == SNAP_HDR_V2 || len == SNAP_HDR_V3) return true;
  for (int n = SNAP_CPUS_V4; n <= MAX_CPUS; n++) {
    uint64_t base = n == SNAP_CPUS_V4 ? SNAP_HDR_V4 : SNAP_HDR_V5(n);
    if (len == base) return true;
    if (len > base && len - base >= SNAP_VFS_LEN(0) && len - base <= SNAP_VFS_LEN(KHV_MAX_VFS) &&
        (len - base - SNAP_VFS_CAB) % sizeof(struct vvfs_state) == 0)
      return true;
  }
  return false;
}

struct snap_pie {
  uint64_t magic, ram_size, hdr_off, hdr_len;
};

// Tope del estado del GIC que acepta khv_restore: el de una VM real (MAX_CPUS
// redistribuidores; con 10 vCPU ocupa ~123 KiB) cabe con mucha holgura; un
// gic_size mayor es un fichero roto o ajeno, y sin tope sería un malloc del
// tamaño que diga el fichero.
#define SNAP_GIC_MAX (16ULL << 20)

// Lo fija TestCabeceraKLNGHV (cabecera_darwin_test.go): cambiar snap_hdr sin
// subir SNAP_VERSION rompería los mem.file que ya hay en disco.
void khv_formato_snap(uint64_t *magia, uint32_t *version, uint64_t *hdr, uint64_t *hdr_v1, uint64_t *hdr_v2,
                      uint64_t *hdr_v3, uint64_t *pie, uint64_t *cpu, uint32_t *cpus_v4, uint64_t *vfs_cab,
                      uint64_t *vfs) {
  *magia = SNAP_MAGIC;
  *version = SNAP_VERSION;
  *hdr = SNAP_HDR_V4;
  *hdr_v1 = SNAP_HDR_V1;
  *hdr_v2 = SNAP_HDR_V2;
  *hdr_v3 = SNAP_HDR_V3;
  *pie = sizeof(struct snap_pie);
  *cpu = sizeof(struct cpu_state);
  *cpus_v4 = SNAP_CPUS_V4;
  *vfs_cab = SNAP_VFS_CAB;
  *vfs = sizeof(struct vvfs_state);
}

// ===================== configuración =====================

static int ncpus = 1;
static int ndisks;
static char *disk_path[KHV_MAX_DISKS];
static bool disk_ro[KHV_MAX_DISKS];
static bool hay_red, hay_rng, hay_vsock, hay_globo;
static bool anidada;  // VM con EL2 (khv_config.nested)

// Una VM anidada no se congela ni se restaura: el estado de EL2 (sus
// registros, los ICH_* del interfaz de virtualización del GIC, el timer de
// EL2 y lo que HVF guarde de un invitado de segundo nivel en marcha) no va en
// la cabecera v4, y no hay forma de comprobar desde aquí que hv_gic_state lo
// lleve entero. Mejor un error claro que un snapshot que restaura un KVM roto.
#define ERR_SNAP_ANIDADA "snapshots of a nested-virtualization VM are not supported"
static int net_fd = -1;
static uint8_t net_mac[6];
static int net_mtu;
static char *kernel_path, *cmdline, *ram_dir;
static uint32_t uart_intid, rtc_intid, virtio_intid[VIRTIO_SLOTS];
static int slot_net = -1, slot_rng = -1, slot_vsock = -1, slot_globo = -1, nvirtio;
// virtio-fs: nvfs dispositivos en los slots slot_vfs.. (tras los demás).
static int nvfs, slot_vfs = -1;

// ===================== vCPU =====================

struct cpu {
  int idx;
  pthread_t hilo;
  hv_vcpu_t id;
  hv_vcpu_exit_t *exit;
  bool on, on_pend;
  uint64_t on_entry, on_ctx;
  // Los registros de sistema recién creado el vCPU (los de reinicio de HVF,
  // con su MPIDR): lo que un CPU_ON le vuelve a poner. Un vCPU que se apagó
  // (CPU_OFF) conserva su SCTLR, TTBR, TCR y VBAR de la vida anterior; sin
  // esto la entrada física del CPU_ON corría con la MMU encendida y el
  // kernel moría en un Oops.
  struct cpu_state inicial;
  uint64_t inicial_el2[NSYS_EL2];  // lo mismo para los EL2 de una VM anidada
  uint8_t inicial_el2_ok[NSYS_EL2];
  bool kick;
  bool parado;          // en punto_pausa (con mu)
  struct cpu_state st;  // destino del guardado / origen de la restauración
  uint64_t vtimer_offset;
  // Lo que tiene que sumar a su vtimer_offset al salir del punto de pausa (lo
  // que duró una pausa del núcleo), con mu. Ver khv_resume.
  uint64_t ajuste_vtimer;
  uint32_t fallos_ram;  // fallos de etapa 2 seguidos en la RAM (ver ram_reparar_ipa)
};

static struct cpu cpus[MAX_CPUS];
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;
static pthread_mutex_t op_mu = PTHREAD_MUTEX_INITIALIZER;  // serializa pausa, reanudación y snapshot
static unsigned pausa_bits;      // con mu
static volatile bool pausa_pedida;
static volatile bool parar;      // la VM terminó: los hilos de vCPU salen
static int n_pausados, n_creados;
// vCPU parados que ya guardaron su estado en c->st (con mu). Solo lo hace una
// pausa del núcleo (KHV_PAUSA_API), que es la que puede acabar en snapshot:
// las del regulador (KHV_PAUSA_FRENO) se paran sin leer los registros.
static int n_guardados;
static int n_turno;              // vCPU que ya llamaron a hv_vcpu_create (con mu)
static bool arrancar;            // la VM puede empezar a correr
static bool restaurando;         // los hilos cargan cpu_state antes de correr
static bool liberada;            // ya se soltaron los vCPU (con mu)
static uint64_t kernel_entry, fdt_gpa;
static uint64_t nuevo_vtimer_offset;  // calculado al soltar una restauración
static uint64_t snap_t_pausa;         // de la restauración pendiente
static uint64_t t_pausado;            // mach_absolute_time() de la última pausa
// La pausa en curso la pidió (también) el núcleo (KHV_PAUSA_API): al
// reanudar, el contador virtual se adelanta lo que duró y el invitado no ve
// el salto. Las del regulador de CPU (KHV_PAUSA_FRENO) son de milisegundos y
// el reloj sigue, como bajo cpu.max. Con mu.
static bool pausa_reloj;
static double t_primer_run = -1, t_restaurado = -1;

static pthread_mutex_t fin_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t fin_cv = PTHREAD_COND_INITIALIZER;
static bool fin_hecho;
static int fin_codigo;
static int fin_motivo = KHV_FIN_NADA;  // con fin_mu
static char fin_texto[256];            // el motivo tal cual (con fin_mu): errbuf lo pisa el siguiente fallo

void vcpu_kick_all(void) {
  pthread_mutex_lock(&mu);
  for (int i = 0; i < ncpus; i++) cpus[i].kick = true;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
}

static void salir_vcpus(void) {
  hv_vcpu_t ids[MAX_CPUS];
  int n = 0;
  pthread_mutex_lock(&mu);
  for (int i = 0; i < n_creados; i++) ids[n++] = cpus[i].id;
  pthread_mutex_unlock(&mu);
  if (n) hv_vcpus_exit(ids, (uint32_t)n);
}

// Con mu: vuelve a sacar los vCPU que aún no llegaron al punto de pausa.
static void resacar_vcpus_locked(void) {
  hv_vcpu_t ids[MAX_CPUS];
  int n = 0;
  for (int i = 0; i < n_creados; i++)
    if (!cpus[i].parado) ids[n++] = cpus[i].id;
  if (n) hv_vcpus_exit(ids, (uint32_t)n);
}

static void fin_vm_por(int codigo, int por, const char *motivo) {
  pthread_mutex_lock(&fin_mu);
  bool primero = !fin_hecho;
  if (primero) {
    fin_hecho = true;
    fin_codigo = codigo;
    fin_motivo = por;
    snprintf(fin_texto, sizeof fin_texto, "%s", motivo);
    if (codigo != 0) fallo("%s", motivo);
  }
  pthread_cond_broadcast(&fin_cv);
  pthread_mutex_unlock(&fin_mu);
  if (primero) logf_("the VM ends: %s", motivo);
  parar = true;
  pthread_mutex_lock(&mu);
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  salir_vcpus();
  uart_flush();
}

void fin_vm(int codigo, const char *motivo) { fin_vm_por(codigo, codigo ? KHV_FIN_FALLO : KHV_FIN_PARADA, motivo); }

int khv_fin_motivo(void) {
  pthread_mutex_lock(&fin_mu);
  int m = fin_motivo;
  pthread_mutex_unlock(&fin_mu);
  return m;
}

int khv_fin(char *buf, size_t n) {
  pthread_mutex_lock(&fin_mu);
  int m = fin_motivo;
  if (n) snprintf(buf, n, "%s", fin_texto);
  pthread_mutex_unlock(&fin_mu);
  return m;
}

int khv_wait(void) {
  pthread_mutex_lock(&fin_mu);
  while (!fin_hecho) pthread_cond_wait(&fin_cv, &fin_mu);
  int c = fin_codigo;
  pthread_mutex_unlock(&fin_mu);
  return c;
}

void khv_stop(void) { fin_vm_por(0, KHV_FIN_PARADA, "stopped"); }

// guardar_sysregs deja en s solo los registros de sistema de c.
static void guardar_sysregs(struct cpu *c, struct cpu_state *s) {
  for (size_t i = 0; i < NSYS; i++) s->sys_ok[i] = hv_vcpu_get_sys_reg(c->id, SYSREGS[i], &s->sys[i]) == HV_SUCCESS;
}

static void guardar_cpu(struct cpu *c, struct cpu_state *s) {
  for (int i = 0; i < 31; i++) hv_vcpu_get_reg(c->id, HV_REG_X0 + i, &s->x[i]);
  hv_vcpu_get_reg(c->id, HV_REG_PC, &s->pc);
  hv_vcpu_get_reg(c->id, HV_REG_CPSR, &s->cpsr);
  hv_vcpu_get_reg(c->id, HV_REG_FPCR, &s->fpcr);
  hv_vcpu_get_reg(c->id, HV_REG_FPSR, &s->fpsr);
  for (int i = 0; i < 32; i++) hv_vcpu_get_simd_fp_reg(c->id, HV_SIMD_FP_REG_Q0 + i, &s->q[i]);
  guardar_sysregs(c, s);
  for (size_t i = 0; i < NICC; i++) s->icc_ok[i] = hv_gic_get_icc_reg(c->id, ICCREGS[i], &s->icc[i]) == HV_SUCCESS;
  hv_vcpu_get_vtimer_offset(c->id, &s->vtimer_offset);
  bool m = false;
  hv_vcpu_get_vtimer_mask(c->id, &m);
  s->vtimer_masked = m;
  s->on = c->on;
}

static void cargar_cpu(struct cpu *c) {
  struct cpu_state *s = &c->st;
  for (int i = 0; i < 31; i++) VOK(hv_vcpu_set_reg(c->id, HV_REG_X0 + i, s->x[i]));
  VOK(hv_vcpu_set_reg(c->id, HV_REG_PC, s->pc));
  VOK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, s->cpsr));
  VOK(hv_vcpu_set_reg(c->id, HV_REG_FPCR, s->fpcr));
  VOK(hv_vcpu_set_reg(c->id, HV_REG_FPSR, s->fpsr));
  for (int i = 0; i < 32; i++) VOK(hv_vcpu_set_simd_fp_reg(c->id, HV_SIMD_FP_REG_Q0 + i, s->q[i]));
  for (size_t i = 0; i < NSYS; i++)
    if (s->sys_ok[i] && hv_vcpu_set_sys_reg(c->id, SYSREGS[i], s->sys[i]) != HV_SUCCESS)
      logf_("cpu%d: could not restore sysreg %#x", c->idx, SYSREGS[i]);
  for (size_t i = 0; i < NICC; i++)
    if (s->icc_ok[i] && hv_gic_set_icc_reg(c->id, ICCREGS[i], s->icc[i]) != HV_SUCCESS)
      logf_("cpu%d: could not restore ICC %#x", c->idx, ICCREGS[i]);
  VOK(hv_vcpu_set_vtimer_offset(c->id, nuevo_vtimer_offset));
  c->vtimer_offset = nuevo_vtimer_offset;
  VOK(hv_vcpu_set_vtimer_mask(c->id, s->vtimer_masked));
  c->on = s->on;
}

// CPSR de la entrada de un vCPU: EL1h con DAIF enmascarado; en una VM
// anidada, EL2h (el kernel arranca en EL2 y decide él si baja a EL1 o se
// queda con VHE, como en una máquina física).
#define CPSR_ENTRADA 0x3c5
#define CPSR_ENTRADA_EL2 0x3c9
#define CPSR_INICIO (anidada ? CPSR_ENTRADA_EL2 : CPSR_ENTRADA)

// estado_cpu_on deja en d lo que tiene un vCPU justo al entrar por un CPU_ON:
// los registros de sistema de reinicio (ini), PC en la entrada, X0 con el
// contexto y encendido. Lo demás de d (GIC, timer) se queda.
static void estado_cpu_on(struct cpu_state *d, const struct cpu_state *ini, uint64_t entry, uint64_t ctx) {
  memcpy(d->sys, ini->sys, sizeof d->sys);
  memcpy(d->sys_ok, ini->sys_ok, sizeof d->sys_ok);
  memset(d->x, 0, sizeof d->x);
  d->x[0] = ctx;
  d->pc = entry;
  d->cpsr = CPSR_ENTRADA;
  d->on = 1;
}

// reiniciar_sysregs vuelve a poner en el vCPU los registros de sistema que
// tenía al crearse (c->inicial), como un encendido de verdad: SCTLR_EL1 de
// reinicio (MMU y cachés apagadas), TTBR/TCR/MAIR/VBAR/CPACR a cero, el timer
// virtual apagado. Desde el hilo del vCPU.
static void reiniciar_sysregs(struct cpu *c) {
  for (size_t i = 0; i < NSYS; i++)
    if (c->inicial.sys_ok[i] && hv_vcpu_set_sys_reg(c->id, SYSREGS[i], c->inicial.sys[i]) != HV_SUCCESS)
      logf_("cpu%d: could not reset sysreg %#x", c->idx, SYSREGS[i]);
  if (!anidada) return;
  for (size_t i = 0; i < NSYS_EL2; i++)
    if (c->inicial_el2_ok[i] && hv_vcpu_set_sys_reg(c->id, SYSREGS_EL2[i], c->inicial_el2[i]) != HV_SUCCESS)
      logf_("cpu%d: could not reset EL2 sysreg %#x", c->idx, SYSREGS_EL2[i]);
}

// guardar_el2 apunta los EL2 de reinicio de un vCPU recién creado.
static void guardar_el2(struct cpu *c) {
  for (size_t i = 0; i < NSYS_EL2; i++)
    c->inicial_el2_ok[i] = hv_vcpu_get_sys_reg(c->id, SYSREGS_EL2[i], &c->inicial_el2[i]) == HV_SUCCESS;
}

// cpu_guardada deja en d el estado de c que va en el snapshot, con mu. Un
// CPU_ON pendiente va como ya entrado (ver khv_snapshot); devuelve true en
// ese caso. Un ajuste_vtimer pendiente va sumado: c->st es de cuando el hilo
// entró en el punto de pausa y el ajuste de una pausa anterior solo se aplica
// al salir; si llega otra (el núcleo, el regulador) antes de que el hilo
// corra, el snapshot llevaba el offset viejo y el restaurado saltaba lo que
// duró aquella pausa (el soft lockup que el ajuste quería evitar).
static bool cpu_guardada(struct cpu_state *d, const struct cpu *c) {
  *d = c->st;
  d->vtimer_offset += c->ajuste_vtimer;
  if (c->on_pend && !c->on) {
    estado_cpu_on(d, &c->inicial, c->on_entry, c->on_ctx);
    return true;
  }
  return false;
}

// khv_prueba_vtimer_pendiente comprueba sin VM que cpu_guardada suma el
// ajuste pendiente al offset guardado (y no lo pierde con un CPU_ON
// pendiente). 0 si cuadra; si no, el número de la comprobación que falló.
int khv_prueba_vtimer_pendiente(void) {
  static struct cpu c;
  static struct cpu_state d;
  memset(&c, 0, sizeof c);
  c.st.vtimer_offset = 1000;
  c.st.pc = 0x1234;
  c.on = true;
  c.ajuste_vtimer = 250;
  if (cpu_guardada(&d, &c)) return 1;
  if (d.vtimer_offset != 1250 || d.pc != 0x1234) return 2;
  // Lo guardado no se aplica: el hilo lo sigue sumando al salir.
  if (c.ajuste_vtimer != 250 || c.st.vtimer_offset != 1000) return 3;
  c.ajuste_vtimer = 0;
  cpu_guardada(&d, &c);
  if (d.vtimer_offset != 1000) return 4;
  c.on = false;
  c.on_pend = true;
  c.on_entry = 0x80000000;
  c.ajuste_vtimer = 7;
  if (!cpu_guardada(&d, &c)) return 5;
  if (d.vtimer_offset != 1007 || d.pc != 0x80000000) return 6;
  return 0;
}

int khv_prueba_cpu_on(void) {
  static struct cpu_state ini, d;
  memset(&ini, 0, sizeof ini);
  memset(&d, 0xab, sizeof d);
  for (size_t i = 0; i < NSYS; i++) ini.sys[i] = 0x1000 + i, ini.sys_ok[i] = 1;
  d.on = 0;
  estado_cpu_on(&d, &ini, 0x80001234, 0x42);
  if (!d.on) return 1;
  if (d.pc != 0x80001234 || d.x[0] != 0x42 || d.cpsr != CPSR_ENTRADA) return 2;
  for (int i = 1; i < 31; i++)
    if (d.x[i] != 0) return 3;
  for (size_t i = 0; i < NSYS; i++)
    if (d.sys[i] != 0x1000 + i || !d.sys_ok[i]) return 4;
  // Lo que no es del CPU_ON (el GIC, el timer) se queda como estaba.
  if (d.icc[0] != 0xababababababababULL || d.vtimer_offset != 0xababababababababULL) return 5;
  return 0;
}

// Campos de los registros de identificación (Arm ARM, D19.2).
#define ID_PFR0_SVE (0xfULL << 32)
#define ID_PFR1_SME (0xfULL << 24)

void khv_filtrar_id(uint64_t *pfr0, uint64_t *pfr1, uint64_t *zfr0, uint64_t *smfr0) {
  *pfr0 &= ~ID_PFR0_SVE;
  *pfr1 &= ~ID_PFR1_SME;
  *zfr0 = 0;
  *smfr0 = 0;
}

// filtrar_id aplica khv_filtrar_id al vCPU recién creado. Pasa en el arranque
// en frío y en la restauración (los ID_* no van en el snapshot: cada vCPU
// nuevo los lee de HVF y los filtra). Un fallo no es fatal: el invitado vería
// lo de antes del arreglo.
static void filtrar_id(struct cpu *c) {
  uint64_t pfr0 = 0, pfr1 = 0, zfr0 = 0, smfr0 = 0;
  bool con_zfr = false;
  if (__builtin_available(macOS 15.2, *)) con_zfr = true;
  if (hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_ID_AA64PFR0_EL1, &pfr0) != HV_SUCCESS ||
      hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_ID_AA64PFR1_EL1, &pfr1) != HV_SUCCESS) {
    logf_("cpu%d: could not read ID_AA64PFR*_EL1", c->idx);
    return;
  }
  khv_filtrar_id(&pfr0, &pfr1, &zfr0, &smfr0);
  if (hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_ID_AA64PFR0_EL1, pfr0) != HV_SUCCESS ||
      hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_ID_AA64PFR1_EL1, pfr1) != HV_SUCCESS)
    logf_("cpu%d: could not filter ID_AA64PFR*_EL1", c->idx);
  if (con_zfr) {
    if (__builtin_available(macOS 15.2, *)) {
      if (hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_ID_AA64ZFR0_EL1, zfr0) != HV_SUCCESS ||
          hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_ID_AA64SMFR0_EL1, smfr0) != HV_SUCCESS)
        logf_("cpu%d: could not clear ID_AA64ZFR0/SMFR0_EL1", c->idx);
    }
  }
}

// Punto seguro: el hilo del vCPU espera a que se reanude. Su estado (c->st)
// solo lo guarda si la pausa es (o pasa a ser) del núcleo: el regulador de
// CPU pausa cada pocos ms y no lo necesita. HVF solo deja leer los registros
// desde el hilo del vCPU, así que una pausa del regulador que el núcleo hace
// suya lo despierta para que guarde (ver khv_pause). Sin correr entre medias
// lo guardado sigue valiendo aunque la pausa del núcleo acabe y vuelva.
static void punto_pausa(struct cpu *c) {
  pthread_mutex_lock(&mu);
  n_pausados++;
  c->parado = true;
  pthread_cond_broadcast(&cv);
  bool guardado = false;
  while (pausa_pedida && !parar) {
    if (!guardado && (pausa_bits & KHV_PAUSA_API)) {
      pthread_mutex_unlock(&mu);
      guardar_cpu(c, &c->st);
      pthread_mutex_lock(&mu);
      guardado = true;
      n_guardados++;
      pthread_cond_broadcast(&cv);
      continue;
    }
    pthread_cond_wait(&cv, &mu);
  }
  if (guardado) n_guardados--;
  c->parado = false;
  n_pausados--;
  uint64_t aj = c->ajuste_vtimer;
  c->ajuste_vtimer = 0;
  pthread_mutex_unlock(&mu);
  // CNTVCT = mach_absolute_time - offset: subir el offset lo que duró la
  // pausa deja el contador donde estaba al pausar. Sin esto, tras una pausa
  // de más de ~20 s el invitado veía saltar el tiempo y daba soft lockups y
  // RCU stalls ("CPU#1 stuck for 319s"). Lo hace el hilo del vCPU: HVF solo
  // deja tocar el offset desde el suyo.
  if (aj && !parar) {
    uint64_t off = c->vtimer_offset;
    if (hv_vcpu_get_vtimer_offset(c->id, &off) != HV_SUCCESS) off = c->vtimer_offset;
    c->vtimer_offset = off + aj;
    VOK(hv_vcpu_set_vtimer_offset(c->id, c->vtimer_offset));
  }
}

static void set_x(struct cpu *c, unsigned r, uint64_t v) {
  if (r < 31) VOK(hv_vcpu_set_reg(c->id, HV_REG_X0 + r, v));
}
static uint64_t get_x(struct cpu *c, unsigned r) {
  uint64_t v = 0;
  if (r < 31) VOK(hv_vcpu_get_reg(c->id, HV_REG_X0 + r, &v));
  return v;
}
static void avanzar_pc(struct cpu *c) {
  uint64_t pc;
  VOK(hv_vcpu_get_reg(c->id, HV_REG_PC, &pc));
  VOK(hv_vcpu_set_reg(c->id, HV_REG_PC, pc + 4));
}

static void mmio(struct cpu *c, uint64_t ipa, bool w, unsigned srt, int size, bool sse) {
  uint64_t v = w ? get_x(c, srt) : 0;
  if (w && size < 8) v &= (1ULL << (size * 8)) - 1;
  if (ipa >= UART_BASE && ipa < UART_BASE + UART_SIZE) {
    if (w) khv_traza_hito(KHV_H_UART);
    uart_mmio(ipa - UART_BASE, w, &v);
  } else if (ipa >= RTC_BASE && ipa < RTC_BASE + RTC_SIZE) {
    rtc_mmio(ipa - RTC_BASE, w, &v, size);
  } else if (ipa >= VIRTIO_BASE && ipa < VIRTIO_BASE + VIRTIO_SLOTS * VIRTIO_STRIDE) {
    int slot = (int)((ipa - VIRTIO_BASE) / VIRTIO_STRIDE);
    uint64_t off = (ipa - VIRTIO_BASE) % VIRTIO_STRIDE;
    khv_traza_hito(KHV_H_VIRTIO);
    if (slot < ndisks) vblk_mmio(slot, off, w, &v, size);
    else if (slot == slot_net) vnet_mmio(off, w, &v, size);
    else if (slot == slot_rng) vrng_mmio(off, w, &v, size);
    else if (slot == slot_vsock) vsock_mmio(off, w, &v, size);
    else if (slot == slot_globo) vballoon_mmio(off, w, &v, size);
    else if (nvfs > 0 && slot >= slot_vfs && slot < slot_vfs + nvfs) vvfs_mmio(slot - slot_vfs, off, w, &v, size);
    else v = 0;
  } else {
    static int avisos;
    if (avisos++ < 10) logf_("cpu%d: MMIO %s at %#llx with no device", c->idx, w ? "write" : "read", ipa);
    v = 0;
  }
  if (!w) {
    if (sse && size < 8) {
      int b = size * 8;
      v = (uint64_t)(((int64_t)(v << (64 - b))) >> (64 - b));
    }
    set_x(c, srt, v);
  }
}

#define PSCI_RET_NOT_SUPPORTED ((uint64_t)-1)
#define PSCI_RET_INVALID ((uint64_t)-2)
#define PSCI_RET_ALREADY_ON ((uint64_t)-4)

// Lo que dice psci: seguir, o la VM termina porque el invitado se apagó o
// pidió reiniciarse.
#define PSCI_SEGUIR 0
#define PSCI_APAGAR 1
#define PSCI_REINICIAR 2

static int psci(struct cpu *c) {
  uint64_t fn = get_x(c, 0), r = PSCI_RET_NOT_SUPPORTED;
  switch ((uint32_t)fn) {
    case 0x84000000: r = 0x00010000; break;  // PSCI_VERSION 1.0
    case 0x84000001:
    case 0xC4000001: r = 0; break;  // CPU_SUSPEND: como un WFI que ya despertó
    case 0x84000002:                // CPU_OFF
      pthread_mutex_lock(&mu);
      c->on = false;
      pthread_mutex_unlock(&mu);
      return PSCI_SEGUIR;
    case 0x84000003:
    case 0xC4000003: {  // CPU_ON
      int t = khv_mpidr_idx(get_x(c, 1), ncpus);
      if (t < 0) {
        r = PSCI_RET_INVALID;
        break;
      }
      // Los registros se leen antes de tomar mu: VOK sale del hilo si HVF
      // falla, y hacerlo con mu cogido dejaría pause/stop esperando siempre.
      uint64_t entry = get_x(c, 2), ctx = get_x(c, 3);
      pthread_mutex_lock(&mu);
      if (cpus[t].on || cpus[t].on_pend) r = PSCI_RET_ALREADY_ON;
      else {
        cpus[t].on_entry = entry;
        cpus[t].on_ctx = ctx;
        cpus[t].on_pend = true;
        pthread_cond_broadcast(&cv);
        r = 0;
      }
      pthread_mutex_unlock(&mu);
      break;
    }
    case 0x84000004:
    case 0xC4000004: {  // AFFINITY_INFO
      int t = khv_mpidr_idx(get_x(c, 1), ncpus);
      r = t >= 0 ? (cpus[t].on ? 0 : 1) : PSCI_RET_INVALID;
      break;
    }
    case 0x84000006: r = 2; break;  // MIGRATE_INFO_TYPE: no hace falta migrar
    case 0x84000008: return PSCI_APAGAR;  // SYSTEM_OFF
    // SYSTEM_RESET (reboot=k, panic=1): reiniciar en el sitio pediría volver a
    // cargar el kernel y rehacer los dispositivos; la VM termina, pero con su
    // motivo (KHV_FIN_REINICIO), no como un fallo.
    case 0x84000009: return PSCI_REINICIAR;
    case 0x8400000A: {  // PSCI_FEATURES
      uint32_t f = (uint32_t)get_x(c, 1);
      switch (f) {
        case 0x84000000: case 0x84000002: case 0x84000003: case 0xC4000003: case 0x84000004:
        case 0xC4000004: case 0x84000006: case 0x84000008: case 0x84000009: case 0x8400000A:
          r = 0;
          break;
      }
      break;
    }
  }
  set_x(c, 0, r);
  return PSCI_SEGUIR;
}

// fin_psci termina la VM por lo que devolvió psci (APAGAR o REINICIAR).
static void fin_psci(int p) {
  if (p == PSCI_REINICIAR) fin_vm_por(0, KHV_FIN_REINICIO, "the guest rebooted (SYSTEM_RESET)");
  else fin_vm_por(0, KHV_FIN_APAGADO, "the guest powered off");
}

// ===================== QoS =====================
//
// En macOS la QoS del hilo es la única palanca sobre el tipo de núcleo: no se
// puede fijar un hilo a un núcleo P o E. Los vCPU van de base en
// QOS_CLASS_UTILITY, para que sus despertares de reposo (el tick del invitado,
// un paquete suelto) puedan ir a los núcleos E, que gastan menos, y suben a
// QOS_CLASS_USER_INITIATED (núcleos P, como una app en primer plano) con un
// override (pthread_override_qos_class_start_np) mientras tienen trabajo.
// Nunca BACKGROUND: en E un vCPU frenaba ×12-15 bajo launchd
// (footprint/coste.go); eso solo lo pone el modo batería del daemon, a todo el
// proceso, con la máquina ociosa y sin peticiones en vuelo
// (internal/machine/bateria.go).
//
// Trabajo es: (1) lo que el host le manda al invitado (datos por vsock, tramas
// de red: un exec, ficheros, una petición a un puerto reenviado), que sube los
// vCPU en el acto, antes de que el invitado lo vea (khv_qos_impulso, desde los
// hilos de vsock y de red); (2) un vCPU que gasta más de la mitad de un núcleo
// por su cuenta (un cron, un proceso que ya corría), que el vigilante ve en su
// vuelta lenta. Un vCPU que corre en el invitado no sale a espacio de usuario
// en cientos de ms (el timer y el WFI los resuelve el framework), así que no
// puede medirse a sí mismo: lo mide el hilo vigilante con thread_info, cada
// QOS_RAPIDA mientras están arriba. En reposo el vigilante no mira el reloj:
// lo despiertan los tics del invitado. Un vCPU ocupado tiene el tic del
// kernel (HZ 250; NO_HZ_IDLE solo lo para en reposo) y cada vencimiento del
// timer virtual sale a espacio de usuario (HV_EXIT_REASON_VTIMER_ACTIVATED):
// QOS_TICS salidas abajo (0,2 s de un vCPU ocupado; uno ocioso da ~0,7/s)
// abren una ventana de QOS_RAPIDA en la que se mide. QOS_LENTA es solo la red
// por si eso fallara: antes era la vuelta fija de 2 s, medio despertar por
// segundo por VM ociosa. Baja tras dos medidas seguidas por debajo de una
// décima de núcleo. Arranca arriba: el arranque y una restauración quieren
// los núcleos P. KLING_HV_QOS=0 lo apaga todo (los hilos quedan con la QoS de
// siempre).
#define QOS_RAPIDA_MS 200
#define QOS_LENTA_MS 60000
#define QOS_TICS 50
static bool qos_activa;
static volatile bool qos_alta;  // con overrides puestos; se lee sin qos_mu en el impulso
static pthread_override_t qos_ov[MAX_CPUS];
static pthread_mutex_t qos_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t qos_cv = PTHREAD_COND_INITIALIZER;
static int qos_n;  // vCPU que vigila
static uint32_t qos_tics;    // salidas del timer virtual abajo desde la última vuelta (atómico)
static uint32_t qos_vueltas; // vueltas del vigilante (pruebas)

void khv_qos_dispositivo(void) {
  if (qos_activa) pthread_set_qos_class_self_np(QOS_CLASS_UTILITY, 0);
}

// Con qos_mu.
static void qos_subir(void) {
  if (qos_alta) return;
  for (int i = 0; i < qos_n; i++)
    qos_ov[i] = pthread_override_qos_class_start_np(cpus[i].hilo, QOS_CLASS_USER_INITIATED, 0);
  qos_alta = true;
  pthread_cond_broadcast(&qos_cv);  // el vigilante pasa a la vuelta rápida
}

static void qos_bajar(void) {
  if (!qos_alta) return;
  for (int i = 0; i < qos_n; i++) {
    if (qos_ov[i]) pthread_override_qos_class_end_np(qos_ov[i]);
    qos_ov[i] = NULL;
  }
  qos_alta = false;
}

void khv_qos_impulso(void) {
  // Ya arriba (lo normal mientras dura una petición): ni un cerrojo.
  if (!qos_activa || qos_alta) return;
  pthread_mutex_lock(&qos_mu);
  if (qos_n > 0) qos_subir();
  pthread_mutex_unlock(&qos_mu);
}

// qos_tic, desde el vCPU en cada salida del timer virtual: con los vCPU abajo,
// la QOS_TICS-ésima despierta al vigilante para que mida. Arriba, nada.
static void qos_tic(void) {
  if (!qos_activa || qos_alta) return;
  if (__atomic_add_fetch(&qos_tics, 1, __ATOMIC_RELAXED) != QOS_TICS) return;
  pthread_mutex_lock(&qos_mu);
  pthread_cond_broadcast(&qos_cv);
  pthread_mutex_unlock(&qos_mu);
}

static uint64_t hilo_cpu_ns(pthread_t h) {
  thread_basic_info_data_t bi;
  mach_msg_type_number_t cnt = THREAD_BASIC_INFO_COUNT;
  if (thread_info(pthread_mach_thread_np(h), THREAD_BASIC_INFO, (thread_info_t)&bi, &cnt) != KERN_SUCCESS) return 0;
  return (uint64_t)(bi.user_time.seconds + bi.system_time.seconds) * 1000000000ULL +
         (uint64_t)(bi.user_time.microseconds + bi.system_time.microseconds) * 1000ULL;
}

static void *qos_vigilante(void *a) {
  (void)a;
  pthread_set_qos_class_self_np(QOS_CLASS_UTILITY, 0);
  uint64_t antes[MAX_CPUS] = {0}, t_antes = clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
  for (int i = 0; i < qos_n; i++) antes[i] = hilo_cpu_ns(cpus[i].hilo);
  int bajas = 0;
  bool midiendo = false;  // ventana abierta por los tics
  pthread_mutex_lock(&qos_mu);
  for (;;) {
    bool estaba = qos_alta;
    if (!estaba && !midiendo) __atomic_store_n(&qos_tics, 0, __ATOMIC_RELAXED);
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    uint64_t fin = (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec +
                   (uint64_t)(estaba || midiendo ? QOS_RAPIDA_MS : QOS_LENTA_MS) * 1000000ULL;
    ts.tv_sec = (time_t)(fin / 1000000000ULL);
    ts.tv_nsec = (long)(fin % 1000000000ULL);
    // Un impulso o los tics en reposo despiertan antes: la ventana empieza ahí.
    int r = pthread_cond_timedwait(&qos_cv, &qos_mu, &ts);
    if (parar) break;
    __atomic_add_fetch(&qos_vueltas, 1, __ATOMIC_RELAXED);
    uint64_t ahora = clock_gettime_nsec_np(CLOCK_UPTIME_RAW), dw = ahora - t_antes, max = 0;
    t_antes = ahora;
    for (int i = 0; i < qos_n; i++) {
      uint64_t c = hilo_cpu_ns(cpus[i].hilo);
      if (c - antes[i] > max) max = c - antes[i];
      antes[i] = c;
    }
    if (r != ETIMEDOUT && !estaba) {
      // Un impulso (ya arriba) o los tics: medir desde aquí. Un despertar
      // espurio con los tics a cero vuelve a la espera larga.
      midiendo = !qos_alta && __atomic_load_n(&qos_tics, __ATOMIC_RELAXED) >= QOS_TICS;
      continue;
    }
    midiendo = false;
    if (qos_alta) {
      bajas = max * 10 < dw ? bajas + 1 : 0;
      if (bajas >= 2) qos_bajar(), bajas = 0;
    } else if (max * 2 > dw) {
      qos_subir();
    }
  }
  qos_bajar();
  pthread_mutex_unlock(&qos_mu);
  return NULL;
}

// qos_empezar pone los vCPU (ya creados) arriba y lanza el vigilante.
static void qos_empezar(void) {
  if (!qos_activa) return;
  pthread_mutex_lock(&qos_mu);
  qos_n = ncpus;
  qos_subir();
  pthread_mutex_unlock(&qos_mu);
  pthread_t t;
  if (pthread_create(&t, NULL, qos_vigilante, NULL) == 0) pthread_detach(t);
}

// khv_prueba_qos (pruebas, sin VM): el vigilante sobre un hilo que hace de
// vCPU. Con ocupado, el hilo gira y da un tic cada 4 ms (HZ 250) y devuelve
// los ms hasta que el vigilante lo sube, o -1 si no lo sube en plazo_ms. Sin
// él, el hilo duerme sin tics y devuelve las vueltas del vigilante en
// plazo_ms (una VM ociosa no debe despertarlo).
static volatile bool prueba_qos_fin;
static void *prueba_qos_vcpu(void *a) {
  bool ocupado = a != NULL;
  uint64_t tic = clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
  while (!prueba_qos_fin) {
    if (!ocupado) {
      usleep(1000);
      continue;
    }
    uint64_t ahora = clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
    if (ahora - tic >= 4000000ULL) qos_tic(), tic = ahora;
  }
  return NULL;
}

int khv_prueba_qos(int ocupado, int plazo_ms) {
  if (qos_n != 0 || cpus[0].hilo != NULL) return -2;  // hay una VM de verdad
  prueba_qos_fin = false;
  pthread_t v, w;
  if (pthread_create(&v, NULL, prueba_qos_vcpu, ocupado ? (void *)1 : NULL) != 0) return -2;
  bool activa = qos_activa;
  pthread_mutex_lock(&qos_mu);
  qos_activa = true;
  cpus[0].hilo = v;
  qos_n = 1;
  qos_alta = false;
  __atomic_store_n(&qos_vueltas, 0, __ATOMIC_RELAXED);
  pthread_mutex_unlock(&qos_mu);
  uint64_t t0 = clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
  if (pthread_create(&w, NULL, qos_vigilante, NULL) != 0) return -2;
  int res = -1;
  while (clock_gettime_nsec_np(CLOCK_UPTIME_RAW) - t0 < (uint64_t)plazo_ms * 1000000ULL) {
    if (ocupado && qos_alta) {
      res = (int)((clock_gettime_nsec_np(CLOCK_UPTIME_RAW) - t0) / 1000000ULL);
      break;
    }
    usleep(5000);
  }
  if (!ocupado) res = (int)__atomic_load_n(&qos_vueltas, __ATOMIC_RELAXED);
  pthread_mutex_lock(&qos_mu);
  parar = true;
  pthread_cond_broadcast(&qos_cv);
  pthread_mutex_unlock(&qos_mu);
  pthread_join(w, NULL);
  prueba_qos_fin = true;
  pthread_join(v, NULL);
  pthread_mutex_lock(&qos_mu);
  parar = false;
  qos_n = 0;
  qos_activa = activa;
  memset(&cpus[0].hilo, 0, sizeof cpus[0].hilo);
  pthread_mutex_unlock(&qos_mu);
  return res;
}

// khv_prueba_qos_lenta_ms: el plazo de la vuelta de reposo (pruebas).
int khv_prueba_qos_lenta_ms(void) { return QOS_LENTA_MS; }

// WFI de respaldo: con el GIC de HVF el framework resuelve el WFI dentro y
// esto no se usa (medido en el prototipo). Si llega, dormir hasta el
// vencimiento del timer virtual o hasta que un dispositivo avise; con varios
// vCPU las IPI no pasan por aquí, así que la espera se acota.
static void wfi(struct cpu *c) {
  uint64_t ctl = 0, cval = 0;
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_CNTV_CTL_EL0, &ctl);
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_CNTV_CVAL_EL0, &cval);
  uint64_t ahora = mach_absolute_time();
  // 2 ms con SMP o con EL2 (el kernel puede estar usando el timer de EL2,
  // CNTHP, que aquí no se mira), 1 s si no.
  uint64_t tope_ns = ncpus > 1 || anidada ? 2000000 : 1000000000;
  if ((ctl & 1) && !(ctl & 2)) {
    uint64_t vence = cval + c->vtimer_offset;
    if (vence <= ahora) return;
    uint64_t ns = (uint64_t)((vence - ahora) * tb.numer / tb.denom);
    if (ns < tope_ns) tope_ns = ns;
  }
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  uint64_t fin = (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec + tope_ns;
  ts.tv_sec = (time_t)(fin / 1000000000ULL);
  ts.tv_nsec = (long)(fin % 1000000000ULL);
  pthread_mutex_lock(&mu);
  if (!c->kick && !pausa_pedida && !parar) pthread_cond_timedwait(&cv, &mu, &ts);
  c->kick = false;
  pthread_mutex_unlock(&mu);
}

static void volcar(struct cpu *c, const char *por) {
  uint64_t pc = 0, cpsr = 0, far = 0, esr = 0;
  hv_vcpu_get_reg(c->id, HV_REG_PC, &pc);
  hv_vcpu_get_reg(c->id, HV_REG_CPSR, &cpsr);
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_FAR_EL1, &far);
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_ESR_EL1, &esr);
  logf_("cpu%d: %s syndrome=%#llx va=%#llx ipa=%#llx pc=%#llx cpsr=%#llx far_el1=%#llx esr_el1=%#llx", c->idx, por,
        c->exit->exception.syndrome, c->exit->exception.virtual_address, c->exit->exception.physical_address, pc, cpsr,
        far, esr);
}

static int ram_reparar_ipa(struct cpu *c, uint64_t ipa);

// khv_sysreg_iss traduce el ISS de un MSR/MRS atrapado (EC 0x18: Op0, Op2,
// Op1, CRn, Rt, CRm, dirección) a la codificación de hv_sys_reg_t (la de
// MRS: Op0:Op1:CRn:CRm:Op2, p. ej. CNTHCTL_EL2 = 0xe708).
uint16_t khv_sysreg_iss(uint64_t iss) {
  unsigned op0 = (iss >> 20) & 3, op2 = (iss >> 17) & 7, op1 = (iss >> 14) & 7, crn = (iss >> 10) & 0xf,
           crm = (iss >> 1) & 0xf;
  return (uint16_t)(op0 << 14 | op1 << 11 | crn << 7 | crm << 3 | op2);
}

// sysreg_el2: en una VM con EL2, HVF devuelve al VMM algunos accesos del
// kernel a registros de EL2 que él mismo lleva (medido en el M4 con macOS
// 26.5: CNTHCTL_EL2, que el kernel escribe en init_el2). Se atienden con
// hv_vcpu_{get,set}_sys_reg, como hace QEMU. true si el registro era de HVF;
// false si HVF no lo conoce (queda como antes: lectura a 0, escritura
// ignorada).
static bool sysreg_el2(struct cpu *c, uint64_t syn) {
  hv_sys_reg_t r = (hv_sys_reg_t)khv_sysreg_iss(syn);
  unsigned rt = (syn >> 5) & 0x1f;
  if (syn & 1) {
    uint64_t v = 0;
    if (hv_vcpu_get_sys_reg(c->id, r, &v) != HV_SUCCESS) return false;
    set_x(c, rt, v);
    return true;
  }
  return hv_vcpu_set_sys_reg(c->id, r, get_x(c, rt)) == HV_SUCCESS;
}

// LOS vCPU SE CREAN EN ORDEN DE ÍNDICE.
//
// hv_gic_state_get_data guarda los redistribuidores por orden de creación
// del vCPU en HVF, y hv_gic_set_state los reparte igual; cpu_state, en
// cambio, va por índice (MPIDR). Con los hilos creando cada uno el suyo a la
// vez, el orden era el que saliera: un snapshot de una VM cuyo cpu1 se creó
// primero, restaurado con el cpu0 primero, le daba a cada vCPU el
// redistribuidor del otro. Casi siempre no se nota (los dos están ociosos),
// pero si el snapshot pilló a un vCPU dentro del manejador del timer (PPI 27
// activa en SU redistribuidor), su EOI llega a uno donde no está activa y HVF
// mata el proceso con SIGTRAP ("FIXME: Handle no active virtual interrupt,
// EOIR1 0x1b"): kling-hv moría a los ~50 ms de restaurar, y el daemon veía un
// EOF en el PUT /kling/forwards. Con el turno, el vCPU i es siempre el i-ésimo
// que se crea, al arrancar y al restaurar.
static void turno_esperar(int idx) {
  pthread_mutex_lock(&mu);
  while (n_turno != idx) pthread_cond_wait(&cv, &mu);
  pthread_mutex_unlock(&mu);
}

// turno_pasar va tras hv_vcpu_create, haya salido bien o no: un fallo no
// puede dejar esperando a los que vienen detrás.
static void turno_pasar(void) {
  pthread_mutex_lock(&mu);
  n_turno++;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
}

// khv_prueba_turno_vcpus comprueba sin VM que el turno ordena la creación:
// hilos que llegan al revés (el último índice primero) pasan por orden. 0 si
// cuadra, o 1 + la posición del primero que se coló.
#define PRUEBA_TURNO_N 8
static int prueba_turno_orden[PRUEBA_TURNO_N], prueba_turno_pos;
static void *prueba_turno_hilo(void *arg) {
  int idx = (int)(intptr_t)arg;
  usleep((useconds_t)(PRUEBA_TURNO_N - idx) * 2000);  // llegan al revés
  turno_esperar(idx);
  prueba_turno_orden[prueba_turno_pos++] = idx;  // solo uno tiene el turno
  turno_pasar();
  return NULL;
}
int khv_prueba_turno_vcpus(void) {
  pthread_t h[PRUEBA_TURNO_N];
  n_turno = 0;
  prueba_turno_pos = 0;
  for (int i = PRUEBA_TURNO_N - 1; i >= 0; i--)
    if (pthread_create(&h[i], NULL, prueba_turno_hilo, (void *)(intptr_t)i) != 0) return 100;
  for (int i = 0; i < PRUEBA_TURNO_N; i++) pthread_join(h[i], NULL);
  n_turno = 0;
  for (int i = 0; i < PRUEBA_TURNO_N; i++)
    if (prueba_turno_orden[i] != i) return 1 + i;
  return 0;
}

static void *vcpu_hilo(void *arg) {
  struct cpu *c = arg;
  khv_qos_dispositivo();  // base UTILITY; los overrides la suben con trabajo (qos_subir)
  turno_esperar(c->idx);
  hv_return_t r = hv_vcpu_create(&c->id, &c->exit, NULL);
  turno_pasar();
  if (r == HV_SUCCESS) r = hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_MPIDR_EL1, khv_mpidr_aff(c->idx) | (1ULL << 31));
  if (r == HV_SUCCESS) {
    filtrar_id(c);
    // De inicial solo se usan los registros de sistema (reiniciar_sysregs,
    // estado_cpu_on): leer el resto era una pasada de más por vCPU en cada
    // restauración.
    guardar_sysregs(c, &c->inicial);
    if (anidada) guardar_el2(c);
  }
  pthread_mutex_lock(&mu);
  if (r != HV_SUCCESS) {
    pthread_mutex_unlock(&mu);
    char m[96];
    snprintf(m, sizeof m, "hv_vcpu_create (cpu%d) failed: 0x%x", c->idx, (unsigned)r);
    fin_vm(1, m);
    pthread_mutex_lock(&mu);
    n_creados++;  // quien espera no debe quedarse colgado
    pthread_cond_broadcast(&cv);
    pthread_mutex_unlock(&mu);
    return NULL;
  }
  n_creados++;
  pthread_cond_broadcast(&cv);
  while (!arrancar && !parar) pthread_cond_wait(&cv, &mu);
  pthread_mutex_unlock(&mu);
  if (parar) return NULL;

  if (restaurando) {
    uint64_t tr = c->idx == 0 ? khv_traza_empieza(KHV_F_CPU) : 0;
    cargar_cpu(c);
    if (c->idx == 0) khv_traza_acaba(KHV_F_CPU, tr);
  } else {
    // Arranque en frío: el kernel empieza en el vCPU 0.
    VOK(hv_vcpu_get_vtimer_offset(c->id, &c->vtimer_offset));
    if (c->idx == 0) {
      VOK(hv_vcpu_set_reg(c->id, HV_REG_PC, kernel_entry));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_X0, fdt_gpa));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, CPSR_INICIO));
      c->on = true;
    }
  }

  for (;;) {
    if (parar) return NULL;
    if (pausa_pedida) {
      punto_pausa(c);
      continue;
    }
    if (!c->on) {
      pthread_mutex_lock(&mu);
      while (!c->on_pend && !pausa_pedida && !parar) pthread_cond_wait(&cv, &mu);
      bool empezar = c->on_pend && !pausa_pedida && !parar;
      if (empezar) {
        c->on_pend = false;
        c->on = true;
      }
      pthread_mutex_unlock(&mu);
      if (!empezar) continue;
      reiniciar_sysregs(c);
      VOK(hv_vcpu_set_reg(c->id, HV_REG_PC, c->on_entry));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_X0, c->on_ctx));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, CPSR_INICIO));
    }
    if (t_primer_run < 0 && c->idx == 0) {
      t_primer_run = ms_desde_inicio();
      khv_traza_hito(KHV_H_PRIMER_RUN);
    }
    VOK(hv_vcpu_run(c->id));
    // Con alguien trazando, lo que cuesta atender la salida (no el WFI, que
    // es esperar): un evento si pasa de KHV_SALIDA_CARA_NS.
    uint64_t t_salida = khv_traza_activa() ? mach_absolute_time() : 0;
    switch (c->exit->reason) {
      case HV_EXIT_REASON_CANCELED: break;
      case HV_EXIT_REASON_VTIMER_ACTIVATED:
        // Con el GIC de HVF el kernel inyecta el PPI 27; basta con desenmascarar.
        hv_vcpu_set_vtimer_mask(c->id, false);
        qos_tic();
        break;
      case HV_EXIT_REASON_EXCEPTION: {
        uint64_t syn = c->exit->exception.syndrome;
        unsigned ec = (syn >> 26) & 0x3f;
        uint64_t ipa = c->exit->exception.physical_address;
        if ((ec == 0x24 || ec == 0x20) && ipa >= RAM_BASE && ipa - RAM_BASE < g_ram_size) {
          // Un fallo de etapa 2 en la RAM: una página que ram_soltar no pudo
          // volver a dar a HVF (o que otro vCPU está cambiando ahora mismo).
          // Se repara y se repite la instrucción, sin avanzar el PC.
          if (ram_reparar_ipa(c, ipa) != 0) {
            volcar(c, "guest RAM fault");
            fin_vm(1, "a guest RAM page could not be mapped again");
            return NULL;
          }
          break;
        }
        c->fallos_ram = 0;
        switch (ec) {
          case 0x24: {  // data abort desde EL inferior: MMIO
            if (!(syn & (1u << 24))) {
              volcar(c, "data abort without ISV");
              fin_vm(1, "guest data abort without ISV");
              return NULL;
            }
            int size = 1 << ((syn >> 22) & 3);
            mmio(c, c->exit->exception.physical_address, (syn >> 6) & 1, (syn >> 16) & 0x1f, size, (syn >> 21) & 1);
            avanzar_pc(c);
            break;
          }
          case 0x16: {  // HVC: PSCI (el PC ya apunta a la siguiente instrucción)
            int p = psci(c);
            if (p != PSCI_SEGUIR) {
              fin_psci(p);
              return NULL;
            }
            break;
          }
          case 0x17: {  // SMC
            int p = psci(c);
            if (p != PSCI_SEGUIR) {
              fin_psci(p);
              return NULL;
            }
            avanzar_pc(c);
            break;
          }
          case 0x18: {  // MSR/MRS atrapado: lecturas a 0, escrituras ignoradas
            unsigned rt = (syn >> 5) & 0x1f;
            if (anidada && sysreg_el2(c, syn)) {
              avanzar_pc(c);
              break;
            }
            static int avisos;
            if (avisos++ < 10) logf_("cpu%d: trapped sysreg iss=%#llx", c->idx, syn & 0x1ffffff);
            if (syn & 1) set_x(c, rt, 0);
            avanzar_pc(c);
            break;
          }
          case 0x01:  // WFI/WFE
            if (!(syn & 1)) wfi(c);
            avanzar_pc(c);
            break;
          default:
            volcar(c, "unhandled exception");
            fin_vm(1, "unhandled guest exception");
            return NULL;
        }
        break;
      }
      default: {
        char m[64];
        snprintf(m, sizeof m, "unknown vCPU exit %u", c->exit->reason);
        fin_vm(1, m);
        return NULL;
      }
    }
    if (t_salida && c->exit->reason == HV_EXIT_REASON_EXCEPTION) {
      uint64_t syn = c->exit->exception.syndrome;
      unsigned ec = (syn >> 26) & 0x3f;
      uint64_t ns = (uint64_t)((__uint128_t)(mach_absolute_time() - t_salida) * tb.numer / tb.denom);
      if (ec != 0x01 && ns > KHV_SALIDA_CARA_NS) khv_traza_salida(c->idx, ec, c->exit->exception.physical_address, ns);
    }
  }
}

static int crear_vcpus(void) {
  for (int i = 0; i < ncpus; i++) {
    cpus[i].idx = i;
    if (pthread_create(&cpus[i].hilo, NULL, vcpu_hilo, &cpus[i]) != 0) {
      fallo("pthread_create: %s", strerror(errno));
      return -1;
    }
    pthread_detach(cpus[i].hilo);
  }
  pthread_mutex_lock(&mu);
  while (n_creados < ncpus) pthread_cond_wait(&cv, &mu);
  pthread_mutex_unlock(&mu);
  if (parar) {
    fallo("%s", errbuf[0] ? errbuf : "creating the vCPUs failed");
    return -1;
  }
  qos_empezar();
  return 0;
}

static void dispositivos_quietos(void) {
  for (int i = 0; i < ndisks; i++) vblk_quieto(i);
  if (hay_red) vnet_quieto();
  if (hay_vsock) vsock_quieto();
  // El globo no tiene hilo propio, pero khv_balloon_stats (desde Go) escribe
  // en la cola de estadísticas, en la RAM del invitado. Mira g_pausa con el
  // cerrojo del globo y lo suelta tras escribir; g_pausa ya está puesta, así
  // que tomar ese cerrojo una vez espera a la que estuviera dentro, y las
  // siguientes ven la pausa y no tocan nada. De paso suelta lo pendiente:
  // una máquina pausada tras un squeeze no retiene esas páginas.
  if (hay_globo) vballoon_vaciar();
  for (int i = 0; i < nvfs; i++) vvfs_quieto(i);
}

static void dispositivos_despertar(void) {
  for (int i = 0; i < ndisks; i++) vblk_despertar(i);
  if (hay_red) vnet_despertar();
  if (hay_vsock) vsock_despertar();
  for (int i = 0; i < nvfs; i++) vvfs_despertar(i);
}

// dispositivos_quietos espera sin plazo (a que un hilo de disco suelte su
// petición, a que la red termine un rx) y khv_pause la llamaba con op_mu
// cogido: un dispositivo atascado dejaba freeze y stop colgados igual que un
// vCPU que no llega a la pausa. La hace un hilo aparte (uno solo, de vida
// larga: el regulador de CPU pausa cada pocos ms y un pthread_create por
// pausa sería caro) y khv_pause la espera con plazo. Si vence, el hilo sigue
// dentro y termina cuando el dispositivo se suelte; la siguiente petición
// espera a que acabe la anterior y vuelve a pasar. Con q_mu.
static pthread_mutex_t q_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t q_cv = PTHREAD_COND_INITIALIZER;
static uint64_t q_pedidas, q_hechas;
static bool q_hilo;

static void *quietos_hilo(void *arg) {
  (void)arg;
  pthread_set_qos_class_self_np(QOS_CLASS_USER_INITIATED, 0);
  pthread_mutex_lock(&q_mu);
  for (;;) {
    while (q_hechas == q_pedidas) pthread_cond_wait(&q_cv, &q_mu);
    uint64_t n = q_pedidas;
    pthread_mutex_unlock(&q_mu);
    dispositivos_quietos();
    pthread_mutex_lock(&q_mu);
    q_hechas = n;
    pthread_cond_broadcast(&q_cv);
  }
  return NULL;
}

// dispositivos_quietos_plazo: true si los dispositivos quedaron quietos antes
// de plazo_ns. Sin hilo (pthread_create falló) lo hace aquí, sin plazo, como
// antes.
static bool dispositivos_quietos_plazo(uint64_t plazo_ns) {
  pthread_mutex_lock(&q_mu);
  if (!q_hilo) {
    pthread_t t;
    if (pthread_create(&t, NULL, quietos_hilo, NULL) != 0) {
      pthread_mutex_unlock(&q_mu);
      dispositivos_quietos();
      return true;
    }
    pthread_detach(t);
    q_hilo = true;
  }
  uint64_t n = ++q_pedidas;
  pthread_cond_broadcast(&q_cv);
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  ts.tv_sec += (time_t)(plazo_ns / 1000000000ULL);
  ts.tv_nsec += (long)(plazo_ns % 1000000000ULL);
  if (ts.tv_nsec >= 1000000000L) ts.tv_sec++, ts.tv_nsec -= 1000000000L;
  while (q_hechas < n)
    if (pthread_cond_timedwait(&q_cv, &q_mu, &ts) == ETIMEDOUT) break;
  bool ok = q_hechas >= n;
  pthread_mutex_unlock(&q_mu);
  return ok;
}

// soltar deja correr la VM por primera vez. Con mu.
static void soltar_locked(void) {
  if (restaurando) {
    // El contador virtual sigue donde se quedó al pausar: CNTVCT =
    // mach_absolute_time - offset. Lo que pasó entre el snapshot y ahora no
    // existe para el invitado (como en Firecracker); el reloj de pared lo
    // arregla /resync del agente.
    uint64_t cnt = snap_t_pausa - cpus[0].st.vtimer_offset;
    nuevo_vtimer_offset = mach_absolute_time() - cnt;
    t_restaurado = ms_desde_inicio();
  }
  pausa_reloj = false;  // el offset de la restauración ya lo tiene en cuenta
  khv_traza_hito(KHV_H_SOLTAR);
  liberada = true;
  arrancar = true;
  g_pausa = false;
  pthread_cond_broadcast(&cv);
}

// ===================== VM =====================

static bool vm_tocada;
bool khv_vm_tocada(void) { return vm_tocada; }

int khv_max_vcpus(void) {
  uint32_t m = 0;
  if (hv_vm_get_max_vcpu_count(&m) != HV_SUCCESS || m == 0) return MAX_CPUS;
  return m < MAX_CPUS ? (int)m : MAX_CPUS;
}

int khv_el2_soportado(void) {
  bool si = false;
  return hv_vm_config_get_el2_supported(&si) == HV_SUCCESS && si;
}

int khv_create(const struct khv_config *c) {
  t_inicio = mach_absolute_time();
  mach_timebase_info(&tb);
  khv_traza_iniciar(t_inicio);
  int maxc = khv_max_vcpus();
  if (c->ncpus < 1 || c->ncpus > maxc) {
    fallo("vcpu_count must be between 1 and %d", maxc);
    return -1;
  }
  if (c->mem_size < (64ULL << 20) || c->mem_size % PAGINA) {
    fallo("mem_size_mib must be at least 64");
    return -1;
  }
  if (c->ndisks < 1 || c->ndisks > KHV_MAX_DISKS) {
    fallo("between 1 and %d drives", KHV_MAX_DISKS);
    return -1;
  }
  // Antes de tocar HVF: sin EL2 el proceso sigue sirviendo para otra VM.
  if (c->nested && !khv_el2_soportado()) {
    fallo("nested virtualization is not supported on this Mac (needs an Apple M3 or later and macOS 15 or later)");
    return -1;
  }
  ncpus = c->ncpus;
  const char *q = getenv("KLING_HV_QOS");
  qos_activa = !(q && strcmp(q, "0") == 0);
  const char *e = getenv("KLING_HV_EVENT_IDX");
  if (e && strcmp(e, "0") == 0) khv_event_idx = 0;  // para comparar: sin VIRTIO_RING_F_EVENT_IDX
  const char *mrg = getenv("KLING_HV_MRG_RXBUF");
  if (mrg && strcmp(mrg, "0") == 0) khv_mrg_rxbuf = 0;  // para comparar: búferes de rx del tamaño de la MTU
  g_ram_size = c->mem_size;
  ndisks = c->ndisks;
  hay_red = c->net_fd >= 0;
  hay_rng = c->rng != 0;
  hay_vsock = c->vsock != 0;
  hay_globo = c->balloon != 0;
  net_fd = c->net_fd;
  memcpy(net_mac, c->mac, 6);
  net_mtu = c->mtu;
  kernel_path = strdup(c->kernel ? c->kernel : "");
  cmdline = strdup(c->cmdline ? c->cmdline : "");
  ram_dir = strdup(c->ram_dir ? c->ram_dir : ".");
  nvirtio = ndisks;
  if (hay_red) slot_net = nvirtio++;
  if (hay_rng) slot_rng = nvirtio++;
  if (hay_vsock) slot_vsock = nvirtio++;
  if (hay_globo) slot_globo = nvirtio++;
  if (nvirtio > VIRTIO_MAX) {
    fallo("too many virtio devices (%d, at most %d)", nvirtio, VIRTIO_MAX);
    return -1;
  }
  if (c->nvfs < 0 || c->nvfs > KHV_MAX_VFS) {
    fallo("at most %d virtio-fs devices", KHV_MAX_VFS);
    return -1;
  }
  nvfs = c->nvfs;
  slot_vfs = nvirtio;
  nvirtio += nvfs;

  // Desde aquí un fallo deja el proceso inservible para otra VM
  // (Hypervisor.framework admite una por proceso y no hay vuelta atrás).
  vm_tocada = true;
  uint64_t tr = khv_traza_empieza(KHV_F_VM);
  anidada = c->nested != 0;
  if (anidada) {
    hv_vm_config_t vc = hv_vm_config_create();
    hv_return_t r = hv_vm_config_set_el2_enabled(vc, true);
    if (r == HV_SUCCESS) r = hv_vm_create(vc);
    os_release(vc);
    if (r != HV_SUCCESS) {
      fallo_hv("hv_vm_create with EL2 (nested virtualization)", r);
      return -1;
    }
  } else {
    TRY(hv_vm_create(NULL));
  }
  hv_gic_config_t g = hv_gic_config_create();
  TRY(hv_gic_config_set_distributor_base(g, GIC_DIST_BASE));
  TRY(hv_gic_config_set_redistributor_base(g, GIC_REDIST_BASE));
  TRY(hv_gic_create(g));
  os_release(g);
  uint32_t spi_base, spi_n;
  TRY(hv_gic_get_spi_interrupt_range(&spi_base, &spi_n));
  uart_intid = spi_base + 1;
  rtc_intid = spi_base + 2;
  if (spi_n < 16 + VIRTIO_SLOTS) {
    fallo("the GIC has %u SPIs, kling-hv needs %d", spi_n, 16 + VIRTIO_SLOTS);
    return -1;
  }
  for (int i = 0; i < VIRTIO_SLOTS; i++) virtio_intid[i] = spi_base + 16 + (uint32_t)i;
  khv_traza_acaba(KHV_F_VM, tr);

  tr = khv_traza_empieza(KHV_F_DISP);
  uart_init(uart_intid, c->console_fd);
  log_fd = c->console_fd >= 0 ? c->console_fd : 2;
  rtc_init();
  for (int i = 0; i < ndisks; i++) {
    disk_path[i] = strdup(c->disks[i].path);
    disk_ro[i] = c->disks[i].ro != 0;
    if (vblk_init(i, disk_path[i], disk_ro[i], c->disks[i].sinc, virtio_intid[i]) != 0) return -1;
  }
  if (hay_red) vnet_init(net_fd, net_mac, net_mtu, virtio_intid[slot_net]);
  if (hay_rng) vrng_init(virtio_intid[slot_rng]);
  if (hay_vsock) vsock_init(virtio_intid[slot_vsock]);
  if (hay_globo && vballoon_init(virtio_intid[slot_globo], c->balloon == 2) != 0) return -1;
  for (int i = 0; i < nvfs; i++)
    if (vvfs_init(i, c->vfs_tags[i], virtio_intid[slot_vfs + i]) != 0) return -1;
  khv_traza_acaba(KHV_F_DISP, tr);
  return 0;
}

// Mapea fd (abierto) como RAM del invitado.
static int mapear_ram(int fd) {
  g_ram = mmap(NULL, g_ram_size, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
  if (g_ram == MAP_FAILED) {
    fallo("mmap of the guest RAM: %s", strerror(errno));
    return -1;
  }
  base_fd = fd;
  TRY(hv_vm_map(g_ram, RAM_BASE, g_ram_size, HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC));
  return 0;
}

// ram_soltar devuelve al fichero las páginas [off, off+len) de la RAM: tira
// la copia privada (sucia) y deja la del fichero, limpia y fuera de
// phys_footprint. Es lo único que lo consigue con un mapeo MAP_PRIVATE de
// fichero: madvise (FREE, FREE_REUSABLE, DONTNEED) no baja la huella de esas
// páginas (medido en macOS 26.5). HVF lleva su propio mapeo de la región, así
// que también se rehace: primero se quita de HVF, luego se cambia el mapeo del
// proceso y al final se vuelve a dar a HVF. Así un fallo nunca deja al
// invitado viendo páginas distintas de las del host:
//   - si hv_vm_unmap falla no se ha tocado nada (las páginas siguen sucias);
//   - si el mmap falla, XNU deja el mapeo anterior y se vuelve a dar a HVF;
//   - si hv_vm_map falla (dos veces), el tramo queda como hueco (huecos) y
//     se vuelve a mapear cuando el invitado lo toque (ram_reparar_ipa) o lo
//     reciba de vuelta (ram_reparar). Antes la VM terminaba: un globo podía
//     tumbar a su propio invitado.
// Solo se llama con páginas que el invitado no usa (en el globo o informadas
// como libres): ningún vCPU las toca mientras no están en HVF.
static pthread_mutex_t hueco_mu = PTHREAD_MUTEX_INITIALIZER;
static uint8_t *huecos;           // una entrada por página del host fuera de HVF, con hueco_mu
static volatile uint64_t n_huecos;
static uint32_t avisos_ram;       // fallos ya contados en la consola (con hueco_mu)

#define RAM_PERMISOS (HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC)

static void aviso_ram(const char *que, uint64_t off, uint64_t len, hv_return_t r) {
  if (avisos_ram++ < 8)
    logf_("balloon: %s of guest RAM [%#llx, +%#llx) failed: 0x%x (the pages stay mapped and dirty)", que,
          (unsigned long long)(RAM_BASE + off), (unsigned long long)len, (unsigned)r);
}

// Con hueco_mu: vuelve a dar a HVF los huecos de [desde, hasta) (páginas).
static int reparar_locked(uint64_t desde, uint64_t hasta) {
  int ret = 0;
  for (uint64_t p = desde; p < hasta;) {
    if (!huecos[p]) {
      p++;
      continue;
    }
    uint64_t q = p;
    while (q < hasta && huecos[q]) q++;
    hv_return_t r = hv_vm_map(g_ram + p * PAGINA, RAM_BASE + p * PAGINA, (q - p) * PAGINA, RAM_PERMISOS);
    if (r == HV_SUCCESS) {
      memset(huecos + p, 0, (size_t)(q - p));
      n_huecos -= q - p;
    } else {
      ret = -1;
    }
    p = q;
  }
  return ret;
}

int ram_reparar(uint64_t off, uint64_t len) {
  if (n_huecos == 0 || !huecos || off >= g_ram_size) return 0;
  if (len > g_ram_size - off) len = g_ram_size - off;
  pthread_mutex_lock(&hueco_mu);
  int r = reparar_locked(off / PAGINA, (off + len + PAGINA - 1) / PAGINA);
  pthread_mutex_unlock(&hueco_mu);
  return r;
}

int ram_soltar(uint64_t off, uint64_t len) {
  if (!g_ram || off > g_ram_size || len > g_ram_size - off || (off | len) % PAGINA || len == 0) return -1;
  pthread_mutex_lock(&hueco_mu);
  // Un hueco dentro del tramo haría fallar el hv_vm_unmap del tramo entero.
  if (n_huecos && reparar_locked(off / PAGINA, (off + len) / PAGINA) != 0) {
    pthread_mutex_unlock(&hueco_mu);
    return -1;
  }
  hv_return_t r = hv_vm_unmap(RAM_BASE + off, len);
  if (r != HV_SUCCESS) {
    aviso_ram("hv_vm_unmap", off, len, r);
    pthread_mutex_unlock(&hueco_mu);
    return -1;
  }
  bool hecho = mmap(g_ram + off, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_FIXED, base_fd, (off_t)off) != MAP_FAILED;
  if (!hecho) aviso_ram("mmap", off, len, (hv_return_t)errno);
  r = hv_vm_map(g_ram + off, RAM_BASE + off, len, RAM_PERMISOS);
  if (r != HV_SUCCESS) r = hv_vm_map(g_ram + off, RAM_BASE + off, len, RAM_PERMISOS);
  if (r != HV_SUCCESS) {
    aviso_ram("hv_vm_map", off, len, r);
    if (!huecos) huecos = calloc((size_t)(g_ram_size / PAGINA), 1);
    if (!huecos) {
      // Sin dónde apuntarlo no hay forma de arreglarlo luego.
      pthread_mutex_unlock(&hueco_mu);
      fin_vm(1, "remapping guest RAM for the balloon failed (and no memory to track it)");
      return -1;
    }
    for (uint64_t p = off / PAGINA; p < (off + len) / PAGINA; p++)
      if (!huecos[p]) huecos[p] = 1, n_huecos++;
    hecho = false;
  }
  pthread_mutex_unlock(&hueco_mu);
  return hecho ? 0 : -1;
}

// ram_reparar_ipa atiende un fallo de etapa 2 del vCPU c en la RAM (ipa):
// vuelve a mapear el hueco, o, si no lo era, espera al ram_soltar que lo
// tuviera quitado (hueco_mu). 0 para repetir la instrucción; -1 si la misma
// página sigue fallando (la VM termina con el volcado del vCPU).
static int ram_reparar_ipa(struct cpu *c, uint64_t ipa) {
  uint64_t pag = (ipa - RAM_BASE) / PAGINA;
  pthread_mutex_lock(&hueco_mu);
  bool era = huecos && huecos[pag];
  int r = era ? reparar_locked(pag, pag + 1) : 0;
  pthread_mutex_unlock(&hueco_mu);
  if (era && r == 0) {
    c->fallos_ram = 0;
    return 0;
  }
  if (++c->fallos_ram > 64) return -1;
  if (r != 0) usleep(1000);  // HVF sin recursos ahora mismo: otra vez en un momento
  return 0;
}

static int cargar_kernel(void) {
  int fd = open(kernel_path, O_RDONLY);
  if (fd < 0) {
    fallo("open %s: %s", kernel_path, strerror(errno));
    return -1;
  }
  struct stat st;
  fstat(fd, &st);
  uint8_t hdr[64];
  if (pread(fd, hdr, 64, 0) != 64 || memcmp(hdr + 56, "ARM\x64", 4) != 0) {
    close(fd);
    fallo("%s is not an arm64 Image", kernel_path);
    return -1;
  }
  uint64_t text_offset, image_size;
  memcpy(&text_offset, hdr + 8, 8);
  memcpy(&image_size, hdr + 16, 8);
  kernel_entry = RAM_BASE + text_offset;
  void *dst = gpa_ptr(kernel_entry, (uint64_t)st.st_size);
  if (!dst || text_offset + image_size + (4 << 20) > g_ram_size) {
    close(fd);
    fallo("not enough guest RAM for the kernel");
    return -1;
  }
  if (pread(fd, dst, (size_t)st.st_size, 0) != st.st_size) {
    close(fd);
    fallo("reading the kernel: %s", strerror(errno));
    return -1;
  }
  close(fd);

  struct fdt_params p = {.ram_size = g_ram_size, .ncpus = ncpus, .bootargs = cmdline, .uart_spi = uart_intid,
                         .rtc_spi = rtc_intid, .nvirtio = nvirtio, .nested = anidada};
  size_t sz;
  hv_gic_get_distributor_size(&sz);
  p.dist_size = sz;
  hv_gic_get_redistributor_size(&sz);
  p.redist_size = sz;
  for (int i = 0; i < nvirtio; i++) p.virtio_spi[i] = virtio_intid[i];
  // El DTB va al final de la RAM, alineado a 2 MiB y fuera de la imagen.
  fdt_gpa = RAM_BASE + g_ram_size - (2 << 20);
  if (fdt_build(gpa_ptr(fdt_gpa, 1 << 20), 1 << 20, &p) == 0) return -1;
  return 0;
}

int khv_boot(void) {
  // La RAM de un arranque en frío: un fichero disperso del tamaño de la RAM,
  // borrado nada más abrirlo (el snapshot lo clona por su fd).
  char ruta[1024];
  uint64_t tr = khv_traza_empieza(KHV_F_RAM);
  snprintf(ruta, sizeof ruta, "%s/.kling-hv-ram.XXXXXX", ram_dir);
  int fd = mkstemp(ruta);
  if (fd < 0) {
    fallo("creating the RAM file in %s: %s", ram_dir, strerror(errno));
    return -1;
  }
  unlink(ruta);
  if (ftruncate(fd, (off_t)g_ram_size) != 0) {
    fallo("sizing the RAM file: %s", strerror(errno));
    close(fd);
    return -1;
  }
  if (mapear_ram(fd) != 0) return -1;
  khv_traza_acaba(KHV_F_RAM, tr);
  tr = khv_traza_empieza(KHV_F_KERNEL);
  if (cargar_kernel() != 0) return -1;
  khv_traza_acaba(KHV_F_KERNEL, tr);
  tr = khv_traza_empieza(KHV_F_VCPUS);
  if (crear_vcpus() != 0) return -1;
  khv_traza_acaba(KHV_F_VCPUS, tr);
  pthread_mutex_lock(&mu);
  soltar_locked();
  pthread_mutex_unlock(&mu);
  return 0;
}

static int leer_todo(int fd, void *b, size_t n, off_t off) {
  while (n > 0) {
    ssize_t r = pread(fd, b, n, off);
    if (r <= 0) return -1;
    b = (uint8_t *)b + r;
    n -= (size_t)r;
    off += r;
  }
  return 0;
}

static int escribir_todo(int fd, const void *b, size_t n, off_t off) {
  while (n > 0) {
    ssize_t w = pwrite(fd, b, n, off);
    if (w <= 0) return -1;
    b = (const uint8_t *)b + w;
    n -= (size_t)w;
    off += w;
  }
  return 0;
}

// used_de_ram pone el used_idx de cada cola de la cabecera desde su anillo
// used en la RAM ya mapeada (snapshots sin SNAP_F_USED_IDX). Las colas de los
// dispositivos que la máquina no tiene quedan como vengan: no se usan.
static void used_de_ram(struct snap_hdr *h, struct snap_vfs *sv) {
#define USED_DE_RAM(qs) \
  for (size_t k = 0; k < sizeof(qs) / sizeof(struct vq); k++) vq_used_de_ram(&((struct vq *)&(qs))[k])
  for (int i = 0; i < ndisks; i++) USED_DE_RAM(h->vblk[i].q);
  if (hay_red) USED_DE_RAM(h->vnet.q);
  if (hay_rng) USED_DE_RAM(h->vrng.q);
  if (hay_vsock) USED_DE_RAM(h->vsock.q);
  if (hay_globo) USED_DE_RAM(h->globo.q);
  for (int i = 0; i < nvfs; i++) USED_DE_RAM(sv->vfs[i].q);
#undef USED_DE_RAM
}

int khv_restore(const char *mem_path) {
  if (anidada) {
    fallo(ERR_SNAP_ANIDADA);
    return -1;
  }
  khv_traza_restaurando();
  uint64_t tr = khv_traza_empieza(KHV_F_LEER);
  int fd = open(mem_path, O_RDONLY);
  if (fd < 0) {
    fallo("open %s: %s", mem_path, strerror(errno));
    return -1;
  }
  struct stat st;
  struct snap_pie pie;
  static struct snap_hdr h;
  if (fstat(fd, &st) != 0 || st.st_size < (off_t)sizeof pie ||
      leer_todo(fd, &pie, sizeof pie, st.st_size - (off_t)sizeof pie) != 0 || pie.magic != SNAP_MAGIC ||
      !snap_forma(pie.hdr_len) ||
      pie.hdr_off > (uint64_t)st.st_size ||
      pie.hdr_len > (uint64_t)st.st_size - pie.hdr_off ||
      (memset(&h, 0, sizeof h), leer_todo(fd, &h, pie.hdr_len < sizeof h ? pie.hdr_len : sizeof h, (off_t)pie.hdr_off)) != 0 ||
      h.magic != SNAP_MAGIC) {
    close(fd);
    fallo("%s is not a kling-hv memory file (or was written by another version)", mem_path);
    return -1;
  }
  // Cada versión con el tamaño de cabecera de esa versión (y de sus vCPU; en
  // la 6, más la sección de virtio-fs con sus dispositivos).
  size_t cab = snap_hdr_len(h.version, h.ncpus);
  static struct snap_vfs sv;
  memset(&sv, 0, sizeof sv);
  if ((h.version == 6 || h.version == 7) && cab > 0) {
    uint64_t resto = pie.hdr_len > cab ? pie.hdr_len - cab : 0;
    if (resto < SNAP_VFS_LEN(h.version == 7 ? 0 : 1) || resto > SNAP_VFS_LEN(KHV_MAX_VFS) ||
        (resto - SNAP_VFS_CAB) % sizeof(struct vvfs_state) != 0 ||
        leer_todo(fd, &sv, resto, (off_t)(pie.hdr_off + cab)) != 0 || SNAP_VFS_LEN(sv.nvfs) != resto ||
        (memset(&h, 0, sizeof h), leer_todo(fd, &h, cab, (off_t)pie.hdr_off)) != 0) {
      cab = 0;
    } else {
      cab = pie.hdr_len;
    }
  }
  if (cab == 0 || cab != pie.hdr_len) {
    close(fd);
    fallo("%s: snapshot state version %u with %u vCPU and a %llu-byte header, this kling-hv reads up to %u (%zu bytes "
          "up to %d vCPU, %zu more per vCPU, %d at most)",
          mem_path, h.version, h.ncpus, (unsigned long long)pie.hdr_len, SNAP_VERSION, (size_t)SNAP_HDR_V4, SNAP_CPUS_V4,
          sizeof(struct cpu_state), MAX_CPUS);
    return -1;
  }
  // La disposición la fija khv_snapshot: RAM en [0, ram_size), la cabecera
  // justo detrás, el estado del GIC y el pie al final. Un fichero recortado o
  // ajeno que no cuadre se rechaza aquí: mapear_ram mapea ram_size bytes del
  // fichero y, si es más corto, el invitado tocaría páginas tras el final y
  // el VMM moriría de SIGBUS en vez de dar un error.
  if (pie.hdr_off != pie.ram_size || h.ram_size != pie.ram_size || h.gic_size > SNAP_GIC_MAX ||
      pie.hdr_off + pie.hdr_len > (uint64_t)st.st_size - sizeof pie ||
      h.gic_size > (uint64_t)st.st_size - sizeof pie - pie.hdr_off - pie.hdr_len) {
    close(fd);
    fallo("%s: truncated or inconsistent kling-hv memory file (size %lld, ram %llu, header at %llu+%llu, GIC %llu)",
          mem_path, (long long)st.st_size, h.ram_size, pie.hdr_off, pie.hdr_len, h.gic_size);
    return -1;
  }
  if (h.version == 1) {
    // Una cabecera anterior no tiene vsock ni globo: el invitado nunca vio
    // esos dispositivos (van detrás de los demás), así que no existen.
    hay_vsock = hay_globo = false;
  } else if (h.version == 2) {
    // El globo de la versión 2 (sin cola de estadísticas) quedó leído con
    // la forma nueva: se vuelve a leer con la suya y se convierte. Sus dos
    // colas y su estado siguen; la de estadísticas la negociará el driver
    // solo si arranca de nuevo (un invitado restaurado no la vio).
    struct vballoon_state_v2 g2;
    if (leer_todo(fd, &g2, sizeof g2, (off_t)(pie.hdr_off + offsetof(struct snap_hdr, globo))) != 0) {
      close(fd);
      fallo("%s: reading the balloon state", mem_path);
      return -1;
    }
    memset(&h.globo, 0, sizeof h.globo);
    h.globo.m = g2.m;
    h.globo.q[0] = g2.q[0];
    h.globo.q[1] = g2.q[1];
    h.globo.num_pages = g2.num_pages;
    h.globo.actual = g2.actual;
  } else if (h.version == 3) {
    // Lo mismo con la 3, sin la cola del informe de páginas libres: no se
    // ofrecía, así que el invitado restaurado no la negoció.
    struct vballoon_state_v3 g3;
    if (leer_todo(fd, &g3, sizeof g3, (off_t)(pie.hdr_off + offsetof(struct snap_hdr, globo))) != 0) {
      close(fd);
      fallo("%s: reading the balloon state", mem_path);
      return -1;
    }
    memset(&h.globo, 0, sizeof h.globo);
    h.globo.m = g3.m;
    memcpy(h.globo.q, g3.q, sizeof g3.q);
    h.globo.num_pages = g3.num_pages;
    h.globo.actual = g3.actual;
    h.globo.stats_head = g3.stats_head;
    h.globo.stats_pend = g3.stats_pend;
  }
  if (h.ram_size != g_ram_size || (int)h.ncpus != ncpus || (int)h.ndisks != ndisks || (bool)h.hay_red != hay_red ||
      (bool)h.hay_rng != hay_rng || (bool)h.hay_vsock != hay_vsock || (bool)h.hay_globo != hay_globo ||
      (int)sv.nvfs != nvfs || h.uart_intid != uart_intid) {
    close(fd);
    fallo("the snapshot does not match the configured machine (ram %llu/%llu, cpus %u/%d, disks %u/%d, net %u/%d, "
          "rng %u/%d, vsock %u/%d, balloon %u/%d, virtio-fs %u/%d)",
          h.ram_size, g_ram_size, h.ncpus, ncpus, h.ndisks, ndisks, h.hay_red, hay_red, h.hay_rng, hay_rng,
          h.hay_vsock, hay_vsock, h.hay_globo, hay_globo, sv.nvfs, nvfs);
    return -1;
  }
  void *gdata = malloc(h.gic_size ? h.gic_size : 1);
  if (!gdata || leer_todo(fd, gdata, h.gic_size, (off_t)(pie.hdr_off + pie.hdr_len)) != 0) {
    free(gdata);
    close(fd);
    fallo("%s: GIC state truncated", mem_path);
    return -1;
  }
  khv_traza_acaba(KHV_F_LEER, tr);
  tr = khv_traza_empieza(KHV_F_RAM);
  if (mapear_ram(fd) != 0) {
    free(gdata);
    return -1;
  }
  khv_traza_acaba(KHV_F_RAM, tr);
  tr = khv_traza_empieza(KHV_F_DISP_ESTADO);
  if (h.version < 4 || !(h.flags & SNAP_F_USED_IDX)) used_de_ram(&h, &sv);
  uart_set(&h.uart);
  rtc_set(&h.rtc);
  for (int i = 0; i < ndisks; i++) vblk_set(i, &h.vblk[i]);
  if (hay_red) {
    // La red es de la instancia (como el netns en Linux); las colas siguen
    // donde las dejó el invitado.
    vnet_set(&h.vnet);
  }
  if (hay_rng) vrng_set(&h.vrng);
  if (hay_vsock) vsock_set(&h.vsock);
  if (hay_globo) vballoon_set(&h.globo);
  for (int i = 0; i < nvfs; i++) vvfs_set(i, &sv.vfs[i]);
  restaurando = true;
  khv_traza_acaba(KHV_F_DISP_ESTADO, tr);
  tr = khv_traza_empieza(KHV_F_VCPUS);
  if (crear_vcpus() != 0) {
    free(gdata);
    return -1;
  }
  khv_traza_acaba(KHV_F_VCPUS, tr);
  tr = khv_traza_empieza(KHV_F_GIC);
  hv_return_t r = hv_gic_set_state(gdata, h.gic_size);
  free(gdata);
  if (r != HV_SUCCESS) {
    fallo("hv_gic_set_state failed: 0x%x", (unsigned)r);
    return -1;
  }
  // El nivel de la línea de cada SPI es del dispositivo y hv_gic_set_state no
  // lo deja como estaba: sin volver a subirlo, una interrupción pendiente en
  // el snapshot no llega nunca y el dispositivo cree que la línea ya está alta.
  uart_reafirmar();
  for (int i = 0; i < ndisks; i++) vblk_reafirmar(i);
  if (hay_red) vnet_reafirmar();
  if (hay_rng) vrng_reafirmar();
  if (hay_vsock) vsock_reafirmar();
  if (hay_globo) vballoon_reafirmar();
  for (int i = 0; i < nvfs; i++) vvfs_reafirmar(i);
  for (int i = 0; i < ncpus; i++) cpus[i].st = h.cpu[i];
  khv_traza_acaba(KHV_F_GIC, tr);
  for (int i = 0; i < ncpus; i++) cpus[i].st = *hdr_cpu(&h, i);
  snap_t_pausa = h.t_pausa;
  // Queda en pausa hasta khv_resume: los PATCH de discos del núcleo ya
  // llegaron antes de crear la VM, pero el núcleo reanuda cuando quiere.
  return 0;
}

// Lo más que espera khv_pause a que todos los vCPU lleguen al punto de pausa.
#define PAUSA_PLAZO_NS (5ULL * 1000000000ULL)

// Los dispositivos quedaron quietos (dispositivos_quietos_plazo) en la pausa
// en curso. Con op_mu. Solo lo necesita una pausa del núcleo (KHV_PAUSA_API):
// el snapshot no puede ver la RAM a medio escribir por un dispositivo. Al
// regulador de CPU (KHV_PAUSA_FRENO, cada pocos ms) le basta con los vCPU
// parados: los hilos de los dispositivos ven g_pausa y se paran solos en su
// siguiente vuelta, y lo que tuvieran en curso puede acabar sin estorbar.
static bool disp_quietos;

// quietos_o_deshacer deja quietos los dispositivos para una pausa del núcleo
// con los vCPU ya parados. Si no llegan en el plazo, la pausa vuelve a ser la
// de antes: con antes == 0 se deshace entera (a los vCPU se les suma lo que
// duró, como en khv_resume, y el invitado no ve el salto; los dispositivos se
// despiertan); con antes != 0 (la del regulador sigue) solo se quita el bit.
// Con op_mu; -1 si no llegaron.
static int quietos_o_deshacer(unsigned antes) {
  if (dispositivos_quietos_plazo(PAUSA_PLAZO_NS)) {
    disp_quietos = true;
    uart_flush();
    return 0;
  }
  pthread_mutex_lock(&mu);
  pausa_bits = antes;
  bool reloj = pausa_reloj;
  pausa_reloj = false;  // antes no tenía KHV_PAUSA_API: la habría dejado quieta
  if (antes == 0) {
    if (reloj && t_pausado) {
      uint64_t d = mach_absolute_time() - t_pausado;
      for (int i = 0; i < ncpus; i++) cpus[i].ajuste_vtimer += d;
    }
    pausa_pedida = false;
    g_pausa = false;
    pthread_cond_broadcast(&cv);
  }
  pthread_mutex_unlock(&mu);
  if (antes == 0) dispositivos_despertar();
  fallo("devices did not quiesce in time (%d ms)", (int)(PAUSA_PLAZO_NS / 1000000));
  logf_("%s", errbuf);
  return -1;
}

int khv_pause(unsigned bit) {
  // Solo la del núcleo: la del regulador va cada pocos ms.
  bool trazar = bit & KHV_PAUSA_API;
  uint64_t tr = trazar ? khv_traza_empieza(KHV_F_PAUSA) : 0;
  pthread_mutex_lock(&op_mu);
  pthread_mutex_lock(&mu);
  unsigned antes = pausa_bits;
  pausa_bits |= bit;
  // También si la pausa ya estaba (la del regulador): desde ahora es del
  // núcleo y puede durar lo que quiera.
  if (bit & KHV_PAUSA_API) pausa_reloj = true;
  if (antes != 0 || !liberada || parar) {
    // La del regulador pasa a ser del núcleo: sus dispositivos no se
    // pararon (ver disp_quietos), así que se paran ahora.
    bool falta = (bit & KHV_PAUSA_API) && antes != 0 && liberada && !parar && !disp_quietos;
    // Los vCPU que paró el regulador no guardaron su estado: se les despierta
    // para que lo guarden (punto_pausa) antes de que haya snapshot.
    bool guardar = (bit & KHV_PAUSA_API) && antes != 0 && liberada && !parar;
    if (guardar) {
      pthread_cond_broadcast(&cv);
      pthread_mutex_unlock(&mu);
      khv_qos_impulso();
      pthread_mutex_lock(&mu);
      while (n_guardados < ncpus && !parar) pthread_cond_wait(&cv, &mu);
    }
    pthread_mutex_unlock(&mu);
    int r = falta ? quietos_o_deshacer(antes) : 0;
    pthread_mutex_unlock(&op_mu);
    if (trazar) khv_traza_acaba(KHV_F_PAUSA, tr);
    return r;
  }
  pausa_pedida = true;
  g_pausa = true;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  // Pausar es trabajo que el host le manda al invitado: los vCPU ociosos
  // están en UTILITY y, con el Mac cargado, tardaban decenas de ms en
  // llegar al punto de pausa (khv_qos_impulso, como un exec). Solo para la
  // del núcleo: la del regulador es justo para que el invitado corra menos,
  // y subirle la QoS en cada una le daba núcleos P a lo que se frena.
  if (bit & KHV_PAUSA_API) khv_qos_impulso();
  salir_vcpus();
  pthread_mutex_lock(&mu);
  // Un vCPU que miró pausa_pedida justo antes de que se pusiera y aún no
  // había entrado en hv_vcpu_run no recibe la salida; dentro, en un WFI, se
  // quedaría hasta su siguiente timer. Cada milisegundo sin todos, se vuelve
  // a sacar a los que faltan.
  //
  // Con un plazo (PAUSA_PLAZO_NS): un vCPU que no llega nunca al punto de
  // pausa (su hilo bloqueado fuera de hv_vcpu_run, en un dispositivo) dejaba
  // freeze y stop esperando para siempre con op_mu cogido. Pasado el plazo se
  // deshace la pausa: los que ya estaban parados siguen, la VM queda
  // corriendo como antes y khv_pause devuelve el error para que el núcleo lo
  // diga.
  uint64_t limite = mach_absolute_time() + (uint64_t)((__uint128_t)PAUSA_PLAZO_NS * tb.denom / tb.numer);
  bool a_tiempo = true;
  // La del núcleo espera también a que cada uno guarde su estado.
  while ((n_pausados < ncpus || ((bit & KHV_PAUSA_API) && n_guardados < ncpus)) && !parar) {
    if (mach_absolute_time() >= limite) {
      a_tiempo = false;
      break;
    }
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_nsec += 1000000;
    if (ts.tv_nsec >= 1000000000L) ts.tv_sec++, ts.tv_nsec -= 1000000000L;
    if (pthread_cond_timedwait(&cv, &mu, &ts) == ETIMEDOUT) resacar_vcpus_locked();
  }
  if (!a_tiempo) {
    int faltan = ncpus - n_pausados;
    pausa_bits = antes;
    pausa_reloj = false;  // antes era 0: no había otra pausa
    pausa_pedida = false;
    g_pausa = false;
    pthread_cond_broadcast(&cv);
    pthread_mutex_unlock(&mu);
    // Los hilos de red, vsock y disco que vieron g_pausa esperan en su cv
    // hasta que alguien los despierte (como en khv_resume); sin esto la VM
    // parecía correr pero exec, red y disco seguían parados hasta el
    // siguiente aviso del invitado.
    dispositivos_despertar();
    fallo("vCPU did not pause in time (%d of %d still running after %d ms)", faltan, ncpus,
          (int)(PAUSA_PLAZO_NS / 1000000));
    logf_("%s", errbuf);
    pthread_mutex_unlock(&op_mu);
    return -1;
  }
  t_pausado = mach_absolute_time();
  pthread_mutex_unlock(&mu);
  // Los dispositivos, con el mismo plazo, solo para una pausa del núcleo (ver
  // disp_quietos).
  if ((bit & KHV_PAUSA_API) && quietos_o_deshacer(antes) < 0) {
    pthread_mutex_unlock(&op_mu);
    return -1;
  }
  pthread_mutex_unlock(&op_mu);
  if (trazar) khv_traza_acaba(KHV_F_PAUSA, tr);
  return parar ? -1 : 0;
}

int khv_resume(unsigned bit) {
  pthread_mutex_lock(&op_mu);
  pthread_mutex_lock(&mu);
  pausa_bits &= ~bit;
  bool despertar = false;
  if (pausa_bits == 0 && !parar) {
    if (!liberada) {
      soltar_locked();
      despertar = true;
    } else if (pausa_pedida) {
      // El contador virtual no avanza durante una pausa del núcleo: cada
      // vCPU suma lo que duró a su offset al salir del punto de pausa (como
      // la restauración en soltar_locked). El reloj de pared del invitado
      // queda atrasado eso mismo: lo arregla el /resync del agente, que el
      // núcleo tiene que pedir tras reanudar.
      if (pausa_reloj && t_pausado) {
        uint64_t d = mach_absolute_time() - t_pausado;
        for (int i = 0; i < ncpus; i++) cpus[i].ajuste_vtimer += d;
      }
      pausa_reloj = false;
      pausa_pedida = false;
      g_pausa = false;
      pthread_cond_broadcast(&cv);
      despertar = true;
    }
    disp_quietos = false;
  }
  pthread_mutex_unlock(&mu);
  if (despertar) dispositivos_despertar();
  pthread_mutex_unlock(&op_mu);
  return parar ? -1 : 0;
}

int khv_snapshot(const char *mem_out) {
  if (anidada) {
    fallo(ERR_SNAP_ANIDADA);
    return -1;
  }
  pthread_mutex_lock(&op_mu);
  int ret = -1;
  int fd = -1;
  void *gdata = NULL;
  unsigned char *v = NULL;
  static struct snap_hdr h;
  pthread_mutex_lock(&mu);
  // disp_quietos: una pausa solo del regulador no paró los dispositivos.
  bool en_pausa = liberada && pausa_pedida && n_pausados == ncpus && n_guardados == ncpus && disp_quietos;
  pthread_mutex_unlock(&mu);
  if (!en_pausa) {
    fallo("the VM must be paused to snapshot it");
    goto fin;
  }
  uint64_t tr = khv_traza_empieza(KHV_F_SNAP_ESTADO);
  memset(&h, 0, sizeof h);
  h.magic = SNAP_MAGIC;
  h.flags = SNAP_F_USED_IDX;
  // La versión más vieja que lo dice todo: la 4 hasta SNAP_CPUS_V4 vCPU.
  h.version = ncpus > SNAP_CPUS_V4 ? 5 : 4;
  h.ncpus = (uint32_t)ncpus;
  h.ndisks = (uint32_t)ndisks;
  h.ram_size = g_ram_size;
  h.uart_intid = uart_intid;
  h.rtc_intid = rtc_intid;
  memcpy(h.virtio_intid, virtio_intid, sizeof h.virtio_intid);
  h.t_pausa = t_pausado;
  for (int i = 0; i < ndisks; i++) {
    h.disk_ro[i] = disk_ro[i];
    vblk_get(i, &h.vblk[i]);
  }
  uart_get(&h.uart);
  rtc_get(&h.rtc);
  h.hay_red = hay_red;
  memcpy(h.mac, net_mac, 6);
  if (hay_red) vnet_get(&h.vnet);
  h.hay_rng = hay_rng;
  if (hay_rng) vrng_get(&h.vrng);
  h.hay_vsock = hay_vsock;
  if (hay_vsock) vsock_get(&h.vsock);
  h.hay_globo = hay_globo;
  if (hay_globo) vballoon_get(&h.globo);
  static struct snap_vfs sv;
  memset(&sv, 0, sizeof sv);
  sv.nvfs = (uint32_t)nvfs;
  for (int i = 0; i < nvfs; i++) vvfs_get(i, &sv.vfs[i]);
  if (nvfs > 0) h.version = 6;
  // MRG_RXBUF aceptado: la 7 (ver SNAP_VERSION), para que no lo restaure un
  // kling-hv que no lo sabe atender.
  if (hay_red && ((h.vnet.m.drv_features >> 15) & 1)) h.version = 7;
  // Un CPU_ON pendiente (on_pend, on_entry, on_ctx) no va en la cabecera:
  // se guarda como si ya hubiera entrado, con el estado que le pone el hilo
  // del vCPU al atenderlo (estado_cpu_on). Así el formato no cambia y el vCPU
  // restaurado arranca en su entrada en vez de quedarse apagado para siempre.
  // Con todos en pausa nadie toca on_pend, pero se lee con mu como en psci.
  bool pendientes[MAX_CPUS] = {0};
  pthread_mutex_lock(&mu);
  for (int i = 0; i < ncpus; i++) pendientes[i] = cpu_guardada(hdr_cpu(&h, i), &cpus[i]);
  pthread_mutex_unlock(&mu);
  for (int i = 0; i < ncpus; i++)
    if (pendientes[i]) logf_("cpu%d: pending CPU_ON saved as already entered", i);
  hv_gic_state_t gs = hv_gic_state_create();
  if (!gs) {
    fallo("hv_gic_state_create returned NULL");
    goto fin;
  }
  size_t gsz = 0;
  if (hv_gic_state_get_size(gs, &gsz) != HV_SUCCESS || !(gdata = malloc(gsz)) ||
      hv_gic_state_get_data(gs, gdata) != HV_SUCCESS) {
    os_release(gs);
    fallo("reading the GIC state failed");
    goto fin;
  }
  os_release(gs);
  h.gic_size = gsz;
  khv_traza_acaba(KHV_F_SNAP_ESTADO, tr);
  tr = khv_traza_empieza(KHV_F_SNAP_CLON);

  // Memoria: clon del fichero que respalda la RAM + las páginas que el
  // invitado cambió (copiadas por el CoW de MAP_PRIVATE). mincore da el mapa.
  if (fclonefileat(base_fd, AT_FDCWD, mem_out, 0) == 0) {
    fd = open(mem_out, O_RDWR);
  } else {
    // Otro volumen o sin clones: copia de los datos. 0600: lleva toda la RAM
    // del invitado (secretos ya leídos incluidos), como el clon.
    fd = open(mem_out, O_RDWR | O_CREAT | O_EXCL, 0600);
    if (fd >= 0 && fcopyfile(base_fd, fd, NULL, COPYFILE_DATA) != 0) {
      fallo("copying the RAM file to %s: %s", mem_out, strerror(errno));
      goto fin;
    }
  }
  if (fd < 0) {
    fallo("creating %s: %s", mem_out, strerror(errno));
    goto fin;
  }
  // Lo que hubiera detrás de la RAM es el estado del snapshot anterior.
  if (ftruncate(fd, (off_t)g_ram_size) != 0) {
    fallo("truncating %s: %s", mem_out, strerror(errno));
    goto fin;
  }
  khv_traza_acaba(KHV_F_SNAP_CLON, tr);
  tr = khv_traza_empieza(KHV_F_SNAP_PAGINAS);
  size_t np = g_ram_size / PAGINA;
  v = malloc(np);
  if (!v || mincore(g_ram, g_ram_size, (char *)v) != 0) {
    fallo("mincore: %s", strerror(errno));
    goto fin;
  }
  // Las páginas residentes que el invitado solo leyó (MINCORE_INCORE, sin
  // cambios) ya están en el clon, pero el clon es otro vnode y no comparte la
  // caché unificada con su base: el invitado restaurado las tendría que leer
  // del SSD al tocarlas (sin nada, el thaw -> primer exec pasaba de ~55 a
  // ~130 ms en el M4). Antes se reescribían, que las ensuciaba en el clon (se
  // dejaban de compartir sus bloques con la base y se iban al disco) y doblaba
  // el freeze (~180 -> ~90 ms, 53 MiB limpios frente a 5 sucios con -mem 256).
  // F_RDADVISE las pide a la caché del clon en segundo plano y sin copiarlas.
  const unsigned char sucia = MINCORE_MODIFIED | MINCORE_COPIED | MINCORE_ANONYMOUS | MINCORE_PAGED_OUT;
  // Lo inflado en el globo no se escribe: al invitado no le importa qué tenga.
  for (size_t i = 0; i < np; i++)
    if (hay_globo && vballoon_inflada(i)) v[i] = 0;
  for (size_t i = 0; i < np;) {
    // 2: hay que escribirla; 1: solo leída (a la caché); 0: nada.
    int k = v[i] & sucia ? 2 : v[i] & MINCORE_INCORE ? 1 : 0;
    size_t j = i + 1;
    while (j < np && (v[j] & sucia ? 2 : v[j] & MINCORE_INCORE ? 1 : 0) == k) j++;
    if (k == 2 && escribir_todo(fd, g_ram + i * PAGINA, (j - i) * PAGINA, (off_t)(i * PAGINA)) != 0) {
      fallo("writing %s: %s", mem_out, strerror(errno));
      goto fin;
    }
    // ra_count es un int: por tramos de 1 GiB como mucho. Es un consejo; si
    // no se atiende, el invitado las lee al tocarlas, como sin él.
    for (size_t p = i; k == 1 && p < j;) {
      size_t n = j - p < (1u << 30) / PAGINA ? j - p : (1u << 30) / PAGINA;
      struct radvisory ra = {.ra_offset = (off_t)(p * PAGINA), .ra_count = (int)(n * PAGINA)};
      (void)fcntl(fd, F_RDADVISE, &ra);
      p += n;
    }
    i = j;
  }
  khv_traza_acaba(KHV_F_SNAP_PAGINAS, tr);
  tr = khv_traza_empieza(KHV_F_SNAP_ESCRIBIR);
  // La cabecera de la 4 (o de la 5, con más vCPU) tal cual y, con virtio-fs
  // (la 6), su sección detrás.
  size_t hl = snap_hdr_len(h.version, h.ncpus);
  size_t vl = h.version >= 6 ? SNAP_VFS_LEN(nvfs) : 0;
  struct snap_pie pie = {.magic = SNAP_MAGIC, .ram_size = g_ram_size, .hdr_off = g_ram_size, .hdr_len = hl + vl};
  if (escribir_todo(fd, &h, hl, (off_t)g_ram_size) != 0 ||
      (vl > 0 && escribir_todo(fd, &sv, vl, (off_t)(g_ram_size + hl)) != 0) ||
      escribir_todo(fd, gdata, gsz, (off_t)(g_ram_size + hl + vl)) != 0 ||
      escribir_todo(fd, &pie, sizeof pie, (off_t)(g_ram_size + hl + vl + gsz)) != 0) {
    fallo("writing the state to %s: %s", mem_out, strerror(errno));
    goto fin;
  }
  khv_traza_acaba(KHV_F_SNAP_ESCRIBIR, tr);
  khv_traza_freeze();
  ret = 0;
fin:
  if (fd >= 0) close(fd);
  if (ret != 0 && fd >= 0) unlink(mem_out);
  free(gdata);
  free(v);
  pthread_mutex_unlock(&op_mu);
  return ret;
}

int khv_balloon(uint32_t marcos) {
  if (!hay_globo) return -1;
  vballoon_objetivo(marcos);
  return 0;
}

uint64_t khv_footprint(void) {
  task_vm_info_data_t vi;
  mach_msg_type_number_t n = TASK_VM_INFO_COUNT;
  if (task_info(mach_task_self(), TASK_VM_INFO, (task_info_t)&vi, &n) != KERN_SUCCESS) return 0;
  return vi.phys_footprint;
}

// Contadores por hilo y por tipo de núcleo: PROC_PIDTHREADCOUNTS de
// <sys/proc_info_private.h> (macOS 13+), que el SDK no publica. Una entrada
// por nivel de rendimiento: la 0 son los núcleos P y la 1 los E. Los tiempos
// van en unidades de mach_absolute_time.
#define KHV_PROC_PIDTHREADCOUNTS 34
struct khv_ptc_datos {
  uint64_t instr, ciclos, user_mach, system_mach, energia_nj;
};
struct khv_ptc {
  uint16_t niveles, reservado0;
  uint32_t reservado1;
  struct khv_ptc_datos c[];
};

static uint64_t mach_a_ns(uint64_t t) { return (uint64_t)((__uint128_t)t * tb.numer / tb.denom); }

// khv_cpu_lectura da la CPU que han gastado los hilos de vCPU (dentro y fuera
// del invitado), lo que el regulador de CPU cobra a la VM: out[0] el total en
// ns, out[1] lo que corrió en núcleos P, out[2] las instrucciones y out[3] las
// de P. Con eso footprint.Contador cobra el tiempo en E a lo que vale en P
// (vz/internal/footprint/coste.go), como hace kling-vz con su auxiliar. Sin
// los contadores por núcleo (un macOS que no los dé), out[1..3] van a 0 y se
// cobra el tiempo tal cual; la elección se hace en la primera lectura y no
// cambia, para que el total no salte entre una fuente y otra.
void khv_cpu_lectura(uint64_t out[4]) {
  static int por_nucleo = -1;  // -1: sin decidir
  out[0] = out[1] = out[2] = out[3] = 0;
  pthread_mutex_lock(&mu);
  int n = n_creados;
  pthread_mutex_unlock(&mu);
  for (int i = 0; i < n; i++) {
    if (por_nucleo != 0) {
      uint64_t tid = 0;
      union {
        struct khv_ptc p;
        uint8_t b[sizeof(struct khv_ptc) + 4 * sizeof(struct khv_ptc_datos)];
      } u;
      int r = pthread_threadid_np(cpus[i].hilo, &tid) == 0
                  ? proc_pidinfo(getpid(), KHV_PROC_PIDTHREADCOUNTS, tid, &u, sizeof u)
                  : -1;
      if (r >= (int)(sizeof(struct khv_ptc) + 2 * sizeof(struct khv_ptc_datos)) && u.p.niveles >= 2) {
        por_nucleo = 1;
        for (int k = 0; k < u.p.niveles && k < 4; k++) {
          uint64_t t = mach_a_ns(u.p.c[k].user_mach + u.p.c[k].system_mach);
          out[0] += t;
          out[2] += u.p.c[k].instr;
          if (k == 0) out[1] += t, out[3] += u.p.c[k].instr;
        }
        continue;
      }
      if (por_nucleo == -1) {
        // La primera lectura decide: sin contadores por núcleo, a la cuenta
        // de siempre para todos los hilos.
        por_nucleo = 0;
        khv_cpu_lectura(out);
        return;
      }
      continue;  // un hilo que no se pudo leer esta vez (no debería pasar)
    }
    thread_basic_info_data_t bi;
    mach_msg_type_number_t cnt = THREAD_BASIC_INFO_COUNT;
    mach_port_t t = pthread_mach_thread_np(cpus[i].hilo);
    if (thread_info(t, THREAD_BASIC_INFO, (thread_info_t)&bi, &cnt) != KERN_SUCCESS) continue;
    out[0] += (uint64_t)(bi.user_time.seconds + bi.system_time.seconds) * 1000000000ULL +
              (uint64_t)(bi.user_time.microseconds + bi.system_time.microseconds) * 1000ULL;
  }
}

void khv_tiempos(double *primer_run_ms, double *restaurado_ms) {
  *primer_run_ms = t_primer_run;
  *restaurado_ms = t_restaurado;
}
