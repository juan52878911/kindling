//go:build darwin && arm64

// Pruebas del globo sin VM (globo_darwin_test.go): un driver de mentira que
// programa las colas por MMIO como el de Linux y comprueba cuántos tramos
// pide soltar. Sin Hypervisor.framework ram_soltar falla en hv_vm_unmap (no
// hay VM) sin tocar nada, pero cuenta el tramo: es lo que se mide.
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>

#include "khv.h"

#define G_RAM (1ULL << 20)  // 64 páginas del host

static void w(uint64_t off, uint64_t v) { vballoon_mmio(off, true, &v, 4); }

// Una cola split de num entradas en [base, base+0x3000) de la RAM de mentira:
// la tabla de descriptores, el anillo avail y el used.
static void cola(uint32_t idx, uint64_t base, uint32_t num) {
  w(0x030, idx);
  w(0x038, num);
  w(0x080, (uint32_t)(RAM_BASE + base));
  w(0x084, (uint32_t)((RAM_BASE + base) >> 32));
  w(0x090, (uint32_t)(RAM_BASE + base + 0x1000));
  w(0x094, (uint32_t)((RAM_BASE + base + 0x1000) >> 32));
  w(0x0a0, (uint32_t)(RAM_BASE + base + 0x2000));
  w(0x0a4, (uint32_t)((RAM_BASE + base + 0x2000) >> 32));
  w(0x044, 1);
}

static void desc(uint64_t base, int i, uint64_t addr, uint32_t len, uint16_t flags, uint16_t next) {
  struct vdesc d = {.addr = addr, .len = len, .flags = flags, .next = next};
  memcpy(g_ram + base + 16 * (uint64_t)i, &d, sizeof d);
}

// Pone head en el avail de la cola de base y la notifica.
static void ofrecer(uint32_t idx, uint64_t base, uint16_t head) {
  uint16_t *avail = (uint16_t *)(g_ram + base + 0x1000);
  avail[2 + avail[1] % 8] = head;
  __atomic_thread_fence(__ATOMIC_RELEASE);
  avail[1]++;
  w(0x050, idx);
}

static uint16_t usados(uint64_t base) { return *(uint16_t *)(g_ram + base + 0x2000 + 2); }

// khv_prueba_globo: out[0] tramos al inflar 128 marcos en orden descendente
// (la mitad alta de la RAM, 32 páginas contiguas: 1 tramo; búfer a búfer y
// solo hacia arriba eran 32), out[1] tramos de un informe de dos bloques
// contiguos desordenados (1), out[2] búferes devueltos por el informe (1) y
// out[3] tramos al desinflar (0). Devuelve 0, o -1 si no hay RAM.
int khv_prueba_globo(uint64_t out[4]) {
  if (!g_ram) {
    uint8_t *ram = mmap(NULL, G_RAM, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
    if (ram == MAP_FAILED) return -1;
    g_ram = ram;
    g_ram_size = G_RAM;
  }
  if (g_ram_size < G_RAM || vballoon_init(60, false) != 0) return -1;
  memset(g_ram, 0, 0x10000);
  uint64_t c[KHV_BALLOON_NCUENTAS];
  // El driver: reinicio, características (stats y el informe) y colas.
  w(0x070, 0);
  w(0x070, 1 | 2);
  w(0x024, 0);
  w(0x020, (1u << 1) | (1u << 5));
  w(0x070, 1 | 2 | 8);
  cola(0, 0x0000, 8);  // inflate
  cola(1, 0x3000, 8);  // deflate
  cola(3, 0x6000, 8);  // informe: la 3, con la de estadísticas negociada
  w(0x070, 1 | 2 | 4 | 8);

  // Inflar: 128 marcos de [512 KiB, 1 MiB) de arriba abajo, como los saca
  // el buddy, en un búfer; el objetivo es justo eso.
  uint32_t *pfns = (uint32_t *)(g_ram + 0x9000);
  for (int k = 0; k < 128; k++) pfns[k] = (uint32_t)((RAM_BASE + G_RAM - 4096 * (uint64_t)(k + 1)) >> 12);
  vballoon_objetivo(128);
  khv_balloon_cuentas(c);
  uint64_t t0 = c[2];
  desc(0x0000, 0, RAM_BASE + 0x9000, 128 * 4, 0, 0);
  ofrecer(0, 0x0000, 0);
  khv_balloon_cuentas(c);
  out[0] = c[2] - t0;

  // Informe: [256 KiB, 384 KiB) y [128 KiB, 256 KiB), en una cadena.
  t0 = c[2];
  desc(0x6000, 0, RAM_BASE + 0x40000, 0x20000, VIRTQ_DESC_F_WRITE | VIRTQ_DESC_F_NEXT, 1);
  desc(0x6000, 1, RAM_BASE + 0x20000, 0x20000, VIRTQ_DESC_F_WRITE, 0);
  ofrecer(3, 0x6000, 0);
  khv_balloon_cuentas(c);
  out[1] = c[2] - t0;
  out[2] = usados(0x6000);

  // Desinflar la mitad: no hay nada que soltar.
  t0 = c[2];
  desc(0x3000, 0, RAM_BASE + 0x9000, 64 * 4, 0, 0);
  ofrecer(1, 0x3000, 0);
  khv_balloon_cuentas(c);
  out[3] = c[2] - t0;
  w(0x070, 0);  // el driver se va
  return 0;
}
