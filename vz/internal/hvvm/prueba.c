//go:build darwin && arm64

// Banco de pruebas de los dispositivos virtio contra un invitado hostil, sin
// Hypervisor.framework: una RAM de mentira, los registros MMIO programados
// desde los datos de la prueba (colas de cualquier tamaño, anillos que se
// salen de la RAM o se pisan) y un notify en cada cola. Lo usa el fuzz de
// hvvm_fuzz_darwin_test.go: nada de esto debe colgar ni romper el proceso.
// Sin VM, hv_gic_set_spi falla sin más (no hay GIC) y el globo no puede
// remapear (no hay fichero de RAM): los caminos de las colas son los mismos.
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

#include "khv.h"

#define PRUEBA_RAM (1ULL << 20)

static uint16_t rd16(const uint8_t *p) { return (uint16_t)(p[0] | p[1] << 8); }
static uint32_t rd32(const uint8_t *p) { return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24; }

typedef bool (*mmio_fn)(uint64_t off, bool escribir, uint64_t *val, int size);

static bool blk_mmio(uint64_t off, bool escribir, uint64_t *val, int size) { return vblk_mmio(0, off, escribir, val, size); }

static void w(mmio_fn f, uint64_t off, uint64_t v) { f(off, true, &v, 4); }

// La RAM de mentira lleva detrás una página sin permisos: lo que se salga de
// ella por poco (un anillo mal medido) rompe el proceso aquí mismo, sin
// esperar a que el desbordamiento pise algo que se note.
static int preparar(void) {
  static bool listo;
  if (!listo) {
    size_t pag = (size_t)getpagesize();
    uint8_t *ram = mmap(NULL, PRUEBA_RAM + pag, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
    if (ram == MAP_FAILED || mprotect(ram + PRUEBA_RAM, pag, PROT_NONE) != 0) return -1;
    g_ram_size = PRUEBA_RAM;
    g_ram = ram;
    char ruta[] = "/tmp/kling-hv-prueba.XXXXXX";
    int fd = mkstemp(ruta);
    if (fd < 0) return -1;
    if (ftruncate(fd, 1 << 20) != 0) return -1;
    close(fd);
    int r = vblk_init(0, ruta, false, false, 48);
    unlink(ruta);
    if (r != 0) return -1;
    static const uint8_t mac[6] = {6, 0, 0xac, 0x10, 0, 2};
    vnet_init(-1, mac, 16384, 49);
    vrng_init(50);
    vsock_init(51);
    if (vballoon_init(52, true) != 0) return -1;
    listo = true;
  }
  return 0;
}

int khv_prueba(const uint8_t *d, size_t n) {
  if (preparar() != 0) return -1;
  // Las primeras 64 B programan el dispositivo; el resto es la RAM.
  if (n < 64) return 0;
  memset(g_ram, 0, g_ram_size);
  size_t l = n - 64 < g_ram_size ? n - 64 : g_ram_size;
  memcpy(g_ram, d + 64, l);

  mmio_fn devs[] = {blk_mmio, vnet_mmio, vrng_mmio, vsock_mmio, vballoon_mmio};
  int ncolas[] = {1, 2, 1, 3, 2};
  int k = d[0] % 5;
  mmio_fn f = devs[k];
  w(f, 0x070, 0);  // reset: cada prueba empieza de cero
  for (int q = 0; q < ncolas[k]; q++) {
    const uint8_t *c = d + 2 + q * 14;  // num (2) y tres direcciones (4)
    w(f, 0x030, (uint64_t)q);
    w(f, 0x038, rd16(c));
    // Direcciones dentro de la RAM, en su borde o fuera, según los datos.
    uint64_t base = RAM_BASE - 0x1000;
    w(f, 0x080, (uint32_t)(base + rd32(c + 2) % (PRUEBA_RAM + 0x2000)));
    w(f, 0x090, (uint32_t)(base + rd32(c + 6) % (PRUEBA_RAM + 0x2000)));
    w(f, 0x0a0, (uint32_t)(base + rd32(c + 10) % (PRUEBA_RAM + 0x2000)));
    w(f, 0x084, 0);
    w(f, 0x094, 0);
    w(f, 0x0a4, 0);
    w(f, 0x044, 1);
  }
  w(f, 0x070, 0xf);  // ACKNOWLEDGE | DRIVER | DRIVER_OK | FEATURES_OK
  // Accesos sueltos a registros y configuración (offsets y tamaños al azar).
  for (int i = 0; i < 3; i++) {
    const uint8_t *a = d + 46 + i * 6;
    uint64_t v = rd32(a + 2);
    int sz = 1 << (a[1] & 3);
    f(rd16(a) % 0x200, a[1] & 4, &v, sz);
  }
  for (int q = 0; q < ncolas[k]; q++) w(f, 0x050, (uint64_t)q);
  if (k == 0) vblk_prueba(0);
  if (k == 4) vballoon_objetivo(rd32(d + 60));
  w(f, 0x064, 0xff);  // InterruptACK
  w(f, 0x070, 0);
  return 0;
}

// ---- carrera de los registros de la cola contra el hilo del disco ----

static volatile bool carrera_fin;

// El invitado hostil: con la cola en marcha, la desarma, le pone el tamaño
// máximo y la vuelve a armar, una y otra vez, desde otro vCPU.
static void *carrera_vcpu(void *a) {
  (void)a;
  while (!carrera_fin) {
    w(blk_mmio, 0x044, 0);
    w(blk_mmio, 0x038, QMAX);
    w(blk_mmio, 0x044, 1);
    w(blk_mmio, 0x038, 4);  // con la cola lista: debe ignorarse
    w(blk_mmio, 0x044, 0);
    w(blk_mmio, 0x038, 4);
    w(blk_mmio, 0x044, 1);
  }
  return NULL;
}

// khv_prueba_carrera atiende iters lecturas del disco mientras otro hilo
// cambia el QueueNum. El used de 4 entradas acaba justo al final de la RAM:
// un avail_event calculado con el QueueNum vivo (256) en vez del que comprobó
// vq_ptrs (4) cae 2 KiB más allá, en la página de guarda. Devuelve cuántas
// peticiones se respondieron, o -1 si no se pudo preparar; también comprueba
// que con la cola lista el QueueNum no cambia (-2 si cambió).
int khv_prueba_carrera(int iters) {
  if (preparar() != 0) return -1;
  memset(g_ram, 0, g_ram_size);
  uint64_t desc = RAM_BASE, avail = RAM_BASE + 0x1000, used = RAM_BASE + PRUEBA_RAM - 40;
  w(blk_mmio, 0x070, 0);
  w(blk_mmio, 0x024, 0);
  w(blk_mmio, 0x020, 1u << 29);  // VIRTIO_RING_F_EVENT_IDX
  w(blk_mmio, 0x030, 0);
  w(blk_mmio, 0x038, 4);
  w(blk_mmio, 0x080, (uint32_t)desc);
  w(blk_mmio, 0x090, (uint32_t)avail);
  w(blk_mmio, 0x0a0, (uint32_t)used);
  w(blk_mmio, 0x084, 0);
  w(blk_mmio, 0x094, 0);
  w(blk_mmio, 0x0a4, 0);
  w(blk_mmio, 0x044, 1);
  w(blk_mmio, 0x070, 0xf);
  // Con la cola lista, ni el tamaño ni las direcciones se mueven.
  w(blk_mmio, 0x038, QMAX);
  w(blk_mmio, 0x0a0, (uint32_t)(used - 0x1000));
  struct vblk_state st;
  vblk_get(0, &st);
  if (st.q.num != 4 || st.q.used != used) return -2;

  // Una lectura de un sector: cabecera (16 B), datos (512 B) y estado (1 B).
  struct vdesc *d = gpa_ptr(desc, 3 * sizeof *d);
  d[0] = (struct vdesc){RAM_BASE + 0x2000, 16, VIRTQ_DESC_F_NEXT, 1};
  d[1] = (struct vdesc){RAM_BASE + 0x3000, 512, VIRTQ_DESC_F_NEXT | VIRTQ_DESC_F_WRITE, 2};
  d[2] = (struct vdesc){RAM_BASE + 0x4000, 1, VIRTQ_DESC_F_WRITE, 0};
  uint16_t *av = gpa_ptr(avail, 6 + 2 * 4);
  carrera_fin = false;
  pthread_t h;
  if (pthread_create(&h, NULL, carrera_vcpu, NULL) != 0) return -1;
  int hechas = 0;
  for (int i = 0; i < iters; i++) {
    av[2 + (uint16_t)i % 4] = 0;
    __atomic_store_n(&av[1], (uint16_t)(i + 1), __ATOMIC_RELEASE);
    vblk_prueba(0);
    hechas = *(volatile uint16_t *)gpa_ptr(used + 2, 2);
  }
  carrera_fin = true;
  pthread_join(h, NULL);
  w(blk_mmio, 0x070, 0);
  return hechas;
}
