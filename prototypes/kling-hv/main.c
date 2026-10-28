// kling-hv: VMM de kindling para macOS sobre Hypervisor.framework (arm64).
//
// La RAM del invitado es un fichero mapeado con mmap(MAP_PRIVATE) y pasado a
// hv_vm_map: perezosa (solo entra lo que el invitado toca), copy-on-write (el
// fichero no cambia) y compartida entre procesos (las páginas limpias son las
// del vnode en la caché unificada). Ver prototypes/hvf-cow para las medidas.
//
// Uso:
//   kling-hv boot -k KERNEL -d DISCO[:ro] [-d ...] [-m MiB] [-c CPUS] [-a CMDLINE]
//                 [--mem FICHERO] [opciones comunes]
//   kling-hv restore DIR [opciones comunes]
// Opciones comunes:
//   -T                      anteponer ms desde el inicio del proceso a cada línea
//   -i TEXTO                escribir TEXTO en la consola del invitado al arrancar
//                           (\n se traduce a salto de línea)
//   --mark CADENA           acción cuando la consola muestra CADENA:
//   --on-mark exit|snap|stats   salir, hacer snapshot o imprimir memoria
//   --snap-dir DIR          directorio de salida del snapshot
//   --snap-after MS         snapshot a los MS ms (alternativa a --mark)
//   --stats-after MS        imprimir phys_footprint a los MS ms
//   --exit-after MS         salir a los MS ms
//   --snap-full             escribir toda la RAM en el snapshot (control)
#include <copyfile.h>
#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <semaphore.h>
#include <stdarg.h>
#include <string.h>
#include <sys/clonefile.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "khv.h"

// ===================== utilidades =====================

static uint64_t t_inicio;
static mach_timebase_info_data_t tb;

static double abs_a_ms(uint64_t d) { return (double)d * tb.numer / tb.denom / 1e6; }
double ms_desde_inicio(void) { return abs_a_ms(mach_absolute_time() - t_inicio); }

void morir(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  fprintf(stderr, "kling-hv: ");
  vfprintf(stderr, fmt, ap);
  fprintf(stderr, " (errno %d: %s)\n", errno, strerror(errno));
  va_end(ap);
  exit(1);
}

#define LOG(...)                                                \
  do {                                                          \
    fprintf(stderr, "[kling-hv %9.3f] ", ms_desde_inicio());    \
    fprintf(stderr, __VA_ARGS__);                               \
    fprintf(stderr, "\n");                                      \
  } while (0)

uint8_t *g_ram;
uint64_t g_ram_size;
static const char *g_mem_path;

void *gpa_ptr(uint64_t gpa, uint64_t len) {
  if (gpa < RAM_BASE || gpa - RAM_BASE + len > g_ram_size) return NULL;
  return g_ram + (gpa - RAM_BASE);
}

static void stats(const char *etapa) {
  task_vm_info_data_t vi;
  mach_msg_type_number_t n = TASK_VM_INFO_COUNT;
  task_info(mach_task_self(), TASK_VM_INFO, (task_info_t)&vi, &n);
  LOG("stats %s: phys_footprint=%.2f MiB resident=%.2f MiB internal=%.2f MiB external=%.2f MiB compressed=%.2f MiB",
      etapa, vi.phys_footprint / 1048576.0, vi.resident_size / 1048576.0, vi.internal / 1048576.0,
      vi.external / 1048576.0, vi.compressed / 1048576.0);
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

#define SNAP_MAGIC 0x4b4c4e4748563031ULL  // "KLNGHV01"
struct snap_hdr {
  uint64_t magic;
  uint32_t ncpus, ndisks;
  uint64_t ram_size;
  uint32_t uart_intid, disk_intid[VIRTIO_MAX];
  char disk[VIRTIO_MAX][512];
  uint8_t disk_ro[VIRTIO_MAX];
  uint64_t t_pausa;  // mach_absolute_time() al pausar
  struct uart_state uart;
  struct vblk_state vblk[VIRTIO_MAX];
  struct cpu_state cpu[MAX_CPUS];
  uint64_t gic_size;
};

// ===================== vCPU =====================

struct cpu {
  int idx;
  pthread_t hilo;
  hv_vcpu_t id;
  hv_vcpu_exit_t *exit;
  bool creado, on, on_pend;
  uint64_t on_entry, on_ctx;
  bool kick;
  struct cpu_state st;  // destino del guardado / origen de la restauración
  uint64_t vtimer_offset;
  uint64_t n_exits, n_mmio, n_wfi, n_vtimer;
};

static struct cpu cpus[MAX_CPUS];
static int ncpus = 1;
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;
static volatile bool pausa_pedida;
static int n_pausados, n_creados;
static bool arrancar;          // la VM puede empezar a correr
static bool restaurando;       // los hilos cargan cpu_state antes de correr
static uint64_t kernel_entry, fdt_gpa;
static double t_primer_run = -1;

void vcpu_kick_all(void) {
  pthread_mutex_lock(&mu);
  for (int i = 0; i < ncpus; i++) cpus[i].kick = true;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
}

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
  bool m;
  hv_vcpu_get_vtimer_mask(c->id, &m);
  s->vtimer_masked = m;
  s->on = c->on;
}

static uint64_t nuevo_vtimer_offset;  // calculado una vez al restaurar

static void cargar_cpu(struct cpu *c) {
  struct cpu_state *s = &c->st;
  for (int i = 0; i < 31; i++) OK(hv_vcpu_set_reg(c->id, HV_REG_X0 + i, s->x[i]));
  OK(hv_vcpu_set_reg(c->id, HV_REG_PC, s->pc));
  OK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, s->cpsr));
  OK(hv_vcpu_set_reg(c->id, HV_REG_FPCR, s->fpcr));
  OK(hv_vcpu_set_reg(c->id, HV_REG_FPSR, s->fpsr));
  for (int i = 0; i < 32; i++) OK(hv_vcpu_set_simd_fp_reg(c->id, HV_SIMD_FP_REG_Q0 + i, s->q[i]));
  for (size_t i = 0; i < NSYS; i++)
    if (s->sys_ok[i] && hv_vcpu_set_sys_reg(c->id, SYSREGS[i], s->sys[i]) != HV_SUCCESS)
      LOG("cpu%d: no se pudo restaurar sysreg %#x", c->idx, SYSREGS[i]);
  for (size_t i = 0; i < NICC; i++)
    if (s->icc_ok[i] && hv_gic_set_icc_reg(c->id, ICCREGS[i], s->icc[i]) != HV_SUCCESS)
      LOG("cpu%d: no se pudo restaurar ICC %#x", c->idx, ICCREGS[i]);
  OK(hv_vcpu_set_vtimer_offset(c->id, nuevo_vtimer_offset));
  c->vtimer_offset = nuevo_vtimer_offset;
  OK(hv_vcpu_set_vtimer_mask(c->id, s->vtimer_masked));
  c->on = s->on;
}

// Punto seguro: el hilo del vCPU guarda su estado y espera a que se reanude.
static void punto_pausa(struct cpu *c) {
  guardar_cpu(c);
  pthread_mutex_lock(&mu);
  n_pausados++;
  pthread_cond_broadcast(&cv);
  while (pausa_pedida) pthread_cond_wait(&cv, &mu);
  n_pausados--;
  pthread_mutex_unlock(&mu);
}

static void set_x(struct cpu *c, unsigned r, uint64_t v) {
  if (r < 31) OK(hv_vcpu_set_reg(c->id, HV_REG_X0 + r, v));
}
static uint64_t get_x(struct cpu *c, unsigned r) {
  uint64_t v = 0;
  if (r < 31) OK(hv_vcpu_get_reg(c->id, HV_REG_X0 + r, &v));
  return v;
}
static void avanzar_pc(struct cpu *c) {
  uint64_t pc;
  OK(hv_vcpu_get_reg(c->id, HV_REG_PC, &pc));
  OK(hv_vcpu_set_reg(c->id, HV_REG_PC, pc + 4));
}

static void mmio(struct cpu *c, uint64_t ipa, bool w, unsigned srt, int size, bool sse) {
  uint64_t v = w ? get_x(c, srt) : 0;
  if (w && size < 8) v &= (1ULL << (size * 8)) - 1;
  c->n_mmio++;
  if (ipa >= UART_BASE && ipa < UART_BASE + UART_SIZE) {
    uart_mmio(ipa - UART_BASE, w, &v);
  } else if (ipa >= VIRTIO_BASE && ipa < VIRTIO_BASE + VIRTIO_MAX * VIRTIO_STRIDE) {
    vblk_mmio((int)((ipa - VIRTIO_BASE) / VIRTIO_STRIDE), (ipa - VIRTIO_BASE) % VIRTIO_STRIDE, w, &v, size);
  } else {
    static int avisos;
    if (avisos++ < 10) LOG("cpu%d: MMIO sin dispositivo %s %#llx", c->idx, w ? "escritura" : "lectura", ipa);
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
      LOG("PSCI SYSTEM_OFF");
      return false;
    case 0x84000009:  // SYSTEM_RESET (reboot=k, panic=1)
      LOG("PSCI SYSTEM_RESET");
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

// WFI: dormir hasta el vencimiento del timer virtual o hasta que un dispositivo
// avise. Con varios vCPU las IPI las entrega el GIC del kernel sin pasar por
// aquí, así que la espera se acota para volver a mirar.
static void wfi(struct cpu *c) {
  c->n_wfi++;
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
  uint64_t fin = (uint64_t)ts.tv_sec * 1000000000ULL + ts.tv_nsec + tope_ns;
  ts.tv_sec = fin / 1000000000ULL;
  ts.tv_nsec = fin % 1000000000ULL;
  pthread_mutex_lock(&mu);
  if (!c->kick && !pausa_pedida) pthread_cond_timedwait(&cv, &mu, &ts);
  c->kick = false;
  pthread_mutex_unlock(&mu);
}

static void volcar(struct cpu *c, const char *por) {
  uint64_t pc, cpsr, far, esr;
  hv_vcpu_get_reg(c->id, HV_REG_PC, &pc);
  hv_vcpu_get_reg(c->id, HV_REG_CPSR, &cpsr);
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_FAR_EL1, &far);
  hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_ESR_EL1, &esr);
  LOG("cpu%d: %s syndrome=%#llx va=%#llx ipa=%#llx pc=%#llx cpsr=%#llx far_el1=%#llx esr_el1=%#llx", c->idx, por,
      c->exit->exception.syndrome, c->exit->exception.virtual_address, c->exit->exception.physical_address, pc, cpsr,
      far, esr);
}

static void fin_vm(int codigo);

static void *vcpu_hilo(void *arg) {
  struct cpu *c = arg;
  OK(hv_vcpu_create(&c->id, &c->exit, NULL));
  OK(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_MPIDR_EL1, (uint64_t)c->idx | (1ULL << 31)));
  pthread_mutex_lock(&mu);
  n_creados++;
  pthread_cond_broadcast(&cv);
  while (!arrancar) pthread_cond_wait(&cv, &mu);
  pthread_mutex_unlock(&mu);

  if (restaurando) {
    cargar_cpu(c);
  } else {
    // Arranque en frío: el kernel de la tabla del vCPU 0 empieza ya.
    OK(hv_vcpu_get_vtimer_offset(c->id, &c->vtimer_offset));
    if (c->idx == 0) {
      OK(hv_vcpu_set_reg(c->id, HV_REG_PC, kernel_entry));
      OK(hv_vcpu_set_reg(c->id, HV_REG_X0, fdt_gpa));
      OK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, 0x3c5));  // EL1h, DAIF enmascarado
      c->on = true;
    }
  }

  for (;;) {
    if (pausa_pedida) {
      punto_pausa(c);
      continue;
    }
    if (!c->on) {
      pthread_mutex_lock(&mu);
      while (!c->on_pend && !pausa_pedida) pthread_cond_wait(&cv, &mu);
      bool empezar = c->on_pend && !pausa_pedida;
      if (empezar) {
        c->on_pend = false;
        c->on = true;
      }
      pthread_mutex_unlock(&mu);
      if (!empezar) continue;
      OK(hv_vcpu_set_reg(c->id, HV_REG_PC, c->on_entry));
      OK(hv_vcpu_set_reg(c->id, HV_REG_X0, c->on_ctx));
      OK(hv_vcpu_set_reg(c->id, HV_REG_CPSR, 0x3c5));
    }
    if (t_primer_run < 0 && c->idx == 0) {
      t_primer_run = ms_desde_inicio();
    }
    OK(hv_vcpu_run(c->id));
    c->n_exits++;
    switch (c->exit->reason) {
      case HV_EXIT_REASON_CANCELED: break;
      case HV_EXIT_REASON_VTIMER_ACTIVATED:
        // Con el GIC de HVF el kernel inyecta el PPI 27; basta con desenmascarar.
        c->n_vtimer++;
        hv_vcpu_set_vtimer_mask(c->id, false);
        break;
      case HV_EXIT_REASON_EXCEPTION: {
        uint64_t syn = c->exit->exception.syndrome;
        unsigned ec = (syn >> 26) & 0x3f;
        switch (ec) {
          case 0x24: {  // data abort desde EL inferior: MMIO
            if (!(syn & (1u << 24))) {
              volcar(c, "data abort sin ISV");
              fin_vm(1);
            }
            int size = 1 << ((syn >> 22) & 3);
            mmio(c, c->exit->exception.physical_address, (syn >> 6) & 1, (syn >> 16) & 0x1f, size, (syn >> 21) & 1);
            avanzar_pc(c);
            break;
          }
          case 0x16:  // HVC: PSCI (el PC ya apunta a la siguiente instrucción)
            if (!psci(c)) fin_vm(0);
            break;
          case 0x17:  // SMC
            if (!psci(c)) fin_vm(0);
            avanzar_pc(c);
            break;
          case 0x18: {  // MSR/MRS atrapado: lecturas a 0, escrituras ignoradas
            unsigned rt = (syn >> 5) & 0x1f;
            static int avisos;
            if (avisos++ < 10) LOG("cpu%d: sysreg atrapado iss=%#llx", c->idx, syn & 0x1ffffff);
            if (syn & 1) set_x(c, rt, 0);
            avanzar_pc(c);
            break;
          }
          case 0x01:  // WFI/WFE
            if (!(syn & 1)) wfi(c);
            avanzar_pc(c);
            break;
          default:
            volcar(c, "excepción no tratada");
            fin_vm(1);
        }
        break;
      }
      default:
        LOG("cpu%d: salida desconocida %u", c->idx, c->exit->reason);
        fin_vm(1);
    }
  }
  return NULL;
}

static void crear_vcpus(void) {
  for (int i = 0; i < ncpus; i++) {
    cpus[i].idx = i;
    pthread_create(&cpus[i].hilo, NULL, vcpu_hilo, &cpus[i]);
  }
  pthread_mutex_lock(&mu);
  while (n_creados < ncpus) pthread_cond_wait(&cv, &mu);
  pthread_mutex_unlock(&mu);
}

static void soltar_vcpus(void) {
  pthread_mutex_lock(&mu);
  arrancar = true;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
}

// ===================== VM =====================

static uint32_t uart_intid, disk_intid[VIRTIO_MAX];
static int ndisks;
static char disk_path[VIRTIO_MAX][512];
static bool disk_ro[VIRTIO_MAX];

static void crear_vm(void) {
  uint64_t t0 = mach_absolute_time();
  OK(hv_vm_create(NULL));
  hv_gic_config_t g = hv_gic_config_create();
  OK(hv_gic_config_set_distributor_base(g, GIC_DIST_BASE));
  OK(hv_gic_config_set_redistributor_base(g, GIC_REDIST_BASE));
  OK(hv_gic_create(g));
  os_release(g);
  uint32_t spi_base, spi_n;
  OK(hv_gic_get_spi_interrupt_range(&spi_base, &spi_n));
  uart_intid = spi_base + 1;
  for (int i = 0; i < VIRTIO_MAX; i++) disk_intid[i] = spi_base + 16 + i;
  LOG("hv_vm_create + hv_gic_create: %.3f ms", abs_a_ms(mach_absolute_time() - t0));
}

// Mapea el fichero de memoria como RAM del invitado. crear: lo crea disperso.
static void mapear_ram(const char *ruta, uint64_t size, bool crear) {
  uint64_t t0 = mach_absolute_time();
  int fd = open(ruta, crear ? (O_RDWR | O_CREAT | O_TRUNC) : O_RDONLY, 0644);
  if (fd < 0) morir("open %s", ruta);
  if (crear && ftruncate(fd, (off_t)size) != 0) morir("ftruncate %s", ruta);
  struct stat st;
  fstat(fd, &st);
  if ((uint64_t)st.st_size < size) morir("%s mide %lld, se esperaban %llu", ruta, st.st_size, size);
  g_ram = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
  if (g_ram == MAP_FAILED) morir("mmap %s", ruta);
  close(fd);
  g_ram_size = size;
  g_mem_path = strdup(ruta);
  OK(hv_vm_map(g_ram, RAM_BASE, size, HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC));
  LOG("RAM %llu MiB = mmap(MAP_PRIVATE, %s) + hv_vm_map: %.3f ms", size >> 20, ruta,
      abs_a_ms(mach_absolute_time() - t0));
}

static void cargar_kernel(const char *ruta, const char *cmdline) {
  int fd = open(ruta, O_RDONLY);
  if (fd < 0) morir("open %s", ruta);
  struct stat st;
  fstat(fd, &st);
  uint8_t hdr[64];
  if (pread(fd, hdr, 64, 0) != 64 || memcmp(hdr + 56, "ARM\x64", 4) != 0) morir("%s no es un Image arm64", ruta);
  uint64_t text_offset, image_size;
  memcpy(&text_offset, hdr + 8, 8);
  memcpy(&image_size, hdr + 16, 8);
  kernel_entry = RAM_BASE + text_offset;
  if (text_offset + image_size + (4 << 20) > g_ram_size) morir("RAM insuficiente para el kernel");
  if (pread(fd, gpa_ptr(kernel_entry, st.st_size), st.st_size, 0) != st.st_size) morir("leer kernel");
  close(fd);

  struct fdt_params p = {.ram_size = g_ram_size, .ncpus = ncpus, .bootargs = cmdline, .uart_spi = uart_intid,
                         .nvirtio = ndisks};
  size_t sz;
  hv_gic_get_distributor_size(&sz);
  p.dist_size = sz;
  hv_gic_get_redistributor_size(&sz);
  p.redist_size = sz;
  for (int i = 0; i < ndisks; i++) p.virtio_spi[i] = disk_intid[i];
  // El DTB va al final de la RAM, alineado a 2 MiB y fuera de la imagen.
  fdt_gpa = RAM_BASE + g_ram_size - (2 << 20);
  size_t n = fdt_build(gpa_ptr(fdt_gpa, 1 << 20), 1 << 20, &p);
  LOG("kernel %lld bytes en %#llx (image_size %llu), DTB %zu bytes en %#llx", st.st_size, kernel_entry, image_size, n,
      fdt_gpa);
}

// ===================== snapshot =====================

static const char *snap_dir;
static bool snap_full;
static sem_t *sem_accion;
enum { ACC_NADA, ACC_EXIT, ACC_SNAP, ACC_STATS, ACC_INPUT };
static int accion_marca = ACC_EXIT;
static int cola[64];
static unsigned cola_r, cola_w;
static pthread_mutex_t cola_mu = PTHREAD_MUTEX_INITIALIZER;
static double t_marca = -1;

static void pedir_accion(int a) {
  pthread_mutex_lock(&cola_mu);
  cola[cola_w++ % 64] = a;
  pthread_mutex_unlock(&cola_mu);
  sem_post(sem_accion);
}
static int sacar_accion(void) {
  sem_wait(sem_accion);
  pthread_mutex_lock(&cola_mu);
  int a = cola[cola_r++ % 64];
  pthread_mutex_unlock(&cola_mu);
  return a;
}
static void marca_cb(int a) {
  if (a != ACC_INPUT && t_marca < 0) t_marca = ms_desde_inicio();
  pedir_accion(a);
}

static void pausar(void) {
  pthread_mutex_lock(&mu);
  pausa_pedida = true;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  hv_vcpu_t ids[MAX_CPUS];
  for (int i = 0; i < ncpus; i++) ids[i] = cpus[i].id;
  OK(hv_vcpus_exit(ids, ncpus));
  pthread_mutex_lock(&mu);
  while (n_pausados < ncpus) pthread_cond_wait(&cv, &mu);
  pthread_mutex_unlock(&mu);
}

__attribute__((unused)) static void reanudar(void) {
  pthread_mutex_lock(&mu);
  pausa_pedida = false;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
}

static void escribir_todo(int fd, const void *b, size_t n, off_t off) {
  while (n > 0) {
    ssize_t w = pwrite(fd, b, n, off);
    if (w <= 0) morir("pwrite");
    b = (const uint8_t *)b + w;
    n -= (size_t)w;
    off += w;
  }
}

static void hacer_snapshot(void) {
  if (!snap_dir) {
    LOG("snapshot pedido sin --snap-dir");
    return;
  }
  uint64_t t0 = mach_absolute_time();
  pausar();
  uint64_t t_pausa = mach_absolute_time();
  double ms_pausa = abs_a_ms(t_pausa - t0);

  // Estado de CPU (ya guardado por cada hilo), GIC y dispositivos.
  static struct snap_hdr h;
  memset(&h, 0, sizeof h);
  h.magic = SNAP_MAGIC;
  h.ncpus = ncpus;
  h.ndisks = ndisks;
  h.ram_size = g_ram_size;
  h.uart_intid = uart_intid;
  h.t_pausa = t_pausa;
  for (int i = 0; i < ndisks; i++) {
    h.disk_intid[i] = disk_intid[i];
    strncpy(h.disk[i], disk_path[i], sizeof h.disk[i] - 1);
    h.disk_ro[i] = disk_ro[i];
    vblk_get(i, &h.vblk[i]);
  }
  uart_get(&h.uart);
  for (int i = 0; i < ncpus; i++) h.cpu[i] = cpus[i].st;
  hv_gic_state_t gs = hv_gic_state_create();
  if (!gs) morir("hv_gic_state_create devolvió NULL");
  size_t gsz;
  OK(hv_gic_state_get_size(gs, &gsz));
  void *gdata = malloc(gsz);
  OK(hv_gic_state_get_data(gs, gdata));
  os_release(gs);
  h.gic_size = gsz;
  double ms_estado = abs_a_ms(mach_absolute_time() - t_pausa);

  mkdir(snap_dir, 0755);
  char ruta[1024];
  snprintf(ruta, sizeof ruta, "%s/state", snap_dir);
  FILE *f = fopen(ruta, "wb");
  if (!f) morir("fopen %s", ruta);
  fwrite(&h, sizeof h, 1, f);
  fwrite(gdata, gsz, 1, f);
  fclose(f);
  free(gdata);

  // Memoria: clon APFS del fichero base + las páginas que el invitado cambió
  // (copiadas por el CoW de MAP_PRIVATE). mincore da el mapa.
  uint64_t t1 = mach_absolute_time();
  snprintf(ruta, sizeof ruta, "%s/mem", snap_dir);
  unlink(ruta);
  if (clonefile(g_mem_path, ruta, 0) != 0 && copyfile(g_mem_path, ruta, NULL, COPYFILE_DATA) != 0)
    morir("clonar %s", g_mem_path);
  int fd = open(ruta, O_RDWR);
  if (fd < 0) morir("open %s", ruta);
  size_t np = g_ram_size / PAGINA, sucias = 0;
  unsigned char *v = malloc(np);
  if (mincore(g_ram, g_ram_size, (char *)v) != 0) morir("mincore");
  const unsigned char mascara = MINCORE_MODIFIED | MINCORE_COPIED | MINCORE_ANONYMOUS | MINCORE_PAGED_OUT;
  for (size_t i = 0; i < np;) {
    if (!snap_full && !(v[i] & mascara)) {
      i++;
      continue;
    }
    size_t j = i;
    while (j < np && (snap_full || (v[j] & mascara))) j++;
    escribir_todo(fd, g_ram + i * PAGINA, (j - i) * PAGINA, (off_t)(i * PAGINA));
    sucias += j - i;
    i = j;
  }
  free(v);
  close(fd);
  double ms_mem = abs_a_ms(mach_absolute_time() - t1);
  LOG("snapshot en %s: pausa %.3f ms, estado cpu+gic+disp %.3f ms (gic %zu bytes), memoria %.3f ms (%zu páginas "
      "de 16K = %.1f MiB escritas), total %.3f ms",
      snap_dir, ms_pausa, ms_estado, gsz, ms_mem, sucias, sucias * PAGINA / 1048576.0,
      abs_a_ms(mach_absolute_time() - t0));
}

static double t_restaurado = -1;

static void restaurar(const char *dir) {
  char ruta[1024];
  snprintf(ruta, sizeof ruta, "%s/state", dir);
  FILE *f = fopen(ruta, "rb");
  if (!f) morir("fopen %s", ruta);
  static struct snap_hdr h;
  if (fread(&h, sizeof h, 1, f) != 1 || h.magic != SNAP_MAGIC) morir("%s: estado inválido", ruta);
  void *gdata = malloc(h.gic_size);
  if (fread(gdata, h.gic_size, 1, f) != 1) morir("%s: estado del GIC truncado", ruta);
  fclose(f);

  ncpus = (int)h.ncpus;
  crear_vm();
  snprintf(ruta, sizeof ruta, "%s/mem", dir);
  mapear_ram(ruta, h.ram_size, false);
  uart_init(h.uart_intid);
  uart_set(&h.uart);
  ndisks = (int)h.ndisks;
  for (int i = 0; i < ndisks; i++) {
    strncpy(disk_path[i], h.disk[i], sizeof disk_path[i] - 1);
    disk_ro[i] = h.disk_ro[i];
    disk_intid[i] = h.disk_intid[i];
    vblk_init(i, disk_path[i], disk_ro[i], disk_intid[i]);
    vblk_set(i, &h.vblk[i]);
  }
  restaurando = true;
  crear_vcpus();
  OK(hv_gic_set_state(gdata, h.gic_size));
  free(gdata);
  for (int i = 0; i < ncpus; i++) cpus[i].st = h.cpu[i];
  // El contador virtual sigue donde se quedó: CNTVCT = mach_absolute_time - offset.
  uint64_t cnt = h.t_pausa - h.cpu[0].vtimer_offset;
  nuevo_vtimer_offset = mach_absolute_time() - cnt;
  t_restaurado = ms_desde_inicio();
  LOG("estado restaurado (gic %llu bytes, %d vCPU)", h.gic_size, ncpus);
  soltar_vcpus();
}

static void fin_vm(int codigo) {
  LOG("fin (código %d): primer hv_vcpu_run a %.3f ms; salidas cpu0=%llu mmio=%llu wfi=%llu vtimer=%llu", codigo,
      t_primer_run, cpus[0].n_exits, cpus[0].n_mmio, cpus[0].n_wfi, cpus[0].n_vtimer);
  fflush(stdout);
  _exit(codigo);
}

struct temporizador {
  double ms;
  int accion;
};
static void *temporizador_hilo(void *a) {
  struct temporizador *t = a;
  double falta = t->ms - ms_desde_inicio();
  if (falta > 0) usleep((useconds_t)(falta * 1000));
  pedir_accion(t->accion);
  return NULL;
}
static void programar(double ms, int accion) {
  struct temporizador *t = malloc(sizeof *t);
  t->ms = ms;
  t->accion = accion;
  pthread_t h;
  pthread_create(&h, NULL, temporizador_hilo, t);
  pthread_detach(h);
}

static void unescape(char *s) {
  char *o = s;
  for (; *s; s++) {
    if (s[0] == '\\' && s[1] == 'n') {
      *o++ = '\n';
      s++;
    } else *o++ = *s;
  }
  *o = 0;
}

static void uso(void) {
  fprintf(stderr,
          "uso: kling-hv boot -k KERNEL -d DISCO[:ro] [-m MiB] [-c CPUS] [-a CMDLINE] [--mem FICHERO] [comunes]\n"
          "     kling-hv restore DIR [comunes]\n"
          "comunes: -T -i TEXTO [--send-on CADENA] --mark CADENA --on-mark exit|snap|stats --snap-dir DIR\n"
          "         --snap-after MS"
          "         --stats-after MS --exit-after MS --snap-full\n");
  exit(2);
}

int main(int argc, char **argv) {
  t_inicio = mach_absolute_time();
  mach_timebase_info(&tb);
  setvbuf(stdout, NULL, _IOFBF, 1 << 16);
  if (argc < 2) uso();
  bool boot = strcmp(argv[1], "boot") == 0;
  if (!boot && strcmp(argv[1], "restore") != 0) uso();

  const char *kernel = NULL, *cmdline = "console=ttyS0 reboot=k panic=1 root=/dev/vda ro init=/bin/sh";
  const char *mem = "kling-hv.mem", *entrada = NULL, *marca = NULL, *send_on = NULL;
  uint64_t mib = 512;
  double snap_after = -1, stats_after = -1, exit_after = -1;
  static struct option largas[] = {{"mem", 1, 0, 'M'},        {"mark", 1, 0, 'K'},       {"on-mark", 1, 0, 'O'},
                                   {"snap-dir", 1, 0, 'D'},   {"snap-after", 1, 0, 'S'}, {"stats-after", 1, 0, 'P'},
                                   {"exit-after", 1, 0, 'E'}, {"snap-full", 0, 0, 'F'},  {"send-on", 1, 0, 'I'},
                                   {0, 0, 0, 0}};
  optind = 2;
  int o;
  while ((o = getopt_long(argc, argv, "k:d:m:c:a:Ti:", largas, NULL)) != -1) {
    switch (o) {
      case 'k': kernel = optarg; break;
      case 'd': {
        if (ndisks >= VIRTIO_MAX) uso();
        strncpy(disk_path[ndisks], optarg, sizeof disk_path[0] - 1);
        char *p = strrchr(disk_path[ndisks], ':');
        if (p && strcmp(p, ":ro") == 0) {
          *p = 0;
          disk_ro[ndisks] = true;
        }
        ndisks++;
        break;
      }
      case 'm': mib = strtoull(optarg, 0, 10); break;
      case 'c': ncpus = atoi(optarg); break;
      case 'a': cmdline = optarg; break;
      case 'T': g_uart_tiempos = true; break;
      case 'i': entrada = strdup(optarg); unescape((char *)entrada); break;
      case 'M': mem = optarg; break;
      case 'K': marca = optarg; break;
      case 'I': send_on = optarg; break;
      case 'O':
        accion_marca = strcmp(optarg, "snap") == 0 ? ACC_SNAP : strcmp(optarg, "stats") == 0 ? ACC_STATS : ACC_EXIT;
        break;
      case 'D': snap_dir = optarg; break;
      case 'S': snap_after = atof(optarg); break;
      case 'P': stats_after = atof(optarg); break;
      case 'E': exit_after = atof(optarg); break;
      case 'F': snap_full = true; break;
      default: uso();
    }
  }
  if (ncpus < 1 || ncpus > MAX_CPUS) uso();
  sem_unlink("/kling-hv");
  char semnom[64];
  snprintf(semnom, sizeof semnom, "/klinghv.%d", getpid());
  sem_accion = sem_open(semnom, O_CREAT | O_EXCL, 0600, 0);
  if (sem_accion == SEM_FAILED) morir("sem_open");
  sem_unlink(semnom);

  if (boot) {
    if (!kernel || ndisks == 0) uso();
    crear_vm();
    mapear_ram(mem, mib << 20, true);
    uart_init(uart_intid);
    for (int i = 0; i < ndisks; i++) vblk_init(i, disk_path[i], disk_ro[i], disk_intid[i]);
    cargar_kernel(kernel, cmdline);
    LOG("cmdline: %s", cmdline);
    crear_vcpus();
    soltar_vcpus();
  } else {
    if (optind >= argc) uso();
    restaurar(argv[optind]);
  }
  if (marca) uart_add_marca(marca, marca_cb, accion_marca);
  if (entrada && send_on) uart_add_marca(send_on, marca_cb, ACC_INPUT);
  else if (entrada) uart_rx_push(entrada, strlen(entrada));
  if (isatty(0)) uart_start_stdin();
  if (snap_after >= 0) programar(snap_after, ACC_SNAP);
  if (stats_after >= 0) programar(stats_after, ACC_STATS);
  if (exit_after >= 0) programar(exit_after, ACC_EXIT);

  bool enviada = false;
  for (;;) {
    switch (sacar_accion()) {
      case ACC_INPUT:
        if (!enviada) {
          enviada = true;
          LOG("entrada enviada a la consola");
          uart_rx_push(entrada, strlen(entrada));
        }
        break;
      case ACC_EXIT:
        LOG("salida pedida; marca %s vista a %.3f ms; restaurado a %.3f ms", marca ? marca : "-", t_marca,
            t_restaurado);
        stats("al salir");
        fin_vm(0);
      case ACC_SNAP:
        if (t_marca >= 0) LOG("marca %s vista a %.3f ms", marca, t_marca);
        hacer_snapshot();
        stats("tras snapshot");
        fin_vm(0);
      case ACC_STATS: stats("pedidas"); break;
    }
  }
}
