//go:build darwin && arm64

// virtio-balloon (virtio-mmio, id 5; colas inflate=0 y deflate=1, sin la de
// estadísticas): el techo de memoria (-mem-max) y kling squeeze.
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
#include <string.h>

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

int vballoon_init(uint32_t id, bool deflate_on_oom) {
  intid = id;
  memset(&s, 0, sizeof s);
  // VIRTIO_BALLOON_F_DEFLATE_ON_OOM (2): el invitado puede desinflar solo si
  // se queda sin memoria, como el deflate_on_oom de Firecracker.
  feat = deflate_on_oom ? (1u << 2) : 0;
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
  switch (vmmio_acceso(&s.m, s.q, 2, 5, feat, off, escribir, val, &c)) {
    case VM_NOTIFY:
      if (c == 0) cola(0, true);
      else if (c == 1) cola(1, false);
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
