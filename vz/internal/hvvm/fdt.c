//go:build darwin && arm64

// Generador de device tree (FDT v17) en memoria: lo justo para que el kernel
// de kindling arranque (memoria, cpus, GICv3, timer, PSCI, 16550, PL031,
// virtio-mmio).
#define __STDC_WANT_LIB_EXT1__ 1  // memset_s: borrar las semillas sin que el compilador lo quite
#include <stdlib.h>
#include <string.h>

#include "khv.h"

static uint8_t st[16384];  // bloque de estructura
static size_t nst;
static char strs[2048];  // bloque de cadenas
static size_t nstrs;
static bool desbordado;  // algo no cupo en st/strs: fdt_build lo rechaza

// cabe: si hay sitio para n bytes más en el bloque de estructura (con el
// relleno a 4); si no, lo deja anotado y no se copia nada. La línea de
// comandos del kernel viene del host, pero una muy larga no debe pisar
// memoria del proceso.
static bool cabe(size_t n) {
  if (nst + n + 4 <= sizeof st) return true;
  desbordado = true;
  return false;
}

static void put32(uint32_t v) {
  if (!cabe(4)) return;
  st[nst++] = v >> 24;
  st[nst++] = v >> 16;
  st[nst++] = v >> 8;
  st[nst++] = v;
}

static void pad4(void) {
  while (nst & 3) st[nst++] = 0;
}

static uint32_t str_off(const char *s) {
  for (size_t i = 0; i < nstrs; i += strlen(strs + i) + 1)
    if (strcmp(strs + i, s) == 0) return (uint32_t)i;
  size_t o = nstrs;
  if (nstrs + strlen(s) + 1 > sizeof strs) {
    desbordado = true;
    return 0;
  }
  memcpy(strs + nstrs, s, strlen(s) + 1);
  nstrs += strlen(s) + 1;
  return (uint32_t)o;
}

static void node(const char *name) {
  put32(1);
  if (!cabe(strlen(name) + 1)) return;
  memcpy(st + nst, name, strlen(name) + 1);
  nst += strlen(name) + 1;
  pad4();
}

static void end(void) { put32(2); }

static void prop(const char *name, const void *data, size_t len) {
  put32(3);
  put32((uint32_t)len);
  put32(str_off(name));
  if (!cabe(len)) return;
  memcpy(st + nst, data, len);
  nst += len;
  pad4();
}

static void prop_str(const char *name, const char *v) { prop(name, v, strlen(v) + 1); }
static void prop_empty(const char *name) { prop(name, "", 0); }

// Propiedad de celdas de 32 bits en big-endian.
static void prop_cells(const char *name, const uint32_t *c, int n) {
  uint32_t be[32];
  for (int i = 0; i < n; i++) be[i] = __builtin_bswap32(c[i]);
  prop(name, be, n * 4);
}

static void prop_u32(const char *name, uint32_t v) { prop_cells(name, &v, 1); }

static void prop_reg(uint64_t base, uint64_t size) {
  uint32_t c[4] = {base >> 32, (uint32_t)base, size >> 32, (uint32_t)size};
  prop_cells("reg", c, 4);
}

// Bytes de rng-seed: con 32 (256 bits acreditados) el crng ya queda listo; 64
// dan margen.
#define FDT_RNG_SEED 64

#define PH_GIC 1
#define PH_CLK 2

size_t fdt_build(void *buf, size_t cap, const struct fdt_params *p) {
  nst = nstrs = 0;
  desbordado = false;
  char nombre[64];

  node("");
  prop_u32("#address-cells", 2);
  prop_u32("#size-cells", 2);
  prop_str("compatible", "linux,dummy-virt");
  prop_u32("interrupt-parent", PH_GIC);

  // Semillas nuevas en cada arranque en frío: kaslr-seed para que arm64
  // reubique el núcleo (sin ella KASLR no tiene de dónde sacar el azar, HVF
  // no ofrece RNDR) y rng-seed para que el crng arranque inicializado sin
  // esperar al virtio-rng (el núcleo solo la acredita con
  // CONFIG_RANDOM_TRUST_BOOTLOADER, ver config-common). Las dos las borra el
  // núcleo al leerlas (kaslr-seed a 0, rng-seed con FDT_NOP), así que el
  // DTB que queda en la RAM, y con él los snapshots y sus clones, no las
  // lleva. Los clones parten de un snapshot y no pasan por aquí.
  uint64_t kaslr;
  uint8_t semilla[FDT_RNG_SEED];
  arc4random_buf(&kaslr, sizeof kaslr);
  arc4random_buf(semilla, sizeof semilla);
  node("chosen");
  prop_str("bootargs", p->bootargs);
  snprintf(nombre, sizeof nombre, "/uart@%llx", UART_BASE);
  prop_str("stdout-path", nombre);
  prop("kaslr-seed", &kaslr, sizeof kaslr);  // u64; da igual el orden de bytes
  prop("rng-seed", semilla, sizeof semilla);
  end();
  memset_s(&kaslr, sizeof kaslr, 0, sizeof kaslr);
  memset_s(semilla, sizeof semilla, 0, sizeof semilla);

  snprintf(nombre, sizeof nombre, "memory@%llx", RAM_BASE);
  node(nombre);
  prop_str("device_type", "memory");
  prop_reg(RAM_BASE, p->ram_size);
  end();

  node("cpus");
  prop_u32("#address-cells", 1);
  prop_u32("#size-cells", 0);
  for (int i = 0; i < p->ncpus; i++) {
    // reg es la afinidad del MPIDR (Aff1 y Aff0), la que le pone vm.c.
    uint32_t aff = (uint32_t)khv_mpidr_aff(i);
    snprintf(nombre, sizeof nombre, "cpu@%x", aff);
    node(nombre);
    prop_str("device_type", "cpu");
    prop_str("compatible", "arm,arm-v8");
    prop_str("enable-method", "psci");
    prop_u32("reg", aff);
    end();
  }
  end();

  node("psci");
  static const char psci_compat[] = "arm,psci-1.0\0arm,psci-0.2";
  prop("compatible", psci_compat, sizeof psci_compat);
  // Con EL2 el kernel corre en EL2 y su HVC no sale del invitado (va a su
  // propio vector); el SMC sí lo atrapa HVF (vm.c, EC 0x17).
  prop_str("method", p->nested ? "smc" : "hvc");
  end();

  snprintf(nombre, sizeof nombre, "intc@%llx", GIC_DIST_BASE);
  node(nombre);
  prop_str("compatible", "arm,gic-v3");
  prop_u32("#interrupt-cells", 3);
  prop_empty("interrupt-controller");
  {
    uint64_t rs = p->redist_size * p->ncpus;
    uint32_t c[8] = {0, (uint32_t)GIC_DIST_BASE, 0, (uint32_t)p->dist_size, 0, (uint32_t)GIC_REDIST_BASE, rs >> 32, (uint32_t)rs};
    prop_cells("reg", c, 8);
  }
  if (p->nested) {
    // Interrupción de mantenimiento del interfaz de virtualización (ICH_*):
    // PPI 9 (HV_GIC_INT_MAINTENANCE = INTID 25). Sin ella KVM no usa el vGIC.
    uint32_t c[3] = {1, 9, 4};
    prop_cells("interrupts", c, 3);
  }
  prop_u32("phandle", PH_GIC);
  end();

  node("timer");
  prop_str("compatible", "arm,armv8-timer");
  {
    // PPI: seguro físico 13, no seguro físico 14, virtual 11, hipervisor 10.
    uint32_t c[12] = {1, 13, 4, 1, 14, 4, 1, 11, 4, 1, 10, 4};
    prop_cells("interrupts", c, 12);
  }
  prop_empty("always-on");
  end();

  node("apb-pclk");
  prop_str("compatible", "fixed-clock");
  prop_u32("#clock-cells", 0);
  prop_u32("clock-frequency", 24000000);
  prop_u32("phandle", PH_CLK);
  end();

  // PL031: lo mismo que pone Firecracker en arm64 (un primecell con su reloj).
  snprintf(nombre, sizeof nombre, "rtc@%llx", RTC_BASE);
  node(nombre);
  {
    static const char rtc_compat[] = "arm,pl031\0arm,primecell";
    prop("compatible", rtc_compat, sizeof rtc_compat);
  }
  prop_reg(RTC_BASE, RTC_SIZE);
  {
    uint32_t c[3] = {0, p->rtc_spi - 32, 4};
    prop_cells("interrupts", c, 3);
  }
  prop_u32("clocks", PH_CLK);
  prop_str("clock-names", "apb_pclk");
  end();

  snprintf(nombre, sizeof nombre, "uart@%llx", UART_BASE);
  node(nombre);
  prop_str("compatible", "ns16550a");
  prop_reg(UART_BASE, UART_SIZE);
  {
    uint32_t c[3] = {0, p->uart_spi - 32, 4};
    prop_cells("interrupts", c, 3);
  }
  prop_u32("clock-frequency", 1843200);
  end();

  for (int i = 0; i < p->nvirtio; i++) {
    uint64_t b = VIRTIO_BASE + i * VIRTIO_STRIDE;
    snprintf(nombre, sizeof nombre, "virtio_mmio@%llx", b);
    node(nombre);
    prop_str("compatible", "virtio,mmio");
    prop_reg(b, VIRTIO_STRIDE);
    uint32_t c[3] = {0, p->virtio_spi[i] - 32, 4};
    prop_cells("interrupts", c, 3);
    prop_empty("dma-coherent");
    end();
  }

  end();
  put32(9);

  // Ensamblado: cabecera (40) + reserva vacía (16) + estructura + cadenas.
  size_t off_rsv = 40, off_st = off_rsv + 16, off_str = off_st + nst;
  size_t total = off_str + nstrs;
  if (desbordado || total > cap) {
    memset_s(st, sizeof st, 0, nst);
    fallo("device tree too large (%zu bytes)", total);
    return 0;
  }
  uint8_t *b = buf;
  memset(b, 0, off_st);
  uint32_t h[10] = {0xd00dfeed, (uint32_t)total, (uint32_t)off_st, (uint32_t)off_str, (uint32_t)off_rsv, 17, 16, 0,
                    (uint32_t)nstrs, (uint32_t)nst};
  for (int i = 0; i < 10; i++) ((uint32_t *)b)[i] = __builtin_bswap32(h[i]);
  memcpy(b + off_st, st, nst);
  memcpy(b + off_str, strs, nstrs);
  // Las semillas no se quedan en el proceso: solo en el DTB del invitado.
  memset_s(st, sizeof st, 0, nst);
  return total;
}
