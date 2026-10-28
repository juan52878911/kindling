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

static double abs_a_ms(uint64_t d) { return (double)d * tb.numer / tb.denom / 1e6; }
double ms_desde_inicio(void) { return abs_a_ms(mach_absolute_time() - t_inicio); }

void fallo(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(errbuf, sizeof errbuf, fmt, ap);
  va_end(ap);
}

const char *khv_error(void) { return errbuf; }

// TRY: llamada a Hypervisor.framework en la preparación (devuelve -1).
#define TRY(x)                                                \
  do {                                                        \
    hv_return_t _r = (x);                                     \
    if (_r != HV_SUCCESS) {                                   \
      fallo("%s failed: 0x%x", #x, (unsigned)_r);             \
      return -1;                                              \
    }                                                         \
  } while (0)

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

static void logf_(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
static int log_fd = 2;
static void logf_(const char *fmt, ...) {
  char b[512];
  int n = snprintf(b, sizeof b, "kling-hv: ");
  va_list ap;
  va_start(ap, fmt);
  n += vsnprintf(b + n, sizeof b - (size_t)n, fmt, ap);
  va_end(ap);
  if (n > (int)sizeof b - 2) n = (int)sizeof b - 2;
  b[n++] = '\n';
  uart_flush();
  (void)!write(log_fd, b, (size_t)n);
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
// 2 añade vsock al final de la cabecera; una de la versión 1 (sin él) se lee
// como un prefijo.
#define SNAP_VERSION 2
struct snap_hdr {
  uint64_t magic;
  uint32_t version, ncpus, ndisks;
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
  struct cpu_state cpu[MAX_CPUS];
  uint64_t gic_size;
  // versión 2
  uint8_t hay_vsock;
  struct vvsock_state vsock;
  uint8_t hay_globo;
  struct vballoon_state globo;
};
#define SNAP_HDR_V1 offsetof(struct snap_hdr, hay_vsock)

struct snap_pie {
  uint64_t magic, ram_size, hdr_off, hdr_len;
};

// ===================== configuración =====================

static int ncpus = 1;
static int ndisks;
static char *disk_path[KHV_MAX_DISKS];
static bool disk_ro[KHV_MAX_DISKS];
static bool hay_red, hay_rng, hay_vsock, hay_globo;
static int net_fd = -1;
static uint8_t net_mac[6];
static char *kernel_path, *cmdline, *ram_dir;
static uint32_t uart_intid, rtc_intid, virtio_intid[VIRTIO_MAX];
static int slot_net = -1, slot_rng = -1, slot_vsock = -1, slot_globo = -1, nvirtio;

// ===================== vCPU =====================

struct cpu {
  int idx;
  pthread_t hilo;
  hv_vcpu_t id;
  hv_vcpu_exit_t *exit;
  bool on, on_pend;
  uint64_t on_entry, on_ctx;
  bool kick;
  struct cpu_state st;  // destino del guardado / origen de la restauración
  uint64_t vtimer_offset;
};

static struct cpu cpus[MAX_CPUS];
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;
static pthread_mutex_t op_mu = PTHREAD_MUTEX_INITIALIZER;  // serializa pausa, reanudación y snapshot
static unsigned pausa_bits;      // con mu
static volatile bool pausa_pedida;
static volatile bool parar;      // la VM terminó: los hilos de vCPU salen
static int n_pausados, n_creados;
static bool arrancar;            // la VM puede empezar a correr
static bool restaurando;         // los hilos cargan cpu_state antes de correr
static bool liberada;            // ya se soltaron los vCPU (con mu)
static uint64_t kernel_entry, fdt_gpa;
static uint64_t nuevo_vtimer_offset;  // calculado al soltar una restauración
static uint64_t snap_t_pausa;         // de la restauración pendiente
static uint64_t t_pausado;            // mach_absolute_time() de la última pausa
static double t_primer_run = -1, t_restaurado = -1;

static pthread_mutex_t fin_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t fin_cv = PTHREAD_COND_INITIALIZER;
static bool fin_hecho;
static int fin_codigo;

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

void fin_vm(int codigo, const char *motivo) {
  pthread_mutex_lock(&fin_mu);
  bool primero = !fin_hecho;
  if (primero) {
    fin_hecho = true;
    fin_codigo = codigo;
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

int khv_wait(void) {
  pthread_mutex_lock(&fin_mu);
  while (!fin_hecho) pthread_cond_wait(&fin_cv, &fin_mu);
  int c = fin_codigo;
  pthread_mutex_unlock(&fin_mu);
  return c;
}

void khv_stop(void) { fin_vm(0, "stopped"); }

static void guardar_cpu(struct cpu *c) {
  struct cpu_state *s = &c->st;
  for (int i = 0; i < 31; i++) hv_vcpu_get_reg(c->id, HV_REG_X0 + i, &s->x[i]);
  hv_vcpu_get_reg(c->id, HV_REG_PC, &s->pc);
  hv_vcpu_get_reg(c->id, HV_REG_CPSR, &s->cpsr);
  hv_vcpu_get_reg(c->id, HV_REG_FPCR, &s->fpcr);
  hv_vcpu_get_reg(c->id, HV_REG_FPSR, &s->fpsr);
  for (int i = 0; i < 32; i++) hv_vcpu_get_simd_fp_reg(c->id, HV_SIMD_FP_REG_Q0 + i, &s->q[i]);
  for (size_t i = 0; i < NSYS; i++) s->sys_ok[i] = hv_vcpu_get_sys_reg(c->id, SYSREGS[i], &s->sys[i]) == HV_SUCCESS;
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

// Punto seguro: el hilo del vCPU guarda su estado y espera a que se reanude.
static void punto_pausa(struct cpu *c) {
  guardar_cpu(c);
  pthread_mutex_lock(&mu);
  n_pausados++;
  pthread_cond_broadcast(&cv);
  while (pausa_pedida && !parar) pthread_cond_wait(&cv, &mu);
  n_pausados--;
  pthread_mutex_unlock(&mu);
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
    uart_mmio(ipa - UART_BASE, w, &v);
  } else if (ipa >= RTC_BASE && ipa < RTC_BASE + RTC_SIZE) {
    rtc_mmio(ipa - RTC_BASE, w, &v, size);
  } else if (ipa >= VIRTIO_BASE && ipa < VIRTIO_BASE + VIRTIO_MAX * VIRTIO_STRIDE) {
    int slot = (int)((ipa - VIRTIO_BASE) / VIRTIO_STRIDE);
    uint64_t off = (ipa - VIRTIO_BASE) % VIRTIO_STRIDE;
    if (slot < ndisks) vblk_mmio(slot, off, w, &v, size);
    else if (slot == slot_net) vnet_mmio(off, w, &v, size);
    else if (slot == slot_rng) vrng_mmio(off, w, &v, size);
    else if (slot == slot_vsock) vsock_mmio(off, w, &v, size);
    else if (slot == slot_globo) vballoon_mmio(off, w, &v, size);
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

// Devuelve false si la VM debe terminar.
static bool psci(struct cpu *c) {
  uint64_t fn = get_x(c, 0), r = PSCI_RET_NOT_SUPPORTED;
  switch ((uint32_t)fn) {
    case 0x84000000: r = 0x00010000; break;  // PSCI_VERSION 1.0
    case 0x84000001:
    case 0xC4000001: r = 0; break;  // CPU_SUSPEND: como un WFI que ya despertó
    case 0x84000002:                // CPU_OFF
      pthread_mutex_lock(&mu);
      c->on = false;
      pthread_mutex_unlock(&mu);
      return true;
    case 0x84000003:
    case 0xC4000003: {  // CPU_ON
      uint64_t t = get_x(c, 1) & 0xff;
      if (t >= (uint64_t)ncpus) {
        r = PSCI_RET_INVALID;
        break;
      }
      pthread_mutex_lock(&mu);
      if (cpus[t].on || cpus[t].on_pend) r = PSCI_RET_ALREADY_ON;
      else {
        cpus[t].on_entry = get_x(c, 2);
        cpus[t].on_ctx = get_x(c, 3);
        cpus[t].on_pend = true;
        pthread_cond_broadcast(&cv);
        r = 0;
      }
      pthread_mutex_unlock(&mu);
      break;
    }
    case 0x84000004:
    case 0xC4000004: {  // AFFINITY_INFO
      uint64_t t = get_x(c, 1) & 0xff;
      r = t < (uint64_t)ncpus ? (cpus[t].on ? 0 : 1) : PSCI_RET_INVALID;
      break;
    }
    case 0x84000006: r = 2; break;  // MIGRATE_INFO_TYPE: no hace falta migrar
    case 0x84000008:                // SYSTEM_OFF
    case 0x84000009:                // SYSTEM_RESET (reboot=k, panic=1)
      return false;
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
  return true;
}

// WFI de respaldo: con el GIC de HVF el framework resuelve el WFI dentro y
// esto no se usa (medido en el prototipo). Si llega, dormir hasta el
// vencimiento del timer virtual o hasta que un dispositivo avise; con varios
// vCPU las IPI no pasan por aquí, así que la espera se acota.
static void wfi(struct cpu *c) {
  uint64_t ctl = 0, cval = 0;
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_CNTV_CTL_EL0, &ctl);
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_CNTV_CVAL_EL0, &cval);
  uint64_t ahora = mach_absolute_time();
  uint64_t tope_ns = ncpus > 1 ? 2000000 : 1000000000;  // 2 ms con SMP, 1 s si no
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

static void *vcpu_hilo(void *arg) {
  struct cpu *c = arg;
  hv_return_t r = hv_vcpu_create(&c->id, &c->exit, NULL);
  if (r == HV_SUCCESS) r = hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_MPIDR_EL1, (uint64_t)c->idx | (1ULL << 31));
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
    cargar_cpu(c);
  } else {
    // Arranque en frío: el kernel empieza en el vCPU 0.
    VOK(hv_vcpu_get_vtimer_offset(c->id, &c->vtimer_offset));
    if (c->idx == 0) {
      VOK(hv_vcpu_set_reg(c->id, HV_REG_PC, kernel_entry));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_X0, fdt_gpa));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, 0x3c5));  // EL1h, DAIF enmascarado
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
      VOK(hv_vcpu_set_reg(c->id, HV_REG_PC, c->on_entry));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_X0, c->on_ctx));
      VOK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, 0x3c5));
    }
    if (t_primer_run < 0 && c->idx == 0) t_primer_run = ms_desde_inicio();
    VOK(hv_vcpu_run(c->id));
    switch (c->exit->reason) {
      case HV_EXIT_REASON_CANCELED: break;
      case HV_EXIT_REASON_VTIMER_ACTIVATED:
        // Con el GIC de HVF el kernel inyecta el PPI 27; basta con desenmascarar.
        hv_vcpu_set_vtimer_mask(c->id, false);
        break;
      case HV_EXIT_REASON_EXCEPTION: {
        uint64_t syn = c->exit->exception.syndrome;
        unsigned ec = (syn >> 26) & 0x3f;
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
          case 0x16:  // HVC: PSCI (el PC ya apunta a la siguiente instrucción)
            if (!psci(c)) {
              fin_vm(0, "the guest powered off");
              return NULL;
            }
            break;
          case 0x17:  // SMC
            if (!psci(c)) {
              fin_vm(0, "the guest powered off");
              return NULL;
            }
            avanzar_pc(c);
            break;
          case 0x18: {  // MSR/MRS atrapado: lecturas a 0, escrituras ignoradas
            unsigned rt = (syn >> 5) & 0x1f;
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
  return 0;
}

static void dispositivos_quietos(void) {
  for (int i = 0; i < ndisks; i++) vblk_quieto(i);
  if (hay_red) vnet_quieto();
  if (hay_vsock) vsock_quieto();
}

static void dispositivos_despertar(void) {
  for (int i = 0; i < ndisks; i++) vblk_despertar(i);
  if (hay_red) vnet_despertar();
  if (hay_vsock) vsock_despertar();
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
  liberada = true;
  arrancar = true;
  g_pausa = false;
  pthread_cond_broadcast(&cv);
}

// ===================== VM =====================

static bool vm_tocada;
bool khv_vm_tocada(void) { return vm_tocada; }

int khv_create(const struct khv_config *c) {
  t_inicio = mach_absolute_time();
  mach_timebase_info(&tb);
  if (c->ncpus < 1 || c->ncpus > MAX_CPUS) {
    fallo("vcpu_count must be between 1 and %d", MAX_CPUS);
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
  ncpus = c->ncpus;
  g_ram_size = c->mem_size;
  ndisks = c->ndisks;
  hay_red = c->net_fd >= 0;
  hay_rng = c->rng != 0;
  hay_vsock = c->vsock != 0;
  hay_globo = c->balloon != 0;
  net_fd = c->net_fd;
  memcpy(net_mac, c->mac, 6);
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

  // Desde aquí un fallo deja el proceso inservible para otra VM
  // (Hypervisor.framework admite una por proceso y no hay vuelta atrás).
  vm_tocada = true;
  TRY(hv_vm_create(NULL));
  hv_gic_config_t g = hv_gic_config_create();
  TRY(hv_gic_config_set_distributor_base(g, GIC_DIST_BASE));
  TRY(hv_gic_config_set_redistributor_base(g, GIC_REDIST_BASE));
  TRY(hv_gic_create(g));
  os_release(g);
  uint32_t spi_base, spi_n;
  TRY(hv_gic_get_spi_interrupt_range(&spi_base, &spi_n));
  uart_intid = spi_base + 1;
  rtc_intid = spi_base + 2;
  for (int i = 0; i < VIRTIO_MAX; i++) virtio_intid[i] = spi_base + 16 + (uint32_t)i;

  uart_init(uart_intid, c->console_fd);
  log_fd = c->console_fd >= 0 ? c->console_fd : 2;
  rtc_init();
  for (int i = 0; i < ndisks; i++) {
    disk_path[i] = strdup(c->disks[i].path);
    disk_ro[i] = c->disks[i].ro != 0;
    if (vblk_init(i, disk_path[i], disk_ro[i], virtio_intid[i]) != 0) return -1;
  }
  if (hay_red) vnet_init(net_fd, net_mac, virtio_intid[slot_net]);
  if (hay_rng) vrng_init(virtio_intid[slot_rng]);
  if (hay_vsock) vsock_init(virtio_intid[slot_vsock]);
  if (hay_globo && vballoon_init(virtio_intid[slot_globo], c->balloon == 2) != 0) return -1;
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
// que también se rehace. Si eso falla, la RAM del invitado queda rota: la VM
// termina.
int ram_soltar(uint64_t off, uint64_t len) {
  if (!g_ram || off > g_ram_size || len > g_ram_size - off || (off | len) % PAGINA) return -1;
  if (mmap(g_ram + off, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_FIXED, base_fd, (off_t)off) == MAP_FAILED)
    return -1;
  if (hv_vm_unmap(RAM_BASE + off, len) != HV_SUCCESS ||
      hv_vm_map(g_ram + off, RAM_BASE + off, len, HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC) != HV_SUCCESS) {
    fin_vm(1, "remapping guest RAM for the balloon failed");
    return -1;
  }
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
                         .rtc_spi = rtc_intid, .nvirtio = nvirtio};
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
  if (cargar_kernel() != 0) return -1;
  if (crear_vcpus() != 0) return -1;
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

int khv_restore(const char *mem_path) {
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
      (pie.hdr_len != sizeof h && pie.hdr_len != SNAP_HDR_V1) || pie.hdr_off > (uint64_t)st.st_size ||
      pie.hdr_len > (uint64_t)st.st_size - pie.hdr_off ||
      (memset(&h, 0, sizeof h), leer_todo(fd, &h, pie.hdr_len, (off_t)pie.hdr_off)) != 0 || h.magic != SNAP_MAGIC) {
    close(fd);
    fallo("%s is not a kling-hv memory file (or was written by another version)", mem_path);
    return -1;
  }
  if (h.version != SNAP_VERSION && !(h.version == 1 && pie.hdr_len == SNAP_HDR_V1)) {
    close(fd);
    fallo("%s: snapshot state version %u, this kling-hv reads %u", mem_path, h.version, SNAP_VERSION);
    return -1;
  }
  if (h.version == 1) {
    // Una cabecera anterior no tiene vsock ni globo: el invitado nunca vio
    // esos dispositivos (van detrás de los demás), así que no existen.
    hay_vsock = hay_globo = false;
  }
  if (h.ram_size != g_ram_size || (int)h.ncpus != ncpus || (int)h.ndisks != ndisks || (bool)h.hay_red != hay_red ||
      (bool)h.hay_rng != hay_rng || (bool)h.hay_vsock != hay_vsock || (bool)h.hay_globo != hay_globo ||
      h.uart_intid != uart_intid) {
    close(fd);
    fallo("the snapshot does not match the configured machine (ram %llu/%llu, cpus %u/%d, disks %u/%d, net %u/%d, "
          "rng %u/%d, vsock %u/%d, balloon %u/%d)",
          h.ram_size, g_ram_size, h.ncpus, ncpus, h.ndisks, ndisks, h.hay_red, hay_red, h.hay_rng, hay_rng,
          h.hay_vsock, hay_vsock, h.hay_globo, hay_globo);
    return -1;
  }
  void *gdata = h.gic_size <= (uint64_t)st.st_size ? malloc(h.gic_size) : NULL;
  if (!gdata || leer_todo(fd, gdata, h.gic_size, (off_t)(pie.hdr_off + pie.hdr_len)) != 0) {
    free(gdata);
    close(fd);
    fallo("%s: GIC state truncated", mem_path);
    return -1;
  }
  if (mapear_ram(fd) != 0) {
    free(gdata);
    return -1;
  }
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
  restaurando = true;
  if (crear_vcpus() != 0) {
    free(gdata);
    return -1;
  }
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
  for (int i = 0; i < ncpus; i++) cpus[i].st = h.cpu[i];
  snap_t_pausa = h.t_pausa;
  // Queda en pausa hasta khv_resume: los PATCH de discos del núcleo ya
  // llegaron antes de crear la VM, pero el núcleo reanuda cuando quiere.
  return 0;
}

int khv_pause(unsigned bit) {
  pthread_mutex_lock(&op_mu);
  pthread_mutex_lock(&mu);
  unsigned antes = pausa_bits;
  pausa_bits |= bit;
  if (antes != 0 || !liberada || parar) {
    pthread_mutex_unlock(&mu);
    pthread_mutex_unlock(&op_mu);
    return 0;
  }
  pausa_pedida = true;
  g_pausa = true;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  salir_vcpus();
  pthread_mutex_lock(&mu);
  while (n_pausados < ncpus && !parar) pthread_cond_wait(&cv, &mu);
  t_pausado = mach_absolute_time();
  pthread_mutex_unlock(&mu);
  dispositivos_quietos();
  uart_flush();
  pthread_mutex_unlock(&op_mu);
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
      pausa_pedida = false;
      g_pausa = false;
      pthread_cond_broadcast(&cv);
      despertar = true;
    }
  }
  pthread_mutex_unlock(&mu);
  if (despertar) dispositivos_despertar();
  pthread_mutex_unlock(&op_mu);
  return parar ? -1 : 0;
}

int khv_snapshot(const char *mem_out) {
  pthread_mutex_lock(&op_mu);
  int ret = -1;
  int fd = -1;
  void *gdata = NULL;
  unsigned char *v = NULL;
  static struct snap_hdr h;
  pthread_mutex_lock(&mu);
  bool en_pausa = liberada && pausa_pedida && n_pausados == ncpus;
  pthread_mutex_unlock(&mu);
  if (!en_pausa) {
    fallo("the VM must be paused to snapshot it");
    goto fin;
  }
  memset(&h, 0, sizeof h);
  h.magic = SNAP_MAGIC;
  h.version = SNAP_VERSION;
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
  for (int i = 0; i < ncpus; i++) h.cpu[i] = cpus[i].st;
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

  // Memoria: clon del fichero que respalda la RAM + las páginas que el
  // invitado cambió (copiadas por el CoW de MAP_PRIVATE). mincore da el mapa.
  if (fclonefileat(base_fd, AT_FDCWD, mem_out, 0) == 0) {
    fd = open(mem_out, O_RDWR);
  } else {
    // Otro volumen o sin clones: copia de los datos.
    fd = open(mem_out, O_RDWR | O_CREAT | O_EXCL, 0644);
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
  size_t np = g_ram_size / PAGINA;
  v = malloc(np);
  if (!v || mincore(g_ram, g_ram_size, (char *)v) != 0) {
    fallo("mincore: %s", strerror(errno));
    goto fin;
  }
  // También las páginas residentes que el invitado solo leyó (MINCORE_INCORE):
  // el clon es otro vnode y no comparte la caché unificada con su base, así
  // que una página que no se escribe aquí la tendría que leer del SSD el
  // invitado restaurado al tocarla. Con la congelación de una máquina ya
  // restaurada eso eran 60-90 ms de fallos de página en el primer /resync
  // tras el thaw (2-4 ms escribiéndolas). Lo escrito queda en la caché.
  const unsigned char mascara =
      MINCORE_INCORE | MINCORE_MODIFIED | MINCORE_COPIED | MINCORE_ANONYMOUS | MINCORE_PAGED_OUT;
  // Lo inflado en el globo no se escribe: al invitado no le importa qué tenga.
  for (size_t i = 0; i < np; i++)
    if (hay_globo && vballoon_inflada(i)) v[i] = 0;
  for (size_t i = 0; i < np;) {
    if (!(v[i] & mascara)) {
      i++;
      continue;
    }
    size_t j = i;
    while (j < np && (v[j] & mascara)) j++;
    if (escribir_todo(fd, g_ram + i * PAGINA, (j - i) * PAGINA, (off_t)(i * PAGINA)) != 0) {
      fallo("writing %s: %s", mem_out, strerror(errno));
      goto fin;
    }
    i = j;
  }
  struct snap_pie pie = {.magic = SNAP_MAGIC, .ram_size = g_ram_size, .hdr_off = g_ram_size, .hdr_len = sizeof h};
  if (escribir_todo(fd, &h, sizeof h, (off_t)g_ram_size) != 0 ||
      escribir_todo(fd, gdata, gsz, (off_t)(g_ram_size + sizeof h)) != 0 ||
      escribir_todo(fd, &pie, sizeof pie, (off_t)(g_ram_size + sizeof h + gsz)) != 0) {
    fallo("writing the state to %s: %s", mem_out, strerror(errno));
    goto fin;
  }
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

// khv_cpu_ns es la CPU que han gastado los hilos de vCPU (dentro y fuera del
// invitado): lo que el regulador de CPU cobra a la VM.
uint64_t khv_cpu_ns(void) {
  uint64_t total = 0;
  pthread_mutex_lock(&mu);
  int n = n_creados;
  pthread_mutex_unlock(&mu);
  for (int i = 0; i < n; i++) {
    thread_basic_info_data_t bi;
    mach_msg_type_number_t cnt = THREAD_BASIC_INFO_COUNT;
    mach_port_t t = pthread_mach_thread_np(cpus[i].hilo);
    if (thread_info(t, THREAD_BASIC_INFO, (thread_info_t)&bi, &cnt) != KERN_SUCCESS) continue;
    total += (uint64_t)(bi.user_time.seconds + bi.system_time.seconds) * 1000000000ULL +
             (uint64_t)(bi.user_time.microseconds + bi.system_time.microseconds) * 1000ULL;
  }
  return total;
}

void khv_tiempos(double *primer_run_ms, double *restaurado_ms) {
  *primer_run_ms = t_primer_run;
  *restaurado_ms = t_restaurado;
}
