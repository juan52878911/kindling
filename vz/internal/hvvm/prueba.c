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
#include <sys/socket.h>
#include <unistd.h>

#include "khv.h"

#define PRUEBA_RAM (1ULL << 20)
// La RAM grande deja cadenas de 256 descriptores solapados de casi 16 MiB:
// más de INT_MAX bytes en una sola cadena, que recvmsg de Darwin rechaza con
// EINVAL (ver rx_una en net.c).
#define PRUEBA_RAM_GRANDE (16ULL << 20)

static uint16_t rd16(const uint8_t *p) { return (uint16_t)(p[0] | p[1] << 8); }
static uint32_t rd32(const uint8_t *p) { return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24; }

typedef bool (*mmio_fn)(uint64_t off, bool escribir, uint64_t *val, int size);

static bool blk_mmio(uint64_t off, bool escribir, uint64_t *val, int size) { return vblk_mmio(0, off, escribir, val, size); }
static bool vfs_mmio(uint64_t off, bool escribir, uint64_t *val, int size) { return vvfs_mmio(0, off, escribir, val, size); }

static void w(mmio_fn f, uint64_t off, uint64_t v) { f(off, true, &v, 4); }

// La RAM de mentira lleva detrás una página sin permisos: lo que se salga de
// ella por poco (un anillo mal medido) rompe el proceso aquí mismo, sin
// esperar a que el desbordamiento pise algo que se note.
// En cada llamada se vuelve a poner: otra prueba del paquete (un khv_create
// que rechaza la configuración) puede haber dejado g_ram_size con el tamaño
// de su VM, y el memset de la RAM pisaría la guarda.
static int preparar(void) {
  static bool listo;
  static uint8_t *propia;
  if (listo) {
    g_ram = propia;
    g_ram_size = PRUEBA_RAM;
  }
  if (!listo) {
    size_t pag = (size_t)getpagesize();
    uint8_t *ram = mmap(NULL, PRUEBA_RAM + pag, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
    if (ram == MAP_FAILED || mprotect(ram + PRUEBA_RAM, pag, PROT_NONE) != 0) return -1;
    g_ram_size = PRUEBA_RAM;
    g_ram = propia = ram;
    char ruta[] = "/tmp/kling-hv-prueba.XXXXXX";
    int fd = mkstemp(ruta);
    if (fd < 0) return -1;
    if (ftruncate(fd, 1 << 20) != 0) return -1;
    close(fd);
    int r = vblk_init(0, ruta, false, KHV_SINC_FULL, 48);
    unlink(ruta);
    if (r != 0) return -1;
    static const uint8_t mac[6] = {6, 0, 0xac, 0x10, 0, 2};
    vnet_init(-1, mac, 16384, 49);
    vrng_init(50);
    vsock_init(51);
    if (vballoon_init(52, true) != 0) return -1;
    if (vvfs_init(0, "kling-share-0", 53) != 0) return -1;
    listo = true;
  }
  return 0;
}

// ram_grande cambia la RAM de mentira por una de PRUEBA_RAM_GRANDE, también
// con su página de guarda detrás. preparar vuelve a la pequeña.
static int ram_grande(void) {
  static uint8_t *grande;
  if (!grande) {
    size_t pag = (size_t)getpagesize();
    uint8_t *ram = mmap(NULL, PRUEBA_RAM_GRANDE + pag, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
    if (ram == MAP_FAILED || mprotect(ram + PRUEBA_RAM_GRANDE, pag, PROT_NONE) != 0) return -1;
    grande = ram;
  }
  g_ram = grande;
  g_ram_size = PRUEBA_RAM_GRANDE;
  return 0;
}

// red_prueba: el otro extremo de la red de mentira (un par de datagramas,
// como el socket que le da Go) por el que llega una trama a cada prueba de
// la red; red[0] hace de socket del VMM.
static int red[2] = {-1, -1};

// Devuelve 0, o el errno con el que habría muerto el hilo de recepción de la
// red (y el invitado se habría quedado sin red para siempre).
int khv_prueba(const uint8_t *d, size_t n) {
  if (preparar() != 0) return -1;
  // Las primeras 64 B programan el dispositivo; el resto es la RAM.
  if (n < 64) return 0;
  // El bit 0 del byte 1: la RAM grande.
  if ((d[1] & 1) && ram_grande() != 0) return -1;
  memset(g_ram, 0, g_ram_size);
  size_t l = n - 64 < g_ram_size ? n - 64 : g_ram_size;
  memcpy(g_ram, d + 64, l);

  mmio_fn devs[] = {blk_mmio, vnet_mmio, vrng_mmio, vsock_mmio, vballoon_mmio, vfs_mmio};
  int ncolas[] = {1, 2, 1, 3, 3, 2};
  int k = d[0] % 6;
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
  int rx = 0;
  if (k == 1) {
    // Una trama llega por la red y se mete en la cola 0 como lo haría el
    // hilo de recepción; lo que no entre se tira para la siguiente prueba.
    if (red[0] < 0 && socketpair(AF_UNIX, SOCK_DGRAM, 0, red) != 0) return -1;
    uint8_t trama[60] = {0xff, 0xff, 0xff, 0xff, 0xff, 0xff};
    (void)!send(red[1], trama, sizeof trama, MSG_DONTWAIT);
    rx = vnet_prueba_rx(red[0]);
    while (recv(red[0], trama, sizeof trama, MSG_DONTWAIT) >= 0) {
    }
  }
  if (k == 0) vblk_prueba(0);
  if (k == 4) vballoon_objetivo(rd32(d + 60));
  // virtio-fs: lo que Go contestaría, con tamaños de los datos (a veces más
  // de lo que cabe en la cadena) y respuestas repetidas.
  if (k == 5) vvfs_prueba(0, d + 40, 6);
  w(f, 0x064, 0xff);  // InterruptACK
  w(f, 0x070, 0);
  return rx;
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
// armar_blk deja el disco 0 con una cola de 4 entradas lista: la tabla en el
// principio de la RAM, el avail en +0x1000 y el used al final.
static const uint64_t desc = RAM_BASE, avail = RAM_BASE + 0x1000, used = RAM_BASE + PRUEBA_RAM - 40;
static void armar_blk(void) {
  memset(g_ram, 0, g_ram_size);
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
}

int khv_prueba_carrera(int iters) {
  if (preparar() != 0) return -1;
  armar_blk();
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

// khv_prueba_flush manda un FLUSH al disco 0 (un fichero normal, no
// desechable) y devuelve el byte de estado: 0 si el F_FULLFSYNC fue bien.
int khv_prueba_flush(void) {
  if (preparar() != 0) return -1;
  armar_blk();
  struct vdesc *d = gpa_ptr(desc, 2 * sizeof *d);
  d[0] = (struct vdesc){RAM_BASE + 0x2000, 16, VIRTQ_DESC_F_NEXT, 1};
  d[1] = (struct vdesc){RAM_BASE + 0x4000, 1, VIRTQ_DESC_F_WRITE, 0};
  *(uint32_t *)gpa_ptr(RAM_BASE + 0x2000, 4) = 4;  // VIRTIO_BLK_T_FLUSH
  *(uint8_t *)gpa_ptr(RAM_BASE + 0x4000, 1) = 0xff;
  uint16_t *av = gpa_ptr(avail, 6 + 2 * 4);
  av[2] = 0;
  __atomic_store_n(&av[1], 1, __ATOMIC_RELEASE);
  vblk_prueba(0);
  int estado = *(volatile uint8_t *)gpa_ptr(RAM_BASE + 0x4000, 1);
  w(blk_mmio, 0x070, 0);
  return estado;
}

// khv_prueba_cfg lee 4 bytes de los registros (off < 0x100) o de la
// configuración (0x100 en adelante) del disco 0.
uint32_t khv_prueba_cfg(uint32_t off) {
  if (preparar() != 0) return 0;
  uint64_t v = 0;
  vblk_mmio(0, off, false, &v, 4);
  return (uint32_t)v;
}

// khv_prueba_es manda al disco 0 una petición tipo (0 IN, 1 OUT, 11 DISCARD,
// 13 WRITE_ZEROES...) en sector con nd búferes de datos de lens[i] bytes,
// cada uno en su trozo de 64 KiB de la RAM (no seguidos: preadv/pwritev
// tienen que repartirlos bien). datos trae lo que se escribe (OUT y los
// segmentos de DISCARD y WRITE_ZEROES) y vuelve con lo leído (IN). El búfer malo (o
// -1) apunta fuera de la RAM. Deja en *usado la longitud del used y devuelve
// el byte de estado, o -1 si no se pudo preparar.
int khv_prueba_es(uint32_t tipo, uint64_t sector, const uint32_t *lens, int nd, uint8_t *datos, int malo,
                  uint32_t *usado) {
  if (preparar() != 0 || nd < 0 || nd > 8) return -1;
  const uint64_t d0 = RAM_BASE, av = RAM_BASE + 0x1000, us = RAM_BASE + 0x2000;
  const uint64_t cab = RAM_BASE + 0x3000, est = RAM_BASE + 0x4000, buf = RAM_BASE + 0x10000;
  memset(g_ram, 0, g_ram_size);
  w(blk_mmio, 0x070, 0);
  w(blk_mmio, 0x030, 0);
  w(blk_mmio, 0x038, 16);
  w(blk_mmio, 0x080, (uint32_t)d0);
  w(blk_mmio, 0x090, (uint32_t)av);
  w(blk_mmio, 0x0a0, (uint32_t)us);
  w(blk_mmio, 0x084, 0);
  w(blk_mmio, 0x094, 0);
  w(blk_mmio, 0x0a4, 0);
  w(blk_mmio, 0x044, 1);
  w(blk_mmio, 0x070, 0xf);
  struct vdesc *d = gpa_ptr(d0, 16 * sizeof *d);
  struct {
    uint32_t type, reserved;
    uint64_t sector;
  } h = {tipo, 0, sector};
  memcpy(gpa_ptr(cab, sizeof h), &h, sizeof h);
  d[0] = (struct vdesc){cab, sizeof h, VIRTQ_DESC_F_NEXT, 1};
  size_t o = 0;
  for (int i = 0; i < nd; i++) {
    if (lens[i] > 0x10000) return -1;
    uint64_t a = buf + (uint64_t)i * 0x10000;
    if (tipo != 0) memcpy(gpa_ptr(a, lens[i]), datos + o, lens[i]);
    if (i == malo) a = RAM_BASE + PRUEBA_RAM;
    uint16_t f = VIRTQ_DESC_F_NEXT | (tipo == 0 ? VIRTQ_DESC_F_WRITE : 0);
    d[1 + i] = (struct vdesc){a, lens[i], f, (uint16_t)(2 + i)};
    o += lens[i];
  }
  d[1 + nd] = (struct vdesc){est, 1, VIRTQ_DESC_F_WRITE, 0};
  *(uint8_t *)gpa_ptr(est, 1) = 0xff;
  uint16_t *a = gpa_ptr(av, 6 + 2 * 16);
  a[2] = 0;
  __atomic_store_n(&a[1], 1, __ATOMIC_RELEASE);
  vblk_prueba(0);
  o = 0;
  for (int i = 0; i < nd; i++) {
    if (tipo == 0 && i != malo) memcpy(datos + o, gpa_ptr(buf + (uint64_t)i * 0x10000, lens[i]), lens[i]);
    o += lens[i];
  }
  *usado = *(volatile uint32_t *)gpa_ptr(us + 8, 4);  // used.ring[0].len
  int e = *(volatile uint8_t *)gpa_ptr(est, 1);
  w(blk_mmio, 0x070, 0);
  return e;
}

// khv_prueba_tanda deja en la cola del disco 0 (256 entradas) 512 WRITE del
// disco entero (1016 KiB: 254 búferes de 4 KiB que aliasan la misma página) y
// atiende UNA tanda, con g_pausa = pausa y el tope de bytes por tanda en tope
// (0: el de siempre). Devuelve cuántas peticiones respondió (-1 si no se pudo
// preparar) y en *estado el byte de estado de la última. Sin tope, un
// invitado así tenía al hilo del disco escribiendo medio GiB por aviso, y
// khv_pause (que espera a que acabe la tanda, vblk_quieto) no llegaba.
int khv_prueba_tanda(bool pausa, uint64_t tope, int *estado) {
  if (preparar() != 0) return -1;
  const uint64_t d0 = RAM_BASE, av = RAM_BASE + 0x1000, us = RAM_BASE + 0x2000;
  const uint64_t cab = RAM_BASE + 0x3000, est = RAM_BASE + 0x4000, buf = RAM_BASE + 0x10000;
  memset(g_ram, 0, g_ram_size);
  w(blk_mmio, 0x070, 0);
  w(blk_mmio, 0x030, 0);
  w(blk_mmio, 0x038, QMAX);
  w(blk_mmio, 0x080, (uint32_t)d0);
  w(blk_mmio, 0x090, (uint32_t)av);
  w(blk_mmio, 0x0a0, (uint32_t)us);
  w(blk_mmio, 0x084, 0);
  w(blk_mmio, 0x094, 0);
  w(blk_mmio, 0x0a4, 0);
  w(blk_mmio, 0x044, 1);
  w(blk_mmio, 0x070, 0xf);
  struct vdesc *d = gpa_ptr(d0, QMAX * sizeof *d);
  struct {
    uint32_t type, reserved;
    uint64_t sector;
  } h = {1, 0, 0};
  memcpy(gpa_ptr(cab, sizeof h), &h, sizeof h);
  d[0] = (struct vdesc){cab, sizeof h, VIRTQ_DESC_F_NEXT, 1};
  for (int i = 1; i < QMAX - 1; i++) d[i] = (struct vdesc){buf, 4096, VIRTQ_DESC_F_NEXT, (uint16_t)(i + 1)};
  d[QMAX - 1] = (struct vdesc){est, 1, VIRTQ_DESC_F_WRITE, 0};
  *(uint8_t *)gpa_ptr(est, 1) = 0xff;
  uint16_t *a = gpa_ptr(av, 6 + 2 * QMAX);
  for (int i = 0; i < QMAX; i++) a[2 + i] = 0;  // todas la misma cadena
  __atomic_store_n(&a[1], (uint16_t)(2 * QMAX), __ATOMIC_RELEASE);
  bool pausa_antes = g_pausa;
  uint64_t tope_antes = vblk_tanda_max;
  g_pausa = pausa;
  if (tope) vblk_tanda_max = tope;
  vblk_prueba(0);
  g_pausa = pausa_antes;
  vblk_tanda_max = tope_antes;
  int hechas = *(volatile uint16_t *)gpa_ptr(us + 2, 2);
  *estado = *(volatile uint8_t *)gpa_ptr(est, 1);
  w(blk_mmio, 0x070, 0);
  return hechas;
}

// ---- used.idx es del dispositivo, no del invitado ----

static bool rng_mmio(uint64_t off, bool escribir, uint64_t *val, int size) { return vrng_mmio(off, escribir, val, size); }

// khv_prueba_used: un invitado que escribe used.idx (que es del dispositivo)
// entre dos peticiones al virtio-rng. La segunda tiene que ir a la entrada 1
// del anillo used y dejar used.idx en 2, no seguir desde lo que puso el
// invitado. 0 si va bien; si no, el paso que falló (negativo).
int khv_prueba_used(void) {
  if (preparar() != 0) return -100;
  memset(g_ram, 0, g_ram_size);
  const uint64_t d0 = RAM_BASE, av0 = RAM_BASE + 0x1000, us0 = RAM_BASE + 0x2000;
  w(rng_mmio, 0x070, 0);
  w(rng_mmio, 0x030, 0);
  w(rng_mmio, 0x038, 4);
  w(rng_mmio, 0x080, (uint32_t)d0);
  w(rng_mmio, 0x090, (uint32_t)av0);
  w(rng_mmio, 0x0a0, (uint32_t)us0);
  w(rng_mmio, 0x084, 0);
  w(rng_mmio, 0x094, 0);
  w(rng_mmio, 0x0a4, 0);
  w(rng_mmio, 0x044, 1);
  w(rng_mmio, 0x070, 0xf);
  struct vdesc *d = gpa_ptr(d0, 4 * sizeof *d);
  for (int i = 0; i < 4; i++) d[i] = (struct vdesc){RAM_BASE + 0x3000 + 0x100 * (uint64_t)i, 16, VIRTQ_DESC_F_WRITE, 0};
  uint16_t *av = gpa_ptr(av0, 6 + 2 * 4);
  volatile uint16_t *uidx = gpa_ptr(us0 + 2, 2);
  uint32_t *anillo = gpa_ptr(us0 + 4, 8 * 4);
  int r = 0;
  // Una petición: entra en la entrada 0 y used.idx pasa a 1.
  av[2] = 0;
  av[1] = 1;
  w(rng_mmio, 0x050, 0);
  if (*uidx != 1 || anillo[0] != 0 || anillo[1] != 16) r = -1;
  // El invitado pisa used.idx; la siguiente sigue donde iba el dispositivo.
  if (!r) {
    *uidx = 0x1235;
    av[3] = 1;
    av[1] = 2;
    w(rng_mmio, 0x050, 0);
    if (*uidx != 2 || anillo[2] != 1 || anillo[3] != 16) r = -2;
  }
  // Snapshot y restauración: el estado lleva el contador, y lo que haya en
  // la RAM al restaurar no cuenta.
  struct vrng_state st;
  if (!r) {
    vrng_get(&st);
    if (st.q.used_idx != 2) r = -3;
    *uidx = 0x777;
    vrng_set(&st);
    av[4] = 2;
    av[1] = 3;
    w(rng_mmio, 0x050, 0);
    if (!r && (*uidx != 3 || anillo[4] != 2)) r = -3;
  }
  // Un snapshot de antes (sin SNAP_F_USED_IDX): used_idx a cero en el
  // fichero y el bueno en el anillo used de su RAM.
  if (!r) {
    vrng_get(&st);
    st.q.used_idx = 0;
    vq_used_de_ram(&st.q);
    if (st.q.used_idx != 3) r = -4;
    vrng_set(&st);
    av[5] = 3;
    av[1] = 4;
    w(rng_mmio, 0x050, 0);
    if (!r && (*uidx != 4 || anillo[6] != 3)) r = -4;
  }
  // Una cola que se vuelve a armar empieza en 0 (el driver deja used a cero).
  if (!r) {
    w(rng_mmio, 0x044, 0);
    w(rng_mmio, 0x044, 1);
    vrng_get(&st);
    if (st.q.used_idx != 0) r = -5;
  }
  w(rng_mmio, 0x070, 0);
  return r;
}

// khv_prueba_poll_red: el hilo de recepción de verdad, sobre un par de
// datagramas, con el primer poll fallando con EAGAIN (khv_poll). Después una
// trama tiene que llegar a la cola 0. 0 si llega.
int khv_prueba_poll_red(void) {
  if (preparar() != 0) return -100;
  memset(g_ram, 0, g_ram_size);
  int sv[2];
  if (socketpair(AF_UNIX, SOCK_DGRAM, 0, sv) != 0) return -100;
  const uint64_t d0 = RAM_BASE, av0 = RAM_BASE + 0x1000, us0 = RAM_BASE + 0x2000;
  w(vnet_mmio, 0x070, 0);
  w(vnet_mmio, 0x030, 0);
  w(vnet_mmio, 0x038, 4);
  w(vnet_mmio, 0x080, (uint32_t)d0);
  w(vnet_mmio, 0x090, (uint32_t)av0);
  w(vnet_mmio, 0x0a0, (uint32_t)us0);
  w(vnet_mmio, 0x084, 0);
  w(vnet_mmio, 0x094, 0);
  w(vnet_mmio, 0x0a4, 0);
  w(vnet_mmio, 0x044, 1);
  w(vnet_mmio, 0x070, 0xf);
  struct vdesc *d = gpa_ptr(d0, 4 * sizeof *d);
  for (int i = 0; i < 4; i++) d[i] = (struct vdesc){RAM_BASE + 0x4000 + 0x800 * (uint64_t)i, 0x800, VIRTQ_DESC_F_WRITE, 0};
  uint16_t *av = gpa_ptr(av0, 6 + 2 * 4);
  for (int i = 0; i < 4; i++) av[2 + i] = (uint16_t)i;
  av[1] = 4;
  volatile uint16_t *uidx = gpa_ptr(us0 + 2, 2);
  static const uint8_t mac[6] = {6, 0, 0xac, 0x10, 0, 2};
  // Sin VM todo empieza en pausa, y el hilo esperaría a que corriera.
  bool pausa_antes = g_pausa;
  g_pausa = false;
  __atomic_store_n(&khv_poll_fallos[KHV_POLL_RED], 1, __ATOMIC_RELEASE);
  vnet_init(sv[0], mac, 16384, 49);
  for (int i = 0; i < 1000 && __atomic_load_n(&khv_poll_fallos[KHV_POLL_RED], __ATOMIC_ACQUIRE); i++) usleep(1000);
  uint8_t trama[60] = {0xff, 0xff, 0xff, 0xff, 0xff, 0xff};
  int r = send(sv[1], trama, sizeof trama, 0) == (ssize_t)sizeof trama ? -1 : -100;
  for (int i = 0; r == -1 && i < 2000; i++) {
    if (*uidx == 1) r = 0;
    else usleep(1000);
  }
  __atomic_store_n(&khv_poll_fallos[KHV_POLL_RED], 0, __ATOMIC_RELEASE);
  vnet_parar();
  g_pausa = pausa_antes;
  w(vnet_mmio, 0x070, 0);
  vnet_init(-1, mac, 16384, 49);
  close(sv[0]);
  close(sv[1]);
  return r;
}

// khv_prueba_poll_vsock: khv_vsock_prueba_poll con los dispositivos de
// prueba ya puestos (vsock_init arranca vs_hilo).
int khv_prueba_poll_vsock(void) {
  if (preparar() != 0) return -100;
  return khv_vsock_prueba_poll();
}
