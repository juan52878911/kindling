// hvcow: spike de viabilidad de restore perezoso copy-on-write con
// Hypervisor.framework en Apple Silicon.
//
// Mapea un fichero con mmap(MAP_PRIVATE) como RAM del invitado (hv_vm_map),
// arranca un vCPU con la MMU apagada que lee una palabra por página de 16K y
// escribe un bloque, y mide qué se hizo residente, qué se copió y si el fichero
// cambió. Modo "alloc" como control: hv_vm_allocate + copiar el fichero entero
// (lo que hace hoy Virtualization.framework con la RAM de un snapshot).
//
// Uso:
//   hvcow create FICHERO MiB            crea el fichero con el patrón (sin caché)
//   hvcow run [opciones] FICHERO        corre el invitado y mide
// Opciones de run:
//   -m file|alloc   origen de la RAM del invitado (por defecto file)
//   -r MiB          MiB a leer, una palabra por página (por defecto 1)
//   -R MiB          desplazamiento de la lectura dentro del fichero (0)
//   -w KiB          KiB a escribir, palabra a palabra (por defecto 64)
//   -W MiB          desplazamiento de la escritura (por defecto 128)
//   -h segundos     esperar antes de salir (para medir N procesos a la vez)
//   -t              ejecutar footprint y vmmap --summary sobre el propio pid
#include <Hypervisor/Hypervisor.h>
#include <errno.h>
#include <fcntl.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <mach/mach_vm.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#define PAGINA 16384ULL
#define IPA_CODIGO 0x10000ULL
#define IPA_RAM 0x40000000ULL
#define VALOR_ESCRITO 0xC0FFEE00DEADBEEFULL

// Código del invitado (aarch64, EL1, MMU apagada). El anfitrión carga los
// registros antes de arrancar:
//   x0 = IPA de lectura, x1 = nº de páginas a leer, x2 = paso (16K)
//   x6 = IPA de escritura, x7 = nº de palabras a escribir, x5 = valor
// Devuelve en x4 la suma de las palabras leídas (para comprobar que el
// invitado vio el contenido real del fichero). Sale con HVC #0.
__asm__(
    ".text\n"
    ".p2align 2\n"
    ".globl _invitado_ini\n"
    "_invitado_ini:\n"
    "  mov x4, #0\n"
    "1: cbz x1, 2f\n"
    "  ldr x3, [x0]\n"
    "  add x4, x4, x3\n"
    "  add x0, x0, x2\n"
    "  sub x1, x1, #1\n"
    "  b 1b\n"
    "2: cbz x7, 3f\n"
    "  str x5, [x6], #8\n"
    "  sub x7, x7, #1\n"
    "  b 2b\n"
    "3: hvc #0\n"
    "  b 3b\n"
    ".globl _invitado_fin\n"
    "_invitado_fin:\n");
extern const char invitado_ini[], invitado_fin[];

#define OK(x)                                                              \
  do {                                                                     \
    hv_return_t _r = (x);                                                  \
    if (_r != HV_SUCCESS) {                                                \
      fprintf(stderr, "%s falló: 0x%x\n", #x, (unsigned)_r);               \
      exit(1);                                                             \
    }                                                                      \
  } while (0)

// Patrón: cada palabra de 8 bytes vale su desplazamiento en palabras XOR una
// constante; así se distingue cualquier página de cualquier otra.
static inline uint64_t patron(uint64_t off) {
  return (off / 8) ^ 0xA5A5A5A55A5A5A5AULL;
}

static double ms_desde(uint64_t t0) {
  static mach_timebase_info_data_t tb;
  if (tb.denom == 0) mach_timebase_info(&tb);
  return (double)(mach_absolute_time() - t0) * tb.numer / tb.denom / 1e6;
}

static void medir(const char *etapa, void *mapa, size_t tam) {
  task_vm_info_data_t vi;
  mach_msg_type_number_t n = TASK_VM_INFO_COUNT;
  if (task_info(mach_task_self(), TASK_VM_INFO, (task_info_t)&vi, &n) != KERN_SUCCESS) {
    perror("task_info");
    exit(1);
  }
  size_t paginas = tam / PAGINA, incore = 0, mod = 0, copiadas = 0, otros = 0;
  if (mapa) {
    // mincore trabaja con la página del sistema (16K en Apple Silicon).
    char *v = malloc(tam / getpagesize());
    if (mincore(mapa, tam, v) != 0) {
      perror("mincore");
      exit(1);
    }
    for (size_t i = 0; i < tam / getpagesize(); i++) {
      if (v[i] & MINCORE_INCORE) incore++;
      if (v[i] & MINCORE_MODIFIED) mod++;
      if (v[i] & MINCORE_COPIED) copiadas++;
      if (v[i] & MINCORE_REFERENCED_OTHER) otros++;
    }
    free(v);
  }
  printf("[%-12s] phys_footprint=%7.2f MiB resident=%7.2f MiB internal=%7.2f MiB external=%7.2f MiB",
         etapa, vi.phys_footprint / 1048576.0, vi.resident_size / 1048576.0,
         vi.internal / 1048576.0, vi.external / 1048576.0);
  if (mapa)
    printf(" | mincore: incore=%zu/%zu mod=%zu copied=%zu ref_other=%zu", incore, paginas,
           mod, copiadas, otros);
  printf("\n");
  if (mapa) {
    // Vista de Mach de la región: modo de compartición y cuántos mapeos
    // referencian el objeto VM (el del vnode si la página no se ha copiado).
    mach_vm_address_t dir = (mach_vm_address_t)mapa;
    mach_vm_size_t t;
    vm_region_extended_info_data_t ei;
    mach_msg_type_number_t c = VM_REGION_EXTENDED_INFO_COUNT;
    mach_port_t obj;
    if (mach_vm_region(mach_task_self(), &dir, &t, VM_REGION_EXTENDED_INFO, (vm_region_info_t)&ei,
                       &c, &obj) == KERN_SUCCESS)
      printf("               region: share_mode=%u (1=COW 2=PRV 3=EMPTY 4=SHM 6=TRUESHARED) ref_count=%u "
             "pages_resident=%u shared_now_private=%u dirtied=%u shadow_depth=%u\n",
             ei.share_mode, ei.ref_count, ei.pages_resident, ei.pages_shared_now_private,
             ei.pages_dirtied, ei.shadow_depth);
  }
  fflush(stdout);
}

static void herramientas(const char *etapa) {
  char cmd[512];
  printf("--- footprint/vmmap (%s) ---\n", etapa);
  fflush(stdout);
  snprintf(cmd, sizeof cmd,
           "footprint -p %d 2>/dev/null | grep -E 'phys_footprint:|TOTAL|IOKit|VM_ALLOCATE|mapped file' | head -8",
           getpid());
  system(cmd);
  snprintf(cmd, sizeof cmd,
           "vmmap --summary %d 2>/dev/null | grep -E 'Physical footprint|mapped file|VM_ALLOCATE|^TOTAL' | head -8",
           getpid());
  system(cmd);
  fflush(stdout);
}

static int crear(const char *ruta, size_t mib) {
  int fd = open(ruta, O_CREAT | O_TRUNC | O_WRONLY, 0644);
  if (fd < 0) {
    perror("open");
    return 1;
  }
  // F_NOCACHE: que el fichero no quede en la caché unificada al escribirlo, para
  // que la primera lectura del invitado venga de verdad del disco.
  fcntl(fd, F_NOCACHE, 1);
  size_t trozo = 4 << 20;
  uint64_t *buf;
  posix_memalign((void **)&buf, PAGINA, trozo);
  for (size_t off = 0; off < mib << 20; off += trozo) {
    for (size_t i = 0; i < trozo / 8; i++) buf[i] = patron(off + i * 8);
    if (write(fd, buf, trozo) != (ssize_t)trozo) {
      perror("write");
      return 1;
    }
  }
  fsync(fd);
  close(fd);
  free(buf);
  printf("creado %s (%zu MiB)\n", ruta, mib);
  return 0;
}

int main(int argc, char **argv) {
  if (argc >= 4 && strcmp(argv[1], "create") == 0) return crear(argv[2], strtoul(argv[3], 0, 10));
  if (argc < 3 || strcmp(argv[1], "run") != 0) {
    fprintf(stderr, "uso: hvcow create FICHERO MiB | hvcow run [-m file|alloc] [-r MiB] [-R MiB] [-w KiB] [-W MiB] [-h s] [-t] FICHERO\n");
    return 2;
  }
  const char *modo = "file";
  size_t leer = 1, offl = 0, escribir = 64, offe = 128;
  int espera = 0, tools = 0, c;
  optind = 2;
  while ((c = getopt(argc, argv, "m:r:R:w:W:h:t")) != -1) {
    switch (c) {
      case 'm': modo = optarg; break;
      case 'r': leer = strtoul(optarg, 0, 10); break;
      case 'R': offl = strtoul(optarg, 0, 10); break;
      case 'w': escribir = strtoul(optarg, 0, 10); break;
      case 'W': offe = strtoul(optarg, 0, 10); break;
      case 'h': espera = atoi(optarg); break;
      case 't': tools = 1; break;
      default: return 2;
    }
  }
  const char *ruta = argv[optind];
  int fd = open(ruta, O_RDONLY);
  if (fd < 0) {
    perror("open");
    return 1;
  }
  struct stat st;
  fstat(fd, &st);
  size_t tam = st.st_size;
  if (((offl + leer) << 20) > tam || (offe << 20) + (escribir << 10) > tam) {
    fprintf(stderr, "rangos fuera del fichero\n");
    return 2;
  }
  printf("pid=%d modo=%s fichero=%s tam=%zu MiB leer=%zu MiB@%zu escribir=%zu KiB@%zu MiB\n",
         getpid(), modo, ruta, tam >> 20, leer, offl, escribir, offe);
  medir("inicio", NULL, 0);

  uint64_t t0 = mach_absolute_time();
  OK(hv_vm_create(NULL));
  printf("hv_vm_create: %.3f ms\n", ms_desde(t0));

  // Página de código aparte, para no ensuciar la RAM del invitado.
  void *codigo = mmap(NULL, PAGINA, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
  memcpy(codigo, invitado_ini, invitado_fin - invitado_ini);
  OK(hv_vm_map(codigo, IPA_CODIGO, PAGINA, HV_MEMORY_READ | HV_MEMORY_EXEC));

  void *ram;
  t0 = mach_absolute_time();
  if (strcmp(modo, "file") == 0) {
    ram = mmap(NULL, tam, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
    if (ram == MAP_FAILED) {
      perror("mmap");
      return 1;
    }
    printf("mmap MAP_PRIVATE: %.3f ms\n", ms_desde(t0));
  } else {
    // Control: memoria anónima de HVF y copia completa del fichero.
    OK(hv_vm_allocate(&ram, tam, HV_ALLOCATE_DEFAULT));
    for (size_t off = 0; off < tam;) {
      ssize_t n = pread(fd, (char *)ram + off, tam - off, off);
      if (n <= 0) {
        perror("pread");
        return 1;
      }
      off += n;
    }
    printf("hv_vm_allocate + pread de %zu MiB: %.3f ms\n", tam >> 20, ms_desde(t0));
  }
  medir("tras mmap", ram, tam);

  t0 = mach_absolute_time();
  OK(hv_vm_map(ram, IPA_RAM, tam, HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC));
  printf("hv_vm_map: %.3f ms\n", ms_desde(t0));
  medir("tras vm_map", ram, tam);
  if (tools) herramientas("tras vm_map");

  hv_vcpu_t vcpu;
  hv_vcpu_exit_t *salida;
  OK(hv_vcpu_create(&vcpu, &salida, NULL));
  OK(hv_vcpu_set_reg(vcpu, HV_REG_PC, IPA_CODIGO));
  OK(hv_vcpu_set_reg(vcpu, HV_REG_CPSR, 0x3c5));  // EL1h, DAIF enmascarado
  OK(hv_vcpu_set_reg(vcpu, HV_REG_X0, IPA_RAM + (offl << 20)));
  OK(hv_vcpu_set_reg(vcpu, HV_REG_X1, (leer << 20) / PAGINA));
  OK(hv_vcpu_set_reg(vcpu, HV_REG_X2, PAGINA));
  OK(hv_vcpu_set_reg(vcpu, HV_REG_X5, VALOR_ESCRITO));
  OK(hv_vcpu_set_reg(vcpu, HV_REG_X6, IPA_RAM + (offe << 20)));
  OK(hv_vcpu_set_reg(vcpu, HV_REG_X7, (escribir << 10) / 8));

  t0 = mach_absolute_time();
  OK(hv_vcpu_run(vcpu));
  double ms = ms_desde(t0);
  uint64_t ec = (salida->exception.syndrome >> 26) & 0x3f;
  uint64_t suma, pc;
  hv_vcpu_get_reg(vcpu, HV_REG_X4, &suma);
  hv_vcpu_get_reg(vcpu, HV_REG_PC, &pc);
  printf("hv_vcpu_run: %.3f ms, exit reason=%u EC=0x%llx (0x16=HVC) pc=0x%llx\n", ms,
         salida->reason, ec, pc);
  if (salida->reason != HV_EXIT_REASON_EXCEPTION || ec != 0x16) {
    fprintf(stderr, "salida inesperada, syndrome=0x%llx va=0x%llx ipa=0x%llx\n",
            salida->exception.syndrome, salida->exception.virtual_address,
            salida->exception.physical_address);
    return 1;
  }

  // Comprobaciones: el invitado leyó el contenido del fichero y su escritura es
  // visible en el mapeo del anfitrión.
  uint64_t esperada = 0;
  for (size_t p = 0; p < (leer << 20) / PAGINA; p++) esperada += patron((offl << 20) + p * PAGINA);
  int escrita_ok = 1;
  uint64_t *w = (uint64_t *)((char *)ram + (offe << 20));
  for (size_t i = 0; i < (escribir << 10) / 8; i++)
    if (w[i] != VALOR_ESCRITO) escrita_ok = 0;
  printf("suma leída por el invitado %s; escritura visible en el anfitrión: %s\n",
         suma == esperada ? "CORRECTA" : "INCORRECTA", escrita_ok ? "sí" : "NO");
  medir("tras run", ram, tam);
  if (tools) herramientas("tras run");

  if (espera) {
    printf("esperando %d s\n", espera);
    fflush(stdout);
    sleep(espera);
    medir("fin espera", ram, tam);
  }
  hv_vcpu_destroy(vcpu);
  hv_vm_destroy();
  return 0;
}
