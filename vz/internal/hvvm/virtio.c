//go:build darwin && arm64

// virtio-mmio (versión 2, colas split): los registros comunes, el disco
// (virtio-blk, con un hilo por disco) y el generador de entropía (virtio-rng).
// La red está en net.c.
#include <errno.h>
#include <fcntl.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include "khv.h"

// ===================== común =====================

bool vq_ptrs(struct vq *q, uint16_t **avail, uint8_t **used, struct vdesc **desc) {
  if (!q->ready || q->num == 0 || q->num > QMAX) return false;
  *avail = gpa_ptr(q->avail, 6 + 2 * (uint64_t)q->num);
  *used = gpa_ptr(q->used, 6 + 8 * (uint64_t)q->num);
  *desc = gpa_ptr(q->desc, 16 * (uint64_t)q->num);
  return *avail && *used && *desc;
}

int vq_cadena(const struct vq *q, const struct vdesc *desc, uint16_t head, struct vdesc *out, int max) {
  int n = 0;
  uint16_t i = head;
  for (;;) {
    // Fuera de la tabla, más larga que la cola (un ciclo) o más larga de lo
    // que cabe: inválida. Los indirectos no se ofrecen (sin
    // VIRTIO_F_INDIRECT_DESC), así que uno es un invitado que miente.
    if (i >= q->num || n >= max || n >= (int)q->num) return -1;
    struct vdesc d;
    memcpy(&d, (const void *)&desc[i], sizeof d);  // una sola lectura: el invitado puede cambiarlo
    if (d.flags & VIRTQ_DESC_F_INDIRECT) return -1;
    out[n++] = d;
    if (!(d.flags & VIRTQ_DESC_F_NEXT)) return n;
    i = d.next;
  }
}

uint32_t vq_leer(const struct vq *q, const struct vdesc *desc, uint16_t head, void *buf, uint32_t cap) {
  struct vdesc ch[QMAX];
  int n = vq_cadena(q, desc, head, ch, QMAX);
  uint32_t total = 0;
  for (int k = 0; k < n; k++) {
    if (ch[k].flags & VIRTQ_DESC_F_WRITE) continue;
    uint32_t l = ch[k].len;
    if (l > cap - total) return 0;  // no cabe: el paquete se descarta entero
    const void *d = gpa_ptr(ch[k].addr, l);
    if (!d) return 0;
    memcpy((uint8_t *)buf + total, d, l);
    total += l;
  }
  return n < 0 ? 0 : total;
}

uint32_t vq_capacidad(const struct vq *q, const struct vdesc *desc, uint16_t head) {
  struct vdesc ch[QMAX];
  int n = vq_cadena(q, desc, head, ch, QMAX);
  uint64_t total = 0;
  for (int k = 0; k < n; k++) {
    if (!(ch[k].flags & VIRTQ_DESC_F_WRITE)) continue;
    if (!gpa_ptr(ch[k].addr, ch[k].len)) return 0;
    total += ch[k].len;
  }
  return n < 0 ? 0 : total > UINT32_MAX ? UINT32_MAX : (uint32_t)total;
}

uint32_t vq_escribir(const struct vq *q, const struct vdesc *desc, uint16_t head, const void *src, uint32_t len) {
  struct vdesc ch[QMAX];
  int n = vq_cadena(q, desc, head, ch, QMAX);
  uint32_t off = 0;
  for (int k = 0; k < n && off < len; k++) {
    if (!(ch[k].flags & VIRTQ_DESC_F_WRITE)) continue;
    uint32_t l = ch[k].len < len - off ? ch[k].len : len - off;
    void *d = gpa_ptr(ch[k].addr, l);
    if (!d) break;
    memcpy(d, (const uint8_t *)src + off, l);
    off += l;
  }
  return off;
}

void vq_push(struct vq *q, uint8_t *used, uint16_t head, uint32_t len) {
  uint16_t uidx = *(volatile uint16_t *)(used + 2);
  uint32_t *e = (uint32_t *)(used + 4 + 8 * (uidx % q->num));
  e[0] = head;
  e[1] = len;
  __atomic_thread_fence(__ATOMIC_RELEASE);
  *(volatile uint16_t *)(used + 2) = uidx + 1;
}

void vmmio_irq(struct vmmio *m, uint32_t intid) {
  uint8_t n = m->isr != 0;
  if (n != m->nivel) {
    m->nivel = n;
    hv_gic_set_spi(intid, n);
    // Un vCPU dormido en el WFI de respaldo (vm.c) no se entera solo.
    if (n) vcpu_kick_all();
  }
}

uint64_t khv_avisos[KHV_N_MAX];
uint64_t khv_event_idx = 1ULL << VIRTIO_RING_F_EVENT_IDX;

void khv_contadores(uint64_t out[KHV_N_MAX]) {
  for (int i = 0; i < KHV_N_MAX; i++) out[i] = __atomic_load_n(&khv_avisos[i], __ATOMIC_RELAXED);
}

static bool con_event_idx(const struct vmmio *m) { return (m->drv_features >> VIRTIO_RING_F_EVENT_IDX) & 1; }

bool vq_interrumpir(const struct vmmio *m, const struct vq *q, uint16_t *avail, uint16_t viejo, uint16_t nuevo) {
  if (viejo == nuevo) return false;
  // El used idx escrito antes de leer used_event (o los flags): si no, el
  // driver podría volver a pedir interrupción entre medias y no recibirla.
  __atomic_thread_fence(__ATOMIC_SEQ_CST);
  if (con_event_idx(m)) {
    uint16_t ev = *(volatile uint16_t *)&avail[2 + q->num];  // used_event
    return (uint16_t)(nuevo - ev - 1) < (uint16_t)(nuevo - viejo);  // vring_need_event
  }
  return !(*(volatile uint16_t *)&avail[0] & 1);  // VRING_AVAIL_F_NO_INTERRUPT
}

bool vq_pedir_aviso(const struct vmmio *m, const struct vq *q, uint16_t *avail, uint8_t *used) {
  if (!con_event_idx(m)) return false;
  *(volatile uint16_t *)(used + 4 + 8 * (uint64_t)q->num) = q->last_avail;  // avail_event
  __atomic_thread_fence(__ATOMIC_SEQ_CST);
  return *(volatile uint16_t *)&avail[1] != q->last_avail;
}

void vq_pedir_aviso_ya(const struct vmmio *m, const struct vq *q, uint16_t *avail, uint8_t *used) {
  if (!con_event_idx(m)) return;
  *(volatile uint16_t *)(used + 4 + 8 * (uint64_t)q->num) = *(volatile uint16_t *)&avail[1];
  __atomic_thread_fence(__ATOMIC_SEQ_CST);
}

static void dir_lo(uint64_t *d, uint32_t v) { *d = (*d & ~0xffffffffULL) | v; }
static void dir_hi(uint64_t *d, uint32_t v) { *d = (*d & 0xffffffffULL) | ((uint64_t)v << 32); }

int vmmio_acceso(struct vmmio *m, struct vq *qs, int nq, uint32_t devid, uint64_t feat_lo, uint64_t off,
                 bool escribir, uint64_t *val, uint32_t *cola) {
  struct vq *q = m->queue_sel < (uint32_t)nq ? &qs[m->queue_sel] : NULL;
  uint32_t v = (uint32_t)*val;
  // Con la cola lista, su tamaño y sus direcciones no se tocan (virtio 1.x:
  // el driver solo los escribe con QueueReady a 0). Un invitado que los
  // cambiara con un hilo de dispositivo a mitad de tanda le movería el anillo
  // por debajo; se ignoran hasta que ponga QueueReady a 0.
  bool fija = q && q->ready && (off == 0x038 || (off >= 0x080 && off <= 0x0a4));
  if (escribir && fija) return VM_NADA;
  if (escribir) {
    switch (off) {
      case 0x014: m->dev_feat_sel = v; break;
      case 0x020:
        if (m->drv_feat_sel < 2) {
          m->drv_features &= ~(0xffffffffULL << (32 * m->drv_feat_sel));
          m->drv_features |= (uint64_t)v << (32 * m->drv_feat_sel);
        }
        break;
      case 0x024: m->drv_feat_sel = v; break;
      case 0x030: m->queue_sel = v; break;
      case 0x038: if (q && v <= QMAX) q->num = v; break;
      case 0x044: if (q) q->ready = v & 1; break;
      case 0x050:
        *cola = v;
        return VM_NOTIFY;
      case 0x064:
        m->isr &= ~v;
        return VM_ISR;
      case 0x070:
        if (v == 0) return VM_RESET;
        m->status = v;
        break;
      case 0x080: if (q) dir_lo(&q->desc, v); break;
      case 0x084: if (q) dir_hi(&q->desc, v); break;
      case 0x090: if (q) dir_lo(&q->avail, v); break;
      case 0x094: if (q) dir_hi(&q->avail, v); break;
      case 0x0a0: if (q) dir_lo(&q->used, v); break;
      case 0x0a4: if (q) dir_hi(&q->used, v); break;
    }
    return VM_NADA;
  }
  uint64_t r = 0;
  switch (off) {
    case 0x000: r = 0x74726976; break;  // "virt"
    case 0x004: r = 2; break;
    case 0x008: r = devid; break;
    case 0x00c: r = 0x4b4c4e47; break;
    case 0x010: r = m->dev_feat_sel == 0 ? feat_lo : m->dev_feat_sel == 1 ? 1 : 0; break;  // VERSION_1
    case 0x034: r = q ? QMAX : 0; break;
    case 0x044: r = q ? q->ready : 0; break;
    case 0x060: r = m->isr; break;
    case 0x070: r = m->status; break;
  }
  *val = r;
  return VM_NADA;
}

// Espacio de configuración: lectura de size bytes de cfg en o.
static uint64_t cfg_leer(const void *cfg, size_t n, uint64_t o, int size) {
  uint64_t r = 0;
  if (o + (uint64_t)size <= n) memcpy(&r, (const uint8_t *)cfg + o, (size_t)size);
  return r;
}

// ===================== block =====================

// Cada disco tiene su hilo: el vCPU solo apunta que hay trabajo (notify) y
// sigue; el hilo lee y escribe el fichero y sube la interrupción. Con la VM en
// pausa el hilo no empieza una tanda, y pausar espera a que acabe la que lleva
// (vblk_quieto): el snapshot no puede ver la RAM a medio escribir.
struct vblk {
  int fd;
  bool ro;
  bool sin_flush;  // desechable: los FLUSH del invitado no hacen fsync
  uint64_t sectores;
  uint32_t intid;
  struct vblk_state s;
  pthread_mutex_t mu;
  pthread_cond_t cv;  // hay trabajo, o el hilo acabó una tanda
  bool pend, ocupado, vivo;
  pthread_t hilo;
};

static struct vblk blks[KHV_MAX_DISKS];

// Atiende las peticiones pendientes de la cola. Solo desde el hilo del disco,
// con ocupado puesto. *irq: hay que interrumpir al invitado (hubo respuestas y
// el driver las quiere, ver vq_interrumpir); *quedan: la tanda se cortó con
// peticiones pendientes y hay que volver a mirar sin esperar un aviso.
//
// q y m son COPIAS tomadas bajo b->mu, y aquí solo se usan ellas: el vCPU
// puede escribir los registros de la cola mientras tanto (sin mu de este
// hilo), y un QueueNum que creciera a mitad de tanda haría que
// vq_pedir_aviso escribiera el avail_event más allá de lo que vq_ptrs
// comprobó, hasta 2 KiB fuera de la RAM. Quien llama vuelca q->last_avail en
// b->s.q bajo b->mu al acabar.
static bool b_procesar(struct vblk *b, struct vq *q, const struct vmmio *m, bool *irq, bool *quedan) {
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  *irq = *quedan = false;
  if (!vq_ptrs(q, &avail, &used, &desc)) return false;
  bool hecho = false;
  uint16_t viejo = *(volatile uint16_t *)(used + 2);
  // Como mucho una vuelta a la cola por aviso (dos pasadas de vueltas
  // cuentan también las de volver a mirar tras pedir aviso): un invitado que
  // ponga el used encima del avail haría que cada respuesta añadiera una
  // petición, y el hilo no saldría nunca (ni dejaría pausar la VM).
  for (uint32_t vueltas = 0;; vueltas++) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    uint16_t aidx = *(volatile uint16_t *)&avail[1];
    if (q->last_avail == aidx) {
      // Vacía: con EVENT_IDX, pedir el aviso del siguiente y volver a mirar
      // (el driver pudo añadir justo antes de ver la petición).
      if (vueltas < 2 * q->num && vq_pedir_aviso(m, q, avail, used)) continue;
      break;
    }
    if (vueltas >= 2 * q->num) {
      vq_pedir_aviso_ya(m, q, avail, used);
      *quedan = true;
      break;
    }
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;

    // Recorre la cadena: cabecera, datos, estado (1 byte, el último).
    struct vdesc ch[QMAX];
    int n = vq_cadena(q, desc, head, ch, QMAX);
    if (n < 0) n = 0;
    uint32_t escrito = 0;
    uint8_t estado = 0;
    struct {
      uint32_t type, reserved;
      uint64_t sector;
    } hdr;
    void *hp = n >= 2 ? gpa_ptr(ch[0].addr, sizeof hdr) : NULL;
    uint8_t *sp = n >= 2 && (ch[n - 1].flags & VIRTQ_DESC_F_WRITE) ? gpa_ptr(ch[n - 1].addr, 1) : NULL;
    if (!hp || !sp || ch[0].len < sizeof hdr) {
      estado = 1;
    } else {
      memcpy(&hdr, hp, sizeof hdr);
      uint64_t off = hdr.sector * 512;
      for (int k = 1; k < n - 1 && estado == 0; k++) {
        void *d = gpa_ptr(ch[k].addr, ch[k].len);
        // Ni fuera de la RAM ni fuera del disco: un pwrite más allá del final
        // haría crecer el overlay a voluntad del invitado.
        bool rw = hdr.type == 0 || hdr.type == 1;
        if (!d || (rw && (hdr.sector > b->sectores || ch[k].len > (b->sectores * 512 - off) ||
                          off > b->sectores * 512))) {
          estado = 1;
          break;
        }
        ssize_t r = 0;
        switch (hdr.type) {
          case 0:  // IN
            r = pread(b->fd, d, ch[k].len, (off_t)off);
            if (r >= 0 && (uint32_t)r < ch[k].len) memset((uint8_t *)d + r, 0, ch[k].len - (uint32_t)r);
            if (r >= 0) r = ch[k].len, escrito += ch[k].len;
            break;
          case 1:  // OUT
            if (b->ro) r = -1;
            else r = pwrite(b->fd, d, ch[k].len, (off_t)off);
            break;
          case 4: break;  // FLUSH: sin datos
          case 8: {       // GET_ID
            char id[20] = "kling-hv";
            uint32_t l = ch[k].len < 20 ? ch[k].len : 20;
            memcpy(d, id, l);
            escrito += l;
            r = l;
            break;
          }
          default: estado = 2;
        }
        if (r < 0) estado = 1;
        off += ch[k].len;
      }
      if (hdr.type == 4 && estado == 0 && !b->sin_flush) {
        // F_FULLFSYNC: el fsync de macOS deja los datos en la caché del SSD, y
        // un corte se los lleva aunque el invitado ya oyó que estaban. Es lo
        // que hace kling-vz (VZDiskImageSynchronizationModeFull). Donde no se
        // admite (un volumen de red), fsync, como hace Go. El overlay
        // desechable (kling_disposable) ni eso, como el synchronizationMode
        // none de vz: lo escrito queda en la caché del host, y el núcleo lo
        // asienta antes de conservar el disco (internal/machine/desechable.go).
        if (fcntl(b->fd, F_FULLFSYNC) != 0 && (errno != ENOTSUP || fsync(b->fd) != 0)) estado = 1;
      }
    }
    if (sp) *sp = estado;  // sin byte de estado (cadena inválida) no hay dónde decirlo
    vq_push(q, used, head, sp ? escrito + 1 : 0);
    hecho = true;
  }
  *irq = hecho && vq_interrumpir(m, q, avail, viejo, *(volatile uint16_t *)(used + 2));
  return hecho;
}

static void *vblk_hilo(void *a) {
  struct vblk *b = a;
  khv_qos_dispositivo();
  pthread_mutex_lock(&b->mu);
  for (;;) {
    while (!(b->pend && !g_pausa)) pthread_cond_wait(&b->cv, &b->mu);
    b->pend = false;
    b->ocupado = true;
    struct vq q = b->s.q;
    struct vmmio m = b->s.m;
    pthread_mutex_unlock(&b->mu);
    bool irq, quedan;
    b_procesar(b, &q, &m, &irq, &quedan);
    pthread_mutex_lock(&b->mu);
    b->s.q.last_avail = q.last_avail;
    b->ocupado = false;
    if (irq) {
      __atomic_fetch_add(&khv_avisos[KHV_N_BLK_IRQ], 1, __ATOMIC_RELAXED);
      b->s.m.isr |= 1;
      vmmio_irq(&b->s.m, b->intid);
    }
    if (quedan) b->pend = true;
    pthread_cond_broadcast(&b->cv);
  }
  return NULL;
}

// vblk_prueba atiende la cola del disco idx en el hilo que llama (solo para
// las pruebas de prueba.c: sin esperar al hilo del disco).
void vblk_prueba(int idx) {
  struct vblk *b = &blks[idx];
  pthread_mutex_lock(&b->mu);
  while (b->ocupado) pthread_cond_wait(&b->cv, &b->mu);
  b->ocupado = true;
  struct vq q = b->s.q;
  struct vmmio m = b->s.m;
  pthread_mutex_unlock(&b->mu);
  bool irq, quedan;
  b_procesar(b, &q, &m, &irq, &quedan);
  pthread_mutex_lock(&b->mu);
  b->s.q.last_avail = q.last_avail;
  b->ocupado = false;
  pthread_cond_broadcast(&b->cv);
  pthread_mutex_unlock(&b->mu);
}

int vblk_init(int idx, const char *ruta, bool ro, bool sin_flush, uint32_t intid) {
  struct vblk *b = &blks[idx];
  b->fd = open(ruta, ro ? O_RDONLY : O_RDWR);
  if (b->fd < 0) {
    fallo("cannot open the disk %s: %s", ruta, strerror(errno));
    return -1;
  }
  struct stat st;
  fstat(b->fd, &st);
  b->sectores = (uint64_t)st.st_size / 512;
  b->ro = ro;
  b->sin_flush = sin_flush && !ro;
  b->intid = intid;
  memset(&b->s, 0, sizeof b->s);
  pthread_mutex_init(&b->mu, NULL);
  pthread_cond_init(&b->cv, NULL);
  b->vivo = true;
  pthread_create(&b->hilo, NULL, vblk_hilo, b);
  pthread_detach(b->hilo);
  return 0;
}

static uint64_t b_features(struct vblk *b) {
  // VIRTIO_BLK_F_SEG_MAX (2), VIRTIO_BLK_F_RO (5), VIRTIO_BLK_F_FLUSH (9) y
  // VIRTIO_RING_F_EVENT_IDX (29): el driver avisa y recibe interrupciones solo
  // cuando hace falta, no por cada petición.
  return (1u << 2) | (1u << 9) | khv_event_idx | (b->ro ? (1u << 5) : 0);
}

bool vblk_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size) {
  struct vblk *b = &blks[idx];
  pthread_mutex_lock(&b->mu);
  if (off >= 0x100) {  // configuración: capacity (u64), size_max (u32), seg_max (u32)
    if (!escribir) {
      uint8_t cfg[64] = {0};
      memcpy(cfg, &b->sectores, 8);
      uint32_t segmax = QMAX - 2;
      memcpy(cfg + 12, &segmax, 4);
      *val = cfg_leer(cfg, sizeof cfg, off - 0x100, size);
    }
    pthread_mutex_unlock(&b->mu);
    return true;
  }
  uint32_t cola = 0;
  switch (vmmio_acceso(&b->s.m, &b->s.q, 1, 2, b_features(b), off, escribir, val, &cola)) {
    case VM_NOTIFY:
      if (cola == 0) {
        __atomic_fetch_add(&khv_avisos[KHV_N_BLK], 1, __ATOMIC_RELAXED);
        b->pend = true;
        pthread_cond_broadcast(&b->cv);
      }
      break;
    case VM_ISR: vmmio_irq(&b->s.m, b->intid); break;
    case VM_RESET:
      while (b->ocupado) pthread_cond_wait(&b->cv, &b->mu);
      {
        uint8_t nivel = b->s.m.nivel;
        memset(&b->s, 0, sizeof b->s);
        b->s.m.nivel = nivel;
        vmmio_irq(&b->s.m, b->intid);
      }
      break;
  }
  pthread_mutex_unlock(&b->mu);
  return true;
}

void vblk_quieto(int idx) {
  struct vblk *b = &blks[idx];
  pthread_mutex_lock(&b->mu);
  while (b->ocupado) pthread_cond_wait(&b->cv, &b->mu);
  pthread_mutex_unlock(&b->mu);
}

void vblk_despertar(int idx) {
  struct vblk *b = &blks[idx];
  pthread_mutex_lock(&b->mu);
  b->pend = true;  // mirar la cola no cuesta nada si está vacía
  pthread_cond_broadcast(&b->cv);
  pthread_mutex_unlock(&b->mu);
}

void vblk_get(int idx, struct vblk_state *s) {
  pthread_mutex_lock(&blks[idx].mu);
  *s = blks[idx].s;
  pthread_mutex_unlock(&blks[idx].mu);
}

void vblk_reafirmar(int idx) {
  pthread_mutex_lock(&blks[idx].mu);
  hv_gic_set_spi(blks[idx].intid, blks[idx].s.m.nivel);
  pthread_mutex_unlock(&blks[idx].mu);
}

void vblk_set(int idx, const struct vblk_state *s) {
  pthread_mutex_lock(&blks[idx].mu);
  blks[idx].s = *s;
  pthread_mutex_unlock(&blks[idx].mu);
}

// ===================== rng =====================

// virtio-rng: cada búfer escribible se llena con arc4random_buf (el CSPRNG del
// sistema). Corre en el hilo del vCPU: es memoria a memoria y no bloquea.
static struct vrng_state rs;
static uint32_t r_intid;
static pthread_mutex_t r_mu = PTHREAD_MUTEX_INITIALIZER;

void vrng_init(uint32_t intid) {
  r_intid = intid;
  memset(&rs, 0, sizeof rs);
}

static void r_procesar(void) {
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(&rs.q, &avail, &used, &desc)) return;
  bool hecho = false;
  for (uint32_t vueltas = 0; vueltas < rs.q.num; vueltas++) {  // ver b_procesar
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (rs.q.last_avail == *(volatile uint16_t *)&avail[1]) break;
    uint16_t head = avail[2 + rs.q.last_avail % rs.q.num];
    rs.q.last_avail++;
    uint32_t total = 0;
    struct vdesc ch[QMAX];
    int n = vq_cadena(&rs.q, desc, head, ch, QMAX);
    for (int k = 0; k < n && total < 65536; k++) {
      if (!(ch[k].flags & VIRTQ_DESC_F_WRITE)) continue;
      uint32_t l = ch[k].len > 65536 - total ? 65536 - total : ch[k].len;
      void *d = gpa_ptr(ch[k].addr, l);
      if (d) {
        arc4random_buf(d, l);
        total += l;
      }
    }
    vq_push(&rs.q, used, head, total);
    hecho = true;
  }
  if (hecho) {
    rs.m.isr |= 1;
    vmmio_irq(&rs.m, r_intid);
  }
}

bool vrng_mmio(uint64_t off, bool escribir, uint64_t *val, int size) {
  pthread_mutex_lock(&r_mu);
  if (off >= 0x100) {  // sin espacio de configuración
    if (!escribir) *val = 0;
    pthread_mutex_unlock(&r_mu);
    return true;
  }
  uint32_t cola = 0;
  switch (vmmio_acceso(&rs.m, &rs.q, 1, 4, 0, off, escribir, val, &cola)) {
    case VM_NOTIFY:
      if (cola == 0) r_procesar();
      break;
    case VM_ISR: vmmio_irq(&rs.m, r_intid); break;
    case VM_RESET: {
      uint8_t nivel = rs.m.nivel;
      memset(&rs, 0, sizeof rs);
      rs.m.nivel = nivel;
      vmmio_irq(&rs.m, r_intid);
      break;
    }
  }
  pthread_mutex_unlock(&r_mu);
  return true;
}

void vrng_get(struct vrng_state *s) {
  pthread_mutex_lock(&r_mu);
  *s = rs;
  pthread_mutex_unlock(&r_mu);
}

void vrng_reafirmar(void) {
  pthread_mutex_lock(&r_mu);
  hv_gic_set_spi(r_intid, rs.m.nivel);
  pthread_mutex_unlock(&r_mu);
}

void vrng_set(const struct vrng_state *s) {
  pthread_mutex_lock(&r_mu);
  rs = *s;
  pthread_mutex_unlock(&r_mu);
}
