//go:build darwin && arm64

// Banco de pruebas de los dispositivos virtio contra un invitado hostil, sin
// Hypervisor.framework: una RAM de mentira, los registros MMIO programados
// desde los datos de la prueba (colas de cualquier tamaño, anillos que se
// salen de la RAM o se pisan) y un notify en cada cola. Lo usa el fuzz de
// hvvm_fuzz_darwin_test.go: nada de esto debe colgar ni romper el proceso.
// Sin VM, hv_gic_set_spi falla sin más (no hay GIC) y el globo no puede
// remapear (no hay fichero de RAM): los caminos de las colas son los mismos.
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "khv.h"

#define PRUEBA_RAM (1ULL << 20)

static uint16_t rd16(const uint8_t *p) { return (uint16_t)(p[0] | p[1] << 8); }
static uint32_t rd32(const uint8_t *p) { return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24; }

typedef bool (*mmio_fn)(uint64_t off, bool escribir, uint64_t *val, int size);

static bool blk_mmio(uint64_t off, bool escribir, uint64_t *val, int size) { return vblk_mmio(0, off, escribir, val, size); }

static void w(mmio_fn f, uint64_t off, uint64_t v) { f(off, true, &v, 4); }

int khv_prueba(const uint8_t *d, size_t n) {
  static bool listo;
  if (!listo) {
    g_ram_size = PRUEBA_RAM;
    g_ram = calloc(1, PRUEBA_RAM);
    if (!g_ram) return -1;
    char ruta[] = "/tmp/kling-hv-prueba.XXXXXX";
    int fd = mkstemp(ruta);
    if (fd < 0) return -1;
    if (ftruncate(fd, 1 << 20) != 0) return -1;
    close(fd);
    int r = vblk_init(0, ruta, false, 48);
    unlink(ruta);
    if (r != 0) return -1;
    static const uint8_t mac[6] = {6, 0, 0xac, 0x10, 0, 2};
    vnet_init(-1, mac, 49);
    vrng_init(50);
    vsock_init(51);
    if (vballoon_init(52, true) != 0) return -1;
    listo = true;
  }
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
