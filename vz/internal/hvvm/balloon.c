//go:build darwin && arm64

// virtio-balloon (virtio-mmio, id 5; colas inflate=0, deflate=1 y stats=2):
// el techo de memoria (-mem-max) y kling squeeze.
//
// El invitado entrega marcos de 4 KiB (VIRTIO_BALLOON_PFN_SHIFT) y la página
// del host es de 16 KiB, así que se lleva un mapa de bits de marcos inflados y
// una página del host se suelta solo cuando sus cuatro marcos están dentro.
// Soltar es volver a mapear ese tramo del fichero (ram_soltar en vm.c): la
// copia privada (sucia) se va y queda la página del fichero, limpia y fuera de
// phys_footprint; madvise no lo consigue con un mapeo MAP_PRIVATE. Lo que el
// invitado lea después de desinflar es lo del fichero (o ceros), y a un marco
// que estuvo en el globo no le importa lo que tenga.
//
// Corre en el hilo del vCPU: un mmap y un remapeo de HVF por tramo contiguo.
#include <mach/mach_time.h>
#include <string.h>
#include <time.h>

#include "khv.h"

#define MARCO 4096ULL
#define MARCOS_POR_PAGINA (PAGINA / MARCO)

static struct vballoon_state s;
static uint32_t intid;
static uint64_t feat;
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static uint8_t *mapa;  // un bit por marco de 4 KiB de la RAM
static uint64_t nmarcos;
static uint64_t paginas_sueltas;  // páginas del host soltadas ahora mismo
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;
// Últimas estadísticas del invitado (VIRTIO_BALLOON_S_*, en bytes) y cuántas
// veces han llegado: 0 = el driver no negoció la cola o aún no las dio.
static uint64_t stats[KHV_BALLOON_NSTATS];
static uint32_t stats_gen;
static uint64_t stats_t;  // mach_absolute_time() de las últimas
static mach_timebase_info_data_t tb;

static uint64_t ms_abs(uint64_t ms) {
  if (tb.denom == 0) mach_timebase_info(&tb);
  return ms * 1000000ULL * tb.denom / tb.numer;
}

static void irq(void) { vmmio_irq(&s.m, intid); }

static bool bit(uint64_t i) { return (mapa[i / 8] >> (i % 8)) & 1; }

static bool pagina_entera(uint64_t pag) {
  for (uint64_t k = 0; k < MARCOS_POR_PAGINA; k++)
    if (!bit(pag * MARCOS_POR_PAGINA + k)) return false;
  return true;
}

// Suelta (soltar) las páginas del host [desde, hasta), o apunta que vuelven
// al invitado: no hay nada que hacer, son las del fichero y la primera
// escritura las vuelve a copiar.
static void tramo(uint64_t desde, uint64_t hasta, bool soltar) {
  if (desde >= hasta || !g_ram) return;
  if (!soltar) {
    paginas_sueltas -= paginas_sueltas < hasta - desde ? paginas_sueltas : hasta - desde;
    return;
  }
  if (ram_soltar(desde * PAGINA, (hasta - desde) * PAGINA) == 0) paginas_sueltas += hasta - desde;
}

// Procesa los búferes de una cola: arrays de PFN (le32). inflar decide el
// sentido. Con mu.
static void cola(int qi, bool inflar) {
  struct vq *q = &s.q[qi];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!mapa || !vq_ptrs(q, &avail, &used, &desc)) return;
  bool hecho = false;
  static uint32_t pfns[QMAX * 4];  // el driver manda como mucho 256 por búfer
  int vueltas = 0;
  for (;;) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1] || vueltas++ >= (int)q->num) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    uint32_t n = vq_leer(q, desc, head, pfns, sizeof pfns) / 4;
    // Páginas del host que cambian de estado, agrupadas en tramos contiguos.
    uint64_t t0 = 0, t1 = 0;
    for (uint32_t k = 0; k < n; k++) {
      uint64_t gpa = (uint64_t)pfns[k] * MARCO;
      if (gpa < RAM_BASE || gpa - RAM_BASE >= g_ram_size) continue;  // fuera de la RAM: se ignora
      uint64_t m = (gpa - RAM_BASE) / MARCO, pag = m / MARCOS_POR_PAGINA;
      bool antes = pagina_entera(pag);
      if (inflar) mapa[m / 8] |= (uint8_t)(1u << (m % 8));
      else mapa[m / 8] &= (uint8_t)~(1u << (m % 8));
      if (antes == pagina_entera(pag)) continue;
      if (t1 == pag && t1 > t0) {
        t1++;
        continue;
      }
      tramo(t0, t1, inflar);
      t0 = pag;
      t1 = pag + 1;
    }
    tramo(t0, t1, inflar);
    vq_push(q, used, head, 0);
    hecho = true;
  }
  if (hecho) {
    s.m.isr |= 1;
    irq();
  }
}

// cola_stats recoge el búfer de estadísticas que deja el driver (pares
// {le16 etiqueta, le64 valor}, sin relleno) y se lo queda: devolvérselo es
// la forma de pedir otras (khv_balloon_stats). Con mu.
static void cola_stats(void) {
  struct vq *q = &s.q[2];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;
  int vueltas = 0;
  for (;;) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1] || vueltas++ >= (int)q->num) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    uint8_t buf[KHV_BALLOON_NSTATS * 10];
    uint32_t n = vq_leer(q, desc, head, buf, sizeof buf) / 10;
    for (uint32_t k = 0; k < n; k++) {
      uint16_t tag;
      uint64_t val;
      memcpy(&tag, buf + k * 10, 2);
      memcpy(&val, buf + k * 10 + 2, 8);
      if (tag < KHV_BALLOON_NSTATS) stats[tag] = val;
    }
    // Solo se retiene uno: si el driver dejó más, los anteriores vuelven.
    if (s.stats_pend) vq_push(q, used, s.stats_head, 0);
    s.stats_head = head;
    s.stats_pend = 1;
    stats_gen++;
    stats_t = mach_absolute_time();
  }
  pthread_cond_broadcast(&cv);
}

int khv_balloon_stats(uint64_t out[KHV_BALLOON_NSTATS], uint32_t *actual, int espera_ms) {
  pthread_mutex_lock(&mu);
  if (!mapa || g_pausa) {
    pthread_mutex_unlock(&mu);
    return -1;
  }
  // Sin estadísticas aún y sin búfer del driver: no negoció la cola. Con
  // búfer (restaurado de un snapshot: el driver lo dejó en otra vida de este
  // proceso) se le devuelve y se esperan las primeras.
  if (stats_gen == 0 && !s.stats_pend) {
    pthread_mutex_unlock(&mu);
    return -2;
  }
  // Se devuelven las últimas y se piden otras; se espera a que lleguen solo
  // si no hay ninguna (recién restaurado) o tienen más de un segundo (las
  // del arranque, en el primer squeeze). Como el sondeo de Firecracker: el
  // que pregunta cada 100 ms mientras el globo se infla ve las de la vuelta
  // anterior, y el driver, que las recoge en una cola de trabajo, no lo
  // retiene.
  uint32_t gen = stats_gen;
  bool esperar = stats_gen == 0 || mach_absolute_time() - stats_t > ms_abs(1000);
  if (s.stats_pend) {
    uint16_t *avail;
    uint8_t *used;
    struct vdesc *desc;
    if (vq_ptrs(&s.q[2], &avail, &used, &desc)) {
      vq_push(&s.q[2], used, s.stats_head, 0);
      s.stats_pend = 0;
      s.m.isr |= 1;
      irq();
    }
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_nsec += (long)espera_ms * 1000000L;
    ts.tv_sec += ts.tv_nsec / 1000000000L;
    ts.tv_nsec %= 1000000000L;
    while (esperar && stats_gen == gen && !g_pausa && pthread_cond_timedwait(&cv, &mu, &ts) == 0) {
    }
  }
  memcpy(out, stats, sizeof stats);
  *actual = s.actual;
  pthread_mutex_unlock(&mu);
  return 0;
}

int vballoon_init(uint32_t id, bool deflate_on_oom) {
  intid = id;
  memset(&s, 0, sizeof s);
  // VIRTIO_BALLOON_F_STATS_VQ (1): la cola de estadísticas, con la que
  // kling squeeze sabe cuánta memoria libre tiene el invitado sin preguntar
  // al agente. VIRTIO_BALLOON_F_DEFLATE_ON_OOM (2): el invitado puede
  // desinflar solo si se queda sin memoria, como el deflate_on_oom de
  // Firecracker.
  feat = (1u << 1) | (deflate_on_oom ? (1u << 2) : 0);
  memset(stats, 0, sizeof stats);
  stats_gen = 0;
  nmarcos = g_ram_size / MARCO;
  mapa = calloc((size_t)(nmarcos + 7) / 8, 1);
  if (!mapa) {
    fallo("no memory for the balloon bitmap");
    return -1;
  }
  return 0;
}

bool vballoon_mmio(uint64_t off, bool escribir, uint64_t *val, int size) {
  pthread_mutex_lock(&mu);
  if (off >= 0x100) {  // configuración: num_pages (le32, lo pide el host), actual (le32, lo dice el invitado)
    uint64_t o = off - 0x100;
    uint32_t cfg[2] = {s.num_pages, s.actual};
    if (!escribir) {
      uint64_t r = 0;
      if (o + (uint64_t)size <= sizeof cfg) memcpy(&r, (uint8_t *)cfg + o, (size_t)size);
      *val = r;
    } else if (o == 4 && size == 4) {
      s.actual = (uint32_t)*val;
    }
    pthread_mutex_unlock(&mu);
    return true;
  }
  uint32_t c = 0;
  switch (vmmio_acceso(&s.m, s.q, 3, 5, feat, off, escribir, val, &c)) {
    case VM_NOTIFY:
      if (c == 0) cola(0, true);
      else if (c == 1) cola(1, false);
      else if (c == 2) cola_stats();
      break;
    case VM_ISR: irq(); break;
    case VM_RESET: {
      // El driver se va: lo inflado vuelve al invitado.
      uint8_t nivel = s.m.nivel;
      uint32_t num = s.num_pages;
      for (uint64_t p = 0; mapa && p < g_ram_size / PAGINA; p++)
        if (pagina_entera(p)) tramo(p, p + 1, false);
      if (mapa) memset(mapa, 0, (size_t)(nmarcos + 7) / 8);
      memset(&s, 0, sizeof s);
      s.m.nivel = nivel;
      s.num_pages = num;
      stats_gen = 0;
      memset(stats, 0, sizeof stats);
      irq();
      break;
    }
  }
  pthread_mutex_unlock(&mu);
  return true;
}

// Fija lo que el invitado debe tener inflado (en marcos de 4 KiB) y le avisa
// con una interrupción de cambio de configuración.
void vballoon_objetivo(uint32_t marcos) {
  pthread_mutex_lock(&mu);
  if (marcos > nmarcos) marcos = (uint32_t)nmarcos;
  if (marcos != s.num_pages) {
    s.num_pages = marcos;
    s.m.isr |= 2;  // VIRTIO_MMIO_INT_CONFIG
    irq();
  }
  pthread_mutex_unlock(&mu);
}

uint32_t vballoon_actual(void) {
  pthread_mutex_lock(&mu);
  uint32_t a = s.actual;
  pthread_mutex_unlock(&mu);
  return a;
}

uint64_t vballoon_sueltas(void) {
  pthread_mutex_lock(&mu);
  uint64_t n = paginas_sueltas;
  pthread_mutex_unlock(&mu);
  return n;
}

// Si la página pag del host está entera en el globo (el snapshot no la
// escribe: su contenido no importa). Sin mu: el snapshot se hace en pausa.
bool vballoon_inflada(uint64_t pag) { return mapa && pag < g_ram_size / PAGINA && pagina_entera(pag); }

void vballoon_get(struct vballoon_state *o) {
  pthread_mutex_lock(&mu);
  *o = s;
  pthread_mutex_unlock(&mu);
}

void vballoon_reafirmar(void) {
  pthread_mutex_lock(&mu);
  hv_gic_set_spi(intid, s.m.nivel);
  pthread_mutex_unlock(&mu);
}

// Tras restaurar, el mapa de lo inflado empieza vacío: esas páginas son las
// del fichero (limpias, fuera de phys_footprint) o huecos. Un desinflado de
// algo que no está en el mapa no hace nada.
void vballoon_set(const struct vballoon_state *i) {
  pthread_mutex_lock(&mu);
  s = *i;
  pthread_mutex_unlock(&mu);
}
