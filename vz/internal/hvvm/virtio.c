//go:build darwin && arm64

// virtio-mmio (versión 2, colas split): los registros comunes, el disco
// (virtio-blk, con un hilo por disco) y el generador de entropía (virtio-rng).
// La red está en net.c.
#include <errno.h>
#include <fcntl.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/uio.h>
#include <unistd.h>

#include "khv.h"
#include "vq_iov.h"

// ===================== común =====================

bool vq_ptrs(struct vq *q, uint16_t **avail, uint8_t **used, struct vdesc **desc) {
  if (!q->ready || q->num == 0 || q->num > QMAX) return false;
  // Las alineaciones que exige virtio (2.7: tabla a 16, avail a 2, used a 4).
  // Un driver que no las cumple está roto, y los accesos de 16 y 32 bits a
  // un anillo desalineado son comportamiento indefinido en C (lo encontró
  // UBSan con el fuzz de las colas).
  if (q->desc % 16 || q->avail % 2 || q->used % 4) return false;
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
  uint16_t uidx = q->used_idx;
  uint32_t *e = (uint32_t *)(used + 4 + 8 * (uidx % q->num));
  e[0] = head;
  e[1] = len;
  __atomic_thread_fence(__ATOMIC_RELEASE);
  q->used_idx = (uint16_t)(uidx + 1);
  *(volatile uint16_t *)(used + 2) = q->used_idx;
}

int khv_poll_fallos[KHV_POLL_N];

int khv_poll(int quien, struct pollfd *pf, nfds_t n, int plazo) {
  int f = __atomic_load_n(&khv_poll_fallos[quien], __ATOMIC_ACQUIRE);
  if (f > 0 && __atomic_compare_exchange_n(&khv_poll_fallos[quien], &f, f - 1, false, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE)) {
    errno = EAGAIN;
    return -1;
  }
  return poll(pf, n, plazo);
}

void vq_used_de_ram(struct vq *q) {
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  q->used_idx = vq_ptrs(q, &avail, &used, &desc) ? *(volatile uint16_t *)(used + 2) : 0;
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
      // QueueNum: potencia de dos (virtio 2.6). Con otro tamaño los índices
      // de 16 bits del driver y los del dispositivo se desencajan al dar la
      // vuelta en 65536, y el anillo queda a medias.
      case 0x038: if (q && v && v <= QMAX && (v & (v - 1)) == 0) q->num = v; break;
      case 0x044:
        // Al armar la cola el driver deja used a cero (virtio 2.7.8): el
        // índice del dispositivo empieza ahí.
        if (q && !q->ready && (v & 1)) q->used_idx = 0;
        if (q) q->ready = v & 1;
        break;
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
  int sinc;       // KHV_SINC_*: qué hace un FLUSH del invitado
  uint32_t alin;  // sectores por bloque del sistema de ficheros del host (F_PUNCHHOLE)
  uint64_t sectores;
  uint32_t intid;
  struct vblk_state s;
  pthread_mutex_t mu;
  pthread_cond_t cv;  // hay trabajo, o el hilo acabó una tanda
  bool pend, ocupado, vivo;
  pthread_t hilo;
  // Para b_procesar, que solo corre con ocupado puesto: la cadena copiada y
  // sus datos como iovec (los sectores de una petición son contiguos, así
  // que van en un solo preadv/pwritev), sin 8 KiB de pila por petición.
  struct vdesc ch[QMAX];
  struct iovec iov[QMAX], tramo[QMAX];
};

static struct vblk blks[KHV_MAX_DISKS];

// Tope de bytes por llamada a preadv/pwritev: read y write de macOS no pasan
// de INT_MAX, y una petición que reúne varios búferes podría.
#define B_TOPE_ES (1u << 30)

// b_es lee (escribir = false) o escribe los total bytes de iov[0..n), que en
// el disco van seguidos desde off, con preadv/pwritev; reintenta lo que quede
// tras una llamada corta o un EINTR. Devuelve los bytes hechos (menos que
// total: fin de fichero al leer o una escritura que no avanza) o -1 si hubo
// error.
static int64_t b_es(struct vblk *b, bool escribir, int n, uint64_t total, uint64_t off) {
  uint64_t hecho = 0;
  while (hecho < total) {
    uint64_t l = total - hecho < B_TOPE_ES ? total - hecho : B_TOPE_ES;
    int k = iov_tramo(b->iov, n, hecho, l, b->tramo);
    ssize_t r = escribir ? pwritev(b->fd, b->tramo, k, (off_t)(off + hecho))
                         : preadv(b->fd, b->tramo, k, (off_t)(off + hecho));
    if (r < 0 && errno == EINTR) continue;
    if (r < 0) return -1;
    if (r == 0) break;
    hecho += (uint64_t)r;
  }
  return (int64_t)hecho;
}

// Tipos de petición de DISCARD y WRITE_ZEROES (VIRTIO_BLK_T_*) y sus topes,
// que van en la configuración: sectores por segmento y segmentos por
// petición. Un DISCARD solo escribe ceros en los bordes que no llenan un
// bloque, así que puede ser grande y de muchos segmentos (fstrim los junta);
// un WRITE_ZEROES sin unmap escribe ceros de verdad, y la tanda del hilo del
// disco (que no deja pausar la VM mientras dura) no debe pasar de unas
// decenas de MiB: un segmento (Linux no manda más) de 32 MiB.
#define B_T_DISCARD 11
#define B_T_WRITE_ZEROES 13
#define B_DESCARTE_MAX (1u << 22)  // 2 GiB
#define B_CEROS_MAX (1u << 16)     // 32 MiB
#define B_SEGS 32  // los del DISCARD; WRITE_ZEROES, uno
#define B_UNMAP 1u  // VIRTIO_BLK_WRITE_ZEROES_FLAG_UNMAP, el único flag que existe

static const uint8_t b_cero[64 << 10];

// b_ceros escribe len bytes a cero desde off; 0 si fue bien.
static int b_ceros(struct vblk *b, uint64_t off, uint64_t len) {
  while (len > 0) {
    size_t l = len < sizeof b_cero ? (size_t)len : sizeof b_cero;
    ssize_t r = pwrite(b->fd, b_cero, l, (off_t)off);
    if (r < 0 && errno == EINTR) continue;
    if (r <= 0) return -1;
    off += (uint64_t)r;
    len -= (uint64_t)r;
  }
  return 0;
}

// b_hueco devuelve al host [off, off+len) con F_PUNCHHOLE. APFS solo lo admite
// en bloques enteros de su tamaño: lo de dentro se libera y los bordes sueltos
// se escriben con ceros, así que después todo el tramo se lee como ceros. Si
// el sistema de ficheros no sabe hacer huecos, escribe ceros en todo el tramo
// con ceros_si_no (WRITE_ZEROES) o lo deja estar (un DISCARD es un consejo).
static int b_hueco(struct vblk *b, uint64_t off, uint64_t len, bool ceros_si_no) {
  uint64_t bs = (uint64_t)b->alin * 512, fin = off + len;
  uint64_t a = (off + bs - 1) / bs * bs, z = fin / bs * bs;
  if (a >= z) return b_ceros(b, off, len);
  struct fpunchhole ph = {.fp_flags = 0, .reserved = 0, .fp_offset = (off_t)a, .fp_length = (off_t)(z - a)};
  if (fcntl(b->fd, F_PUNCHHOLE, &ph) != 0) {
    if (errno != ENOTSUP) return -1;
    return ceros_si_no ? b_ceros(b, off, len) : 0;
  }
  return b_ceros(b, off, a - off) != 0 || b_ceros(b, z, fin - z) != 0 ? -1 : 0;
}

// b_descartar atiende un DISCARD (ceros = false) o un WRITE_ZEROES: sus nd
// búferes de datos son segmentos {sector u64, num_sectors u32, flags u32}.
// Devuelve el byte de estado. Primero copia (el invitado puede cambiar su
// RAM mientras tanto) y valida TODOS los segmentos, como read/write validan
// sus sectores: uno malo y no se toca nada.
static uint8_t b_descartar(struct vblk *b, bool ceros, const struct vdesc *d, int nd) {
  struct {
    uint64_t sector;
    uint32_t num, flags;
  } seg[B_SEGS];
  if (b->ro) return 1;
  uint64_t n = 0;
  for (int k = 0; k < nd; k++) {
    const uint8_t *p = gpa_ptr(d[k].addr, d[k].len);
    if (!p || d[k].len % sizeof seg[0] != 0 || n + d[k].len / sizeof seg[0] > (ceros ? 1 : B_SEGS)) return 1;
    memcpy(&seg[n], p, d[k].len);
    n += d[k].len / sizeof seg[0];
  }
  if (n == 0) return 1;
  uint32_t tope = ceros ? B_CEROS_MAX : B_DESCARTE_MAX;
  for (uint64_t i = 0; i < n; i++) {
    // Un flag desconocido, o unmap en un DISCARD, es UNSUPP (virtio 1.2,
    // 5.2.6.2); fuera del disco o más largo que el tope, IOERR.
    if ((seg[i].flags & ~B_UNMAP) != 0 || (!ceros && seg[i].flags != 0)) return 2;
    if (seg[i].sector > b->sectores || seg[i].num > b->sectores - seg[i].sector || seg[i].num > tope) return 1;
  }
  for (uint64_t i = 0; i < n; i++) {
    uint64_t off = seg[i].sector * 512, len = (uint64_t)seg[i].num * 512;
    if (len == 0) continue;
    // WRITE_ZEROES sin unmap: el invitado quiere los bloques asignados
    // (fallocate sin PUNCH_HOLE, REQ_NOUNMAP), ceros de verdad.
    int r = !ceros ? b_hueco(b, off, len, false)
            : (seg[i].flags & B_UNMAP) ? b_hueco(b, off, len, true)
                                       : b_ceros(b, off, len);
    if (r != 0) return 1;
  }
  return 0;
}

// Atiende las peticiones pendientes de la cola. Solo desde el hilo del disco,
// con ocupado puesto. *irq: hay que interrumpir al invitado (hubo respuestas y
// el driver las quiere, ver vq_interrumpir); *quedan: la tanda se cortó con
// peticiones pendientes y hay que volver a mirar sin esperar un aviso.
//
// q y m son COPIAS tomadas bajo b->mu, y aquí solo se usan ellas: el vCPU
// puede escribir los registros de la cola mientras tanto (sin mu de este
// hilo), y un QueueNum que creciera a mitad de tanda haría que
// vq_pedir_aviso escribiera el avail_event más allá de lo que vq_ptrs
// comprobó, hasta 2 KiB fuera de la RAM. Quien llama vuelca q->last_avail y
// q->used_idx en b->s.q bajo b->mu al acabar.
static bool b_procesar(struct vblk *b, struct vq *q, const struct vmmio *m, bool *irq, bool *quedan) {
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  *irq = *quedan = false;
  if (!vq_ptrs(q, &avail, &used, &desc)) return false;
  bool hecho = false;
  uint16_t viejo = q->used_idx;
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
    struct vdesc *ch = b->ch;
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
      if (hdr.type == 0 || hdr.type == 1) {
        // IN y OUT: los búferes de datos, en orden, hasta el primero que no
        // vale (fuera de la RAM o fuera del disco: un pwrite más allá del
        // final haría crecer el overlay a voluntad del invitado). Los de
        // antes se leen o escriben igual, como cuando iba uno a uno, y la
        // petición acaba en error.
        int ni = 0;
        uint64_t total = 0;
        bool malo = false;
        for (int k = 1; k < n - 1; k++) {
          void *d = gpa_ptr(ch[k].addr, ch[k].len);
          if (!d || hdr.sector > b->sectores || ch[k].len > (b->sectores * 512 - off) || off > b->sectores * 512) {
            malo = true;
            break;
          }
          b->iov[ni].iov_base = d;
          b->iov[ni].iov_len = ch[k].len;
          ni++;
          total += ch[k].len;
          off += ch[k].len;
        }
        off = hdr.sector * 512;
        if (ni > 0 && hdr.type == 0) {
          // Lo que pase del final del fichero se lee como ceros.
          int64_t r = b_es(b, false, ni, total, off);
          if (r < 0) {
            estado = 1;
          } else {
            uint64_t z = (uint64_t)r;
            for (int j = 0; j < ni; j++) {
              size_t l = b->iov[j].iov_len;
              if (z >= l) {
                z -= l;
                continue;
              }
              memset((uint8_t *)b->iov[j].iov_base + z, 0, l - z);
              z = 0;
            }
            escrito += (uint32_t)total;
          }
        } else if (ni > 0) {
          // Hasta el último byte o error: un pwrite corto (ENOSPC con el
          // overlay lleno, EINTR) con r >= 0 se daba por bueno y el sistema
          // de ficheros del invitado creía escritos sectores que no lo
          // estaban.
          if (b->ro || b_es(b, true, ni, total, off) != (int64_t)total) estado = 1;
        }
        if (malo) estado = 1;
      } else if (hdr.type == B_T_DISCARD || hdr.type == B_T_WRITE_ZEROES) {
        estado = b_descartar(b, hdr.type == B_T_WRITE_ZEROES, ch + 1, n - 2);
      }
      bool hecha = hdr.type == 0 || hdr.type == 1 || hdr.type == B_T_DISCARD || hdr.type == B_T_WRITE_ZEROES;
      for (int k = 1; k < n - 1 && estado == 0 && !hecha; k++) {
        // El resto, uno a uno: sus búferes solo tienen que estar en la RAM.
        void *d = gpa_ptr(ch[k].addr, ch[k].len);
        if (!d) {
          estado = 1;
          break;
        }
        ssize_t r = 0;
        switch (hdr.type) {
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
      }
      if (hdr.type == 4 && estado == 0 && b->sinc != KHV_SINC_NONE) {
        // full, F_FULLFSYNC: el fsync de macOS deja los datos en la caché del
        // SSD, y un corte se los lleva aunque el invitado ya oyó que estaban.
        // Es lo que hace kling-vz (VZDiskImageSynchronizationModeFull).
        // barrier, F_BARRIERFSYNC: lo de antes del FLUSH llega al SSD antes
        // que lo de después (el journal de ext4 queda coherente), pero no
        // espera a que salga de su caché: un corte puede llevarse los últimos
        // FLUSH, no dejar el sistema de ficheros a medias. Donde no se admite
        // (un volumen de red), fsync, como hace Go. none no hace nada: el
        // overlay desechable (kling_disposable, como el synchronizationMode
        // none de vz; el núcleo lo asienta antes de conservar el disco,
        // internal/machine/desechable.go) o un volumen que se pidió así.
        int op = b->sinc == KHV_SINC_BARRIER ? F_BARRIERFSYNC : F_FULLFSYNC;
        if (fcntl(b->fd, op) != 0 && (errno != ENOTSUP || fsync(b->fd) != 0)) estado = 1;
      }
    }
    if (sp) *sp = estado;  // sin byte de estado (cadena inválida) no hay dónde decirlo
    vq_push(q, used, head, sp ? escrito + 1 : 0);
    hecho = true;
  }
  *irq = hecho && vq_interrumpir(m, q, avail, viejo, q->used_idx);
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
    b->s.q.used_idx = q.used_idx;
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
  b->s.q.used_idx = q.used_idx;
  b->ocupado = false;
  pthread_cond_broadcast(&b->cv);
  pthread_mutex_unlock(&b->mu);
}

// vblk_prueba_modo cambia ro y sinc del disco idx (solo pruebas, con la cola
// quieta).
void vblk_prueba_modo(int idx, bool ro, int sinc) {
  pthread_mutex_lock(&blks[idx].mu);
  blks[idx].ro = ro;
  blks[idx].sinc = sinc;
  pthread_mutex_unlock(&blks[idx].mu);
}

// vblk_prueba_bloques: los bloques de 512 B que ocupa el fichero del disco
// idx (pruebas: lo que devuelve un DISCARD).
int64_t vblk_prueba_bloques(int idx) {
  struct stat st;
  return fstat(blks[idx].fd, &st) == 0 ? (int64_t)st.st_blocks : -1;
}

int vblk_init(int idx, const char *ruta, bool ro, int sinc, uint32_t intid) {
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
  b->sinc = ro || sinc < KHV_SINC_FULL || sinc > KHV_SINC_NONE ? KHV_SINC_FULL : sinc;
  // F_PUNCHHOLE solo admite bloques enteros del sistema de ficheros (4 KiB
  // en APFS): es la alineación que se anuncia al invitado.
  struct statfs sf;
  uint32_t bs = fstatfs(b->fd, &sf) == 0 ? sf.f_bsize : 4096;
  b->alin = bs >= 512 && bs <= (1u << 20) && bs % 512 == 0 ? bs / 512 : 8;
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
  // cuando hace falta, no por cada petición. En los de escritura,
  // VIRTIO_BLK_F_DISCARD (13) y VIRTIO_BLK_F_WRITE_ZEROES (14): lo que el
  // invitado borra (fstrim, mount -o discard) vuelve al host como huecos.
  return (1u << 2) | (1u << 9) | khv_event_idx | (b->ro ? (1u << 5) : (1u << 13) | (1u << 14));
}

bool vblk_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size) {
  struct vblk *b = &blks[idx];
  pthread_mutex_lock(&b->mu);
  if (off >= 0x100) {
    // Configuración (struct virtio_blk_config): capacity (u64, 0),
    // seg_max (u32, 12) y, desde 36, max_discard_sectors, max_discard_seg,
    // discard_sector_alignment, max_write_zeroes_sectors,
    // max_write_zeroes_seg (u32) y write_zeroes_may_unmap (u8, 56).
    if (!escribir) {
      uint8_t cfg[64] = {0};
      memcpy(cfg, &b->sectores, 8);
      uint32_t segmax = QMAX - 2;
      memcpy(cfg + 12, &segmax, 4);
      uint32_t dz[5] = {B_DESCARTE_MAX, B_SEGS, b->alin, B_CEROS_MAX, 1};
      memcpy(cfg + 36, dz, sizeof dz);
      cfg[56] = 1;
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
