//go:build darwin && arm64

// virtio-balloon (virtio-mmio, id 5; colas inflate=0, deflate=1, stats=2 y
// el informe de páginas libres, 3): el techo de memoria (-mem-max), kling
// squeeze y la huella en reposo.
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
// Soltar cuesta un hv_vm_unmap, un mmap y un hv_vm_map por tramo, y el driver
// manda 256 marcos por búfer en el orden en que los saca del buddy: soltar
// búfer a búfer eran miles de tramos de una página. Las páginas enteras se
// apuntan como pendientes (pend) y se sueltan juntas, en tramos contiguos
// ordenados, al llegar al objetivo, al pasar de UMBRAL_PEND, antes de dar
// estadísticas (el núcleo mide la huella justo después) y al pausar. Una
// pendiente sigue en el globo: el invitado no la toca, así que esperar no
// cambia nada salvo cuándo baja la huella.
//
// El informe de páginas libres (VIRTIO_BALLOON_F_REPORTING, CONFIG_PAGE_
// REPORTING del invitado) hace lo mismo con la memoria que el invitado libera
// sin globo de por medio: el driver aparta bloques libres de 2 MiB (el orden
// de pageblock con páginas de 4 KiB), los manda en una cola y los recupera al
// devolvérselos; entre medias se sueltan.
#include <mach/mach_time.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "khv.h"

#define MARCO 4096ULL
#define MARCOS_POR_PAGINA (PAGINA / MARCO)
// Bits de características (virtio 1.x, 5.5.3).
#define F_STATS_VQ 1
#define F_DEFLATE_ON_OOM 2
#define F_REPORTING 5
// Páginas pendientes (16 KiB) que fuerzan a soltar sin esperar al objetivo:
// 32 MiB. Acota lo que tarda una tanda en el hilo del vCPU (y cuánto tarda la
// huella en bajar si el invitado no llega al objetivo).
#define UMBRAL_PEND 2048
// Cuota del informe de páginas libres: páginas del host que se sueltan por
// segundo, 1 GiB/s. El driver de Linux informa cada 2 s en bloques de 2 MiB,
// como mucho 64 MiB por búfer, así que no se acerca; un invitado que informa
// una y otra vez del mismo bloque remapeaba su RAM a voluntad bajo mu (que
// también toman las estadísticas y la pausa). Cada tramo cuenta al menos un
// bloque de 2 MiB, porque lo que cuesta es el tramo (un hv_vm_unmap, un mmap
// y un hv_vm_map). Lo que pasa de la cuota se devuelve sin soltar: es solo
// memoria que tarda más en bajar. Reiniciar el dispositivo no la repone.
#define INFORME_CUOTA (65536ULL)
#define INFORME_MIN_TRAMO ((2ULL << 20) / PAGINA)

static struct vballoon_state s;
static uint32_t intid;
static uint64_t feat;
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static uint8_t *mapa;  // un bit por marco de 4 KiB de la RAM
static uint64_t nmarcos, npaginas;
static uint64_t n_inflados;  // marcos con su bit en el mapa
// Por página del host (un byte): pend, entera en el globo y sin soltar
// todavía; suelta, ya devuelta al fichero (y aún en el globo).
static uint8_t *pend, *suelta;
static uint64_t n_pend, pend_min, pend_max;  // [pend_min, pend_max] acota dónde mirar
static uint64_t paginas_sueltas;             // páginas del host soltadas ahora mismo
// Contadores acumulados (khv_balloon_cuentas): páginas devueltas por el
// informe de libres, tramos soltados y lo que costaron.
static uint64_t c_informadas, c_tramos, c_ns, c_fallos;
static uint64_t informe_fichas, informe_t;  // la cuota (informe_cobrar)
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

static uint64_t abs_ns(uint64_t t) {
  if (tb.denom == 0) mach_timebase_info(&tb);
  return (uint64_t)((__uint128_t)t * tb.numer / tb.denom);
}

static void irq(void) { vmmio_irq(&s.m, intid); }

static bool bit(uint64_t i) { return (mapa[i / 8] >> (i % 8)) & 1; }

static bool pagina_entera(uint64_t pag) {
  for (uint64_t k = 0; k < MARCOS_POR_PAGINA; k++)
    if (!bit(pag * MARCOS_POR_PAGINA + k)) return false;
  return true;
}

// Las colas según lo negociado: con la de estadísticas el informe es la 3; sin
// ella, la 2 (virtio 5.5.2: las que no se negocian no ocupan índice).
static bool con_stats(void) { return (s.m.drv_features >> F_STATS_VQ) & 1; }
static bool con_informe(void) { return (s.m.drv_features >> F_REPORTING) & 1; }
static int cola_informe_idx(void) { return con_stats() ? 3 : 2; }

// soltar_tramo devuelve al fichero las páginas [desde, hasta) y lo cuenta.
static int soltar_tramo(uint64_t desde, uint64_t hasta) {
  uint64_t t = mach_absolute_time();
  int r = ram_soltar(desde * PAGINA, (hasta - desde) * PAGINA);
  c_ns += abs_ns(mach_absolute_time() - t);
  c_tramos++;
  if (r != 0) c_fallos++;
  return r;
}

// vaciar_locked suelta las páginas pendientes en tramos contiguos. Un tramo
// puede llevar dentro páginas ya sueltas (siguen en el globo y volver a
// soltarlas no cambia nada): así dos tandas que se intercalan salen en un
// tramo y no en muchos. Si ram_soltar falla, las del tramo se quedan en el
// globo sin soltar (sucias): no se reintenta en cada tanda. Con mu.
static bool log_globo;  // KLING_HV_BALLOON_LOG=1: una línea por tanda (para medir)

static void vaciar_locked(void) {
  if (!n_pend || !g_ram) return;
  uint64_t t0 = mach_absolute_time(), tramos0 = c_tramos, n0 = n_pend;
  uint64_t p = pend_min;
  while (p <= pend_max && p < npaginas) {
    if (!pend[p]) {
      p++;
      continue;
    }
    uint64_t a = p, b = p;  // [a, b): el tramo; se extiende por pend o suelta
    while (b < npaginas && (pend[b] || suelta[b])) b++;
    bool ok = soltar_tramo(a, b) == 0;
    for (uint64_t q = a; q < b; q++) {
      if (!pend[q]) continue;
      pend[q] = 0;
      n_pend--;
      if (ok) {
        suelta[q] = 1;
        paginas_sueltas++;
      }
    }
    p = b;
  }
  n_pend = 0;  // todo lo que quedaba está dentro de [pend_min, pend_max]
  pend_min = UINT64_MAX;
  pend_max = 0;
  if (log_globo)
    khv_log("balloon: released %llu pages in %llu runs in %.2f ms", (unsigned long long)n0,
            (unsigned long long)(c_tramos - tramos0), (double)abs_ns(mach_absolute_time() - t0) / 1e6);
}

// devolver: la página pag sale del globo (desinflado). No hay nada que
// soltar: es la del fichero y la primera escritura la vuelve a copiar.
static void devolver(uint64_t pag) {
  if (pend[pag]) pend[pag] = 0, n_pend--;
  if (suelta[pag]) suelta[pag] = 0, paginas_sueltas--;
  ram_reparar(pag * PAGINA, PAGINA);  // por si quedó fuera de HVF
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
    for (uint32_t k = 0; k < n; k++) {
      uint64_t gpa = (uint64_t)pfns[k] * MARCO;
      if (gpa < RAM_BASE || gpa - RAM_BASE >= g_ram_size) continue;  // fuera de la RAM: se ignora
      uint64_t m = (gpa - RAM_BASE) / MARCO, pag = m / MARCOS_POR_PAGINA;
      bool estaba = bit(m);
      if (estaba == inflar) continue;  // repetido: nada cambia
      bool antes = pagina_entera(pag);
      if (inflar) {
        mapa[m / 8] |= (uint8_t)(1u << (m % 8));
        n_inflados++;
      } else {
        mapa[m / 8] &= (uint8_t)~(1u << (m % 8));
        n_inflados--;
      }
      if (antes == pagina_entera(pag)) continue;
      if (!inflar) {
        devolver(pag);
      } else if (!pend[pag] && !suelta[pag]) {
        pend[pag] = 1;
        n_pend++;
        if (pag < pend_min) pend_min = pag;
        if (pag > pend_max) pend_max = pag;
      }
    }
    vq_push(q, used, head, 0);
    hecho = true;
  }
  // Inflando, se suelta al llegar al objetivo o al juntar UMBRAL_PEND; al
  // desinflar, lo que quede pendiente (el núcleo mide justo después).
  if (n_pend && (!inflar || n_pend >= UMBRAL_PEND || n_inflados >= s.num_pages)) vaciar_locked();
  if (hecho) {
    s.m.isr |= 1;
    irq();
  }
}

// informe_cobrar descuenta de la cuota del informe un tramo de pags páginas
// (al menos INFORME_MIN_TRAMO); false si no queda. Las fichas se reponen a
// INFORME_CUOTA por segundo, hasta un segundo de reserva. Con mu.
static bool informe_cobrar(uint64_t pags) {
  uint64_t ahora = mach_absolute_time();
  if (informe_t == 0) {
    informe_fichas = INFORME_CUOTA;
  } else {
    uint64_t ns = abs_ns(ahora - informe_t);
    informe_fichas = ns >= 1000000000ULL ? INFORME_CUOTA : informe_fichas + ns * INFORME_CUOTA / 1000000000ULL;
    if (informe_fichas > INFORME_CUOTA) informe_fichas = INFORME_CUOTA;
  }
  informe_t = ahora;
  if (pags < INFORME_MIN_TRAMO) pags = INFORME_MIN_TRAMO;
  if (informe_fichas < pags) return false;
  informe_fichas -= pags;
  return true;
}

// cola_informe atiende el informe de páginas libres: cada búfer es una
// cadena de descriptores (de escritura) con bloques libres del invitado, que
// no los toca hasta recibirlo de vuelta. Se sueltan las páginas del host que
// caen enteras dentro, en tramos contiguos, y se devuelve. Con mu.
static void cola_informe(int qi) {
  struct vq *q = &s.q[qi];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!g_ram || !vq_ptrs(q, &avail, &used, &desc)) return;
  static struct vdesc cad[QMAX];
  static uint64_t tramos[QMAX][2];
  bool hecho = false;
  int vueltas = 0;
  for (;;) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1] || vueltas++ >= (int)q->num) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    int nd = vq_cadena(q, desc, head, cad, QMAX), nt = 0;
    for (int k = 0; k < nd; k++) {
      uint64_t a = cad[k].addr, l = cad[k].len;
      if (a < RAM_BASE || a - RAM_BASE >= g_ram_size) continue;
      uint64_t ini = a - RAM_BASE, fin = ini + l;
      if (fin > g_ram_size || fin < ini) fin = g_ram_size;
      uint64_t p0 = (ini + PAGINA - 1) / PAGINA, p1 = fin / PAGINA;
      if (p0 >= p1) continue;
      // Ordenado por inserción: el driver los manda casi en orden y son 32.
      int j = nt++;
      while (j > 0 && tramos[j - 1][0] > p0) {
        tramos[j][0] = tramos[j - 1][0];
        tramos[j][1] = tramos[j - 1][1];
        j--;
      }
      tramos[j][0] = p0;
      tramos[j][1] = p1;
    }
    for (int k = 0; k < nt;) {
      uint64_t a = tramos[k][0], b = tramos[k][1];
      for (k++; k < nt && tramos[k][0] <= b; k++)
        if (tramos[k][1] > b) b = tramos[k][1];
      if (!informe_cobrar(b - a)) {
        if (log_globo) khv_log("balloon: free page report of %llu pages over quota", (unsigned long long)(b - a));
        continue;
      }
      if (soltar_tramo(a, b) == 0) c_informadas += b - a;
      if (log_globo) khv_log("balloon: free page report of %llu pages", (unsigned long long)(b - a));
    }
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
  // Quien pregunta va a medir la huella: lo pendiente se suelta ya.
  vaciar_locked();
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
    uint64_t fin = khv_ahora_ns() + (uint64_t)(espera_ms > 0 ? espera_ms : 0) * 1000000ULL;
    while (esperar && stats_gen == gen && !g_pausa && khv_esperar_hasta(&cv, &mu, fin) == 0) {
    }
  }
  memcpy(out, stats, sizeof stats);
  *actual = s.actual;
  pthread_mutex_unlock(&mu);
  return 0;
}

void khv_balloon_cuentas(uint64_t out[KHV_BALLOON_NCUENTAS]) {
  pthread_mutex_lock(&mu);
  out[0] = paginas_sueltas;
  out[1] = c_informadas;
  out[2] = c_tramos;
  out[3] = c_ns;
  out[4] = c_fallos;
  pthread_mutex_unlock(&mu);
}

int vballoon_init(uint32_t id, bool deflate_on_oom) {
  intid = id;
  memset(&s, 0, sizeof s);
  // VIRTIO_BALLOON_F_STATS_VQ: la cola de estadísticas, con la que kling
  // squeeze sabe cuánta memoria libre tiene el invitado sin preguntar al
  // agente. VIRTIO_BALLOON_F_DEFLATE_ON_OOM: el invitado puede desinflar solo
  // si se queda sin memoria, como el deflate_on_oom de Firecracker.
  // VIRTIO_BALLOON_F_REPORTING: el informe de páginas libres (como el
  // free_page_reporting de Firecracker); KLING_HV_REPORTING=0 lo quita, para
  // comparar.
  feat = (1u << F_STATS_VQ) | (deflate_on_oom ? (1u << F_DEFLATE_ON_OOM) : 0);
  const char *e = getenv("KLING_HV_REPORTING");
  if (!(e && strcmp(e, "0") == 0)) feat |= 1u << F_REPORTING;
  e = getenv("KLING_HV_BALLOON_LOG");
  log_globo = e && strcmp(e, "1") == 0;
  memset(stats, 0, sizeof stats);
  stats_gen = 0;
  nmarcos = g_ram_size / MARCO;
  npaginas = g_ram_size / PAGINA;
  n_inflados = n_pend = paginas_sueltas = 0;
  informe_fichas = informe_t = 0;
  pend_min = UINT64_MAX;
  pend_max = 0;
  free(mapa);
  free(pend);
  free(suelta);
  mapa = calloc((size_t)(nmarcos + 7) / 8, 1);
  pend = calloc((size_t)npaginas + 1, 1);
  suelta = calloc((size_t)npaginas + 1, 1);
  if (!mapa || !pend || !suelta) {
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
  switch (vmmio_acceso(&s.m, s.q, 4, 5, feat, off, escribir, val, &c)) {
    case VM_NOTIFY:
      if (c == 0) cola(0, true);
      else if (c == 1) cola(1, false);
      else if (c == 2 && con_stats()) cola_stats();
      else if (c == (uint32_t)cola_informe_idx() && con_informe()) cola_informe((int)c);
      break;
    case VM_ISR: irq(); break;
    case VM_RESET: {
      // El driver se va: lo inflado vuelve al invitado.
      uint8_t nivel = s.m.nivel;
      uint32_t num = s.num_pages;
      if (mapa) {
        memset(mapa, 0, (size_t)(nmarcos + 7) / 8);
        memset(pend, 0, (size_t)npaginas);
        memset(suelta, 0, (size_t)npaginas);
      }
      n_inflados = n_pend = paginas_sueltas = 0;
      pend_min = UINT64_MAX;
      pend_max = 0;
      ram_reparar(0, g_ram_size);
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
  vaciar_locked();
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

void vballoon_vaciar(void) {
  pthread_mutex_lock(&mu);
  vaciar_locked();
  pthread_mutex_unlock(&mu);
}

uint64_t vballoon_sueltas(void) {
  pthread_mutex_lock(&mu);
  uint64_t n = paginas_sueltas;
  pthread_mutex_unlock(&mu);
  return n;
}

// Si la página pag del host está entera en el globo (el snapshot no la
// escribe: su contenido no importa). Sin mu: el snapshot se hace en pausa.
bool vballoon_inflada(uint64_t pag) { return mapa && pag < npaginas && pagina_entera(pag); }

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
