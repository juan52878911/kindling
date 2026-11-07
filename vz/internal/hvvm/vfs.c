//go:build darwin && arm64

// virtio-fs (virtio 1.x, 5.11; device id 26): las carpetas compartidas vivas
// de kling-hv. El C solo transporta: saca cada petición FUSE de sus colas (la
// de alta prioridad, con los FORGET, y una de peticiones), valida la cadena,
// la copia a un búfer de Go (khv_vfs_leer) y escribe en la cadena la
// respuesta que da Go (khv_vfs_responder). Quien entiende FUSE es Go: el mismo
// fuseFS del agente (pkg/guest), que habla con internal/share (os.Root) por la
// sesión kling-share/1 de siempre. Ver docs/backend-hv.md y docs/compartir.md.
//
// El unique de cada petición se cambia, en la copia que ve Go, por una ficha
// del C (generación, cola y head): la respuesta dice a qué cadena va sin fiarse
// de los unique del invitado, que podrían repetirse, y una respuesta tardía a
// una cadena que ya no espera (reset, otra generación) no escribe nada. Al
// escribir la respuesta se pone otra vez el unique del invitado.
//
// Sin hilo propio: lee el hilo de Go que llama a khv_vfs_leer (bloqueado en
// la cv del dispositivo hasta que hay petición) y escribe el que contesta.
// Con la VM en pausa (g_pausa) ninguno toca la RAM: esperan, y pausar espera
// a los que estuvieran dentro (vvfs_quieto).
//
// Lo que Go tiene en vuelo al hacer snapshot (peticiones leídas sin respuesta)
// va en el estado como un mapa de heads por cola; al restaurar, khv_vfs_leer
// las vuelve a dar antes que las nuevas: el invitado las sigue esperando y su
// cadena sigue en la RAM restaurada.
#include <errno.h>
#include <string.h>
#include <sys/uio.h>

#include "khv.h"
#include "vq_iov.h"

#define VFS_HDR_IN 40   // struct fuse_in_header
#define VFS_HDR_OUT 16  // struct fuse_out_header
// Una tabla indirecta como mucho tiene esto: una petición de 1 MiB (max_pages
// 256) son ~260 búferes. Linux la pone siempre que puede.
#define VFS_MAX_IND 512
#define VFS_MAX_IOV (VFS_MAX_IND + QMAX)
#define VIRTIO_RING_F_INDIRECT_DESC 28

// Opcodes FUSE que no llevan respuesta: el C devuelve su cadena en seguida
// (con 0 bytes) y Go solo los apunta.
#define FUSE_FORGET 2
#define FUSE_INTERRUPT 36
#define FUSE_BATCH_FORGET 42

struct vfs_vuelo {
  uint64_t unique;  // el del invitado
  uint32_t gen;     // la de la ficha que tiene Go
  uint8_t vivo;     // VFS_ESPERA o VFS_ESCRIBIENDO; 0 si nadie espera
};
#define VFS_ESPERA 1
#define VFS_ESCRIBIENDO 2

struct vvfs {
  bool vivo, cerrado;
  char tag[VFS_TAG_MAX];
  uint32_t intid;
  struct vvfs_state s;
  pthread_mutex_t mu;
  pthread_cond_t cv;  // hay petición, se despausó, o alguien salió de la RAM
  int ocupado;        // llamadas que tocan la RAM fuera del cerrojo
  uint32_t gen;
  int turno;  // la cola por la que se empieza a mirar (se alternan)
  struct vfs_vuelo vuelo[VFS_NQ][QMAX];
  uint16_t rep[VFS_NQ][QMAX];  // heads que repetir tras restaurar
  int nrep[VFS_NQ];
};

static struct vvfs vfss[KHV_MAX_VFS];

// vfs_sin_pausa: vvfs_prueba atiende las colas sin VM, con g_pausa puesta.
static bool vfs_sin_pausa;
#define VFS_EN_PAUSA (g_pausa && !vfs_sin_pausa)

static struct vvfs *vfs_de(int idx) {
  if (idx < 0 || idx >= KHV_MAX_VFS || !vfss[idx].vivo) return NULL;
  return &vfss[idx];
}

// vfs_cadena recorre la cadena de head y pone en r los búferes de lectura (que
// van primero) y en w los de escritura, con sus sumas. Una tabla indirecta
// (VIRTIO_RING_F_INDIRECT_DESC) solo vale como descriptor único de la cadena,
// que es como la pone Linux. -1 si la cadena no vale: fuera de la tabla o de
// la RAM, un ciclo, una lectura tras una escritura, un indirecto dentro de
// otro o más búferes de los que caben. Cada descriptor se lee una sola vez:
// el invitado puede cambiarlo mientras tanto.
static int vfs_cadena(const struct vq *q, const struct vdesc *desc, uint16_t head, struct iovec *r, int *nr,
                      uint64_t *tr, struct iovec *w, int *nw, uint64_t *tw) {
  const struct vdesc *tabla = desc;
  uint32_t ntabla = q->num, vistos = 0;
  bool indirecta = false;
  uint16_t i = head;
  *nr = *nw = 0;
  *tr = *tw = 0;
  for (;;) {
    if (i >= ntabla || vistos >= ntabla) return -1;
    struct vdesc d;
    memcpy(&d, (const void *)&tabla[i], sizeof d);
    vistos++;
    if (d.flags & VIRTQ_DESC_F_INDIRECT) {
      if (indirecta || vistos != 1 || (d.flags & VIRTQ_DESC_F_NEXT) || d.len == 0 || d.len % sizeof(struct vdesc) ||
          d.len / sizeof(struct vdesc) > VFS_MAX_IND)
        return -1;
      tabla = gpa_ptr(d.addr, d.len);
      if (!tabla) return -1;
      ntabla = d.len / (uint32_t)sizeof(struct vdesc);
      indirecta = true;
      vistos = 0;
      i = 0;
      continue;
    }
    if (d.len > 0) {
      void *p = gpa_ptr(d.addr, d.len);
      if (!p) return -1;
      if (d.flags & VIRTQ_DESC_F_WRITE) {
        if (*nw >= VFS_MAX_IOV) return -1;
        w[*nw].iov_base = p;
        w[*nw].iov_len = d.len;
        (*nw)++;
        *tw += d.len;
      } else {
        if (*nw > 0 || *nr >= VFS_MAX_IOV) return -1;
        r[*nr].iov_base = p;
        r[*nr].iov_len = d.len;
        (*nr)++;
        *tr += d.len;
      }
    }
    if (!(d.flags & VIRTQ_DESC_F_NEXT)) return 0;
    i = d.next;
  }
}

// vfs_error escribe en w una respuesta FUSE de solo cabecera con el error err
// (negativo, como lo espera el kernel). Devuelve los bytes escritos: 0 si no
// caben.
static uint32_t vfs_error(const struct iovec *w, int nw, uint64_t tw, uint64_t unique, int32_t err) {
  if (tw < VFS_HDR_OUT) return 0;
  uint8_t h[VFS_HDR_OUT];
  uint32_t len = VFS_HDR_OUT;
  memcpy(h, &len, 4);
  memcpy(h + 4, &err, 4);
  memcpy(h + 8, &unique, 8);
  iov_poner(w, nw, h, sizeof h);
  return VFS_HDR_OUT;
}

// vfs_devolver pone head en el used de la cola con len bytes escritos y avisa
// al driver si hace falta. Con el cerrojo.
static void vfs_devolver(struct vvfs *v, int cola, uint16_t head, uint32_t len) {
  struct vq *q = &v->s.q[cola];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;  // el driver quitó la cola: nadie espera
  uint16_t viejo = q->used_idx;
  vq_push(q, used, head, len);
  if (vq_interrumpir(&v->s.m, q, avail, viejo, q->used_idx)) {
    v->s.m.isr |= 1;
    vmmio_irq(&v->s.m, v->intid);
  }
}

// vfs_siguiente toma el siguiente head pendiente (los que hay que repetir
// primero) de alguna cola. Con el cerrojo y sin pausa. false si no hay.
static bool vfs_siguiente(struct vvfs *v, int *cola, uint16_t *head) {
  for (int k = 0; k < VFS_NQ; k++) {
    int c = (v->turno + k) % VFS_NQ;
    struct vq *q = &v->s.q[c];
    uint16_t *avail;
    uint8_t *used;
    struct vdesc *desc;
    if (!vq_ptrs(q, &avail, &used, &desc)) continue;
    if (v->nrep[c] > 0) {
      *head = v->rep[c][--v->nrep[c]];
    } else {
      __atomic_thread_fence(__ATOMIC_ACQUIRE);
      uint16_t aidx = *(volatile uint16_t *)&avail[1];
      if (q->last_avail == aidx) continue;
      *head = avail[2 + q->last_avail % q->num];
      q->last_avail++;
    }
    *cola = c;
    v->turno = c + 1;
    return true;
  }
  return false;
}

// Resultado de vfs_copiar: la petición es para Go, o el C ya la contestó (o
// la tiró: cadena inválida) y no hay nada que dar.
enum { VFS_PARA_GO, VFS_HECHA };

// vfs_copiar copia la parte de lectura de la cadena de head a buf. Fuera del
// cerrojo (con ocupado puesto) y con q copiada. Si no es para Go, *len es lo
// que hay que poner en el used.
static int vfs_copiar(const struct vq *q, uint16_t head, uint8_t *buf, uint32_t cap, uint32_t *n,
                      uint32_t *len) {
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  struct vq c = *q;
  struct iovec r[VFS_MAX_IOV], w[VFS_MAX_IOV];
  int nr, nw;
  uint64_t tr, tw;
  *len = 0;
  if (!vq_ptrs(&c, &avail, &used, &desc) || vfs_cadena(&c, desc, head, r, &nr, &tr, w, &nw, &tw) != 0)
    return VFS_HECHA;
  if (tr < VFS_HDR_IN) return VFS_HECHA;  // sin cabecera no hay ni a quién contestar
  if (tr > cap) {
    // Más grande que lo que se negoció en INIT (max_write): un invitado que
    // miente. Se le contesta EIO sin pasarla a Go.
    uint64_t unique;
    uint8_t cab[VFS_HDR_IN];
    size_t o = 0;
    for (int k = 0; k < nr && o < sizeof cab; k++) {
      size_t l = r[k].iov_len < sizeof cab - o ? r[k].iov_len : sizeof cab - o;
      memcpy(cab + o, r[k].iov_base, l);
      o += l;
    }
    memcpy(&unique, cab + 8, 8);
    *len = vfs_error(w, nw, tw, unique, -EIO);
    return VFS_HECHA;
  }
  uint32_t o = 0;
  for (int k = 0; k < nr; k++) {
    memcpy(buf + o, r[k].iov_base, r[k].iov_len);
    o += (uint32_t)r[k].iov_len;
  }
  *n = o;
  return VFS_PARA_GO;
}

// vfs_tomar es khv_vfs_leer, que puede no esperar (las pruebas).
//
// Las cadenas que contesta el propio C (inválidas, demasiado grandes) se
// atienden como mucho de 2*QMAX en 2*QMAX por despertar: un invitado que ponga
// el used encima del avail haría que cada una añadiera otra, y el lector no
// volvería nunca.
static int vfs_tomar(struct vvfs *v, uint8_t *buf, uint32_t cap, bool bloquear) {
  pthread_mutex_lock(&v->mu);
  int hechas = 0;
  for (;;) {
    if (v->cerrado) {
      pthread_mutex_unlock(&v->mu);
      return -1;
    }
    int cola = -1;
    uint16_t head = 0;
    if (VFS_EN_PAUSA || hechas >= 2 * QMAX || !vfs_siguiente(v, &cola, &head)) {
      if (!bloquear) {
        pthread_mutex_unlock(&v->mu);
        return 0;
      }
      pthread_cond_wait(&v->cv, &v->mu);
      hechas = 0;
      continue;
    }
    struct vq q = v->s.q[cola];
    v->ocupado++;
    pthread_mutex_unlock(&v->mu);
    uint32_t n = 0, len = 0;
    // Un head fuera de la cola no tiene cadena (y no indexa vuelo).
    int r = head < q.num ? vfs_copiar(&q, head, buf, cap, &n, &len) : VFS_HECHA;
    pthread_mutex_lock(&v->mu);
    v->ocupado--;
    pthread_cond_broadcast(&v->cv);
    if (r == VFS_HECHA) {
      if (head < QMAX && !v->vuelo[cola][head].vivo) vfs_devolver(v, cola, head, len);
      hechas++;
      continue;
    }
    struct vfs_vuelo *f = &v->vuelo[cola][head];
    if (f->vivo) {
      // El mismo head dos veces en vuelo: el invitado rompió el protocolo.
      // La copia nueva se tira; la primera sigue esperando su respuesta.
      hechas++;
      continue;
    }
    uint32_t opcode;
    memcpy(&opcode, buf + 4, 4);
    if (++v->gen == 0) v->gen = 1;
    memcpy(&f->unique, buf + 8, 8);
    f->gen = v->gen;
    uint64_t ficha = (uint64_t)v->gen << 32 | (uint64_t)cola << 16 | head;
    memcpy(buf + 8, &ficha, 8);
    if (opcode == FUSE_FORGET || opcode == FUSE_BATCH_FORGET || opcode == FUSE_INTERRUPT) {
      vfs_devolver(v, cola, head, 0);
    } else {
      f->vivo = VFS_ESPERA;
    }
    pthread_mutex_unlock(&v->mu);
    return (int)n;
  }
}

int khv_vfs_leer(int idx, void *buf, uint32_t cap) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return -1;
  return vfs_tomar(v, buf, cap, true);
}

int khv_vfs_responder(int idx, const void *src, uint32_t len) {
  struct vvfs *v = vfs_de(idx);
  if (!v || len < VFS_HDR_OUT) return -1;
  uint64_t ficha;
  memcpy(&ficha, (const uint8_t *)src + 8, 8);
  // unique 0: un aviso del servidor (FUSE_NOTIFY_*). virtio-fs no tiene cola
  // de avisos (Linux no la implementa): se tira.
  if (ficha == 0) return 0;
  uint32_t gen = (uint32_t)(ficha >> 32);
  uint32_t cola = (uint32_t)(ficha >> 16) & 0xffff;
  uint16_t head = (uint16_t)ficha;
  if (cola >= VFS_NQ || head >= QMAX) return -1;
  pthread_mutex_lock(&v->mu);
  while (VFS_EN_PAUSA && !v->cerrado) pthread_cond_wait(&v->cv, &v->mu);
  struct vfs_vuelo *f = &v->vuelo[cola][head];
  if (v->cerrado || f->vivo != VFS_ESPERA || f->gen != gen) {
    pthread_mutex_unlock(&v->mu);
    return -1;
  }
  // Mientras se escribe sigue en vuelo: el mismo head puesto otra vez por el
  // invitado se tira (vfs_tomar) y otra respuesta con la misma ficha no pasa.
  f->vivo = VFS_ESCRIBIENDO;
  uint64_t unique = f->unique;
  struct vq q = v->s.q[cola];
  v->ocupado++;
  pthread_mutex_unlock(&v->mu);

  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  struct iovec r[VFS_MAX_IOV], w[VFS_MAX_IOV], resto[VFS_MAX_IOV];
  int nr, nw;
  uint64_t tr, tw;
  uint32_t escrito = 0;
  if (head < q.num && vq_ptrs(&q, &avail, &used, &desc) &&
      vfs_cadena(&q, desc, head, r, &nr, &tr, w, &nw, &tw) == 0) {
    if (len > tw) {
      // No cabe donde dejó sitio el driver: no debería pasar (Go contesta
      // como mucho lo pedido).
      escrito = vfs_error(w, nw, tw, unique, -EIO);
    } else {
      uint8_t cab[VFS_HDR_OUT];
      memcpy(cab, src, VFS_HDR_OUT);
      memcpy(cab + 8, &unique, 8);
      iov_poner(w, nw, cab, VFS_HDR_OUT);
      int k = iov_tramo(w, nw, VFS_HDR_OUT, len - VFS_HDR_OUT, resto);
      iov_poner(resto, k, (const uint8_t *)src + VFS_HDR_OUT, len - VFS_HDR_OUT);
      escrito = len;
    }
  }

  pthread_mutex_lock(&v->mu);
  v->ocupado--;
  // Un reset espera a que ocupado baje, así que el vuelo sigue siendo este.
  if (f->vivo == VFS_ESCRIBIENDO && f->gen == gen) {
    f->vivo = 0;
    vfs_devolver(v, (int)cola, head, escrito);
  }
  pthread_cond_broadcast(&v->cv);
  pthread_mutex_unlock(&v->mu);
  return 0;
}

void khv_vfs_cerrar(int idx) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return;
  pthread_mutex_lock(&v->mu);
  v->cerrado = true;
  pthread_cond_broadcast(&v->cv);
  pthread_mutex_unlock(&v->mu);
}

int vvfs_init(int idx, const char *tag, uint32_t intid) {
  if (idx < 0 || idx >= KHV_MAX_VFS) return -1;
  size_t l = tag ? strlen(tag) : 0;
  if (l == 0 || l >= VFS_TAG_MAX) {
    fallo("virtio-fs tag %d must have 1 to %d bytes", idx, VFS_TAG_MAX - 1);
    return -1;
  }
  struct vvfs *v = &vfss[idx];
  if (!v->vivo) {
    pthread_mutex_init(&v->mu, NULL);
    pthread_cond_init(&v->cv, NULL);
  }
  pthread_mutex_lock(&v->mu);
  memset(v->tag, 0, sizeof v->tag);
  memcpy(v->tag, tag, l);
  v->intid = intid;
  memset(&v->s, 0, sizeof v->s);
  memset(v->vuelo, 0, sizeof v->vuelo);
  memset(v->nrep, 0, sizeof v->nrep);
  v->cerrado = false;
  v->vivo = true;
  pthread_mutex_unlock(&v->mu);
  return 0;
}

static void vfs_reset(struct vvfs *v) {
  while (v->ocupado > 0) pthread_cond_wait(&v->cv, &v->mu);
  uint8_t nivel = v->s.m.nivel;
  memset(&v->s, 0, sizeof v->s);
  v->s.m.nivel = nivel;
  // Lo que tuviera Go ya no tiene adónde ir: sus respuestas no casan.
  memset(v->vuelo, 0, sizeof v->vuelo);
  memset(v->nrep, 0, sizeof v->nrep);
  vmmio_irq(&v->s.m, v->intid);
}

bool vvfs_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return false;
  pthread_mutex_lock(&v->mu);
  if (off >= 0x100) {  // configuración: tag[36] y num_request_queues (u32)
    if (!escribir) {
      uint8_t cfg[VFS_TAG_MAX + 4] = {0};
      memcpy(cfg, v->tag, VFS_TAG_MAX);
      uint32_t nreq = VFS_NQ - 1;
      memcpy(cfg + VFS_TAG_MAX, &nreq, 4);
      uint64_t o = off - 0x100, r = 0;
      if (o + (uint64_t)size <= sizeof cfg) memcpy(&r, cfg + o, (size_t)size);
      *val = r;
    }
    pthread_mutex_unlock(&v->mu);
    return true;
  }
  uint32_t cola = 0;
  switch (vmmio_acceso(&v->s.m, v->s.q, VFS_NQ, 26, 1u << VIRTIO_RING_F_INDIRECT_DESC, off, escribir, val, &cola)) {
    case VM_NOTIFY:
      if (cola < VFS_NQ) pthread_cond_broadcast(&v->cv);
      break;
    case VM_ISR: vmmio_irq(&v->s.m, v->intid); break;
    case VM_RESET: vfs_reset(v); break;
  }
  pthread_mutex_unlock(&v->mu);
  return true;
}

void vvfs_quieto(int idx) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return;
  pthread_mutex_lock(&v->mu);
  while (v->ocupado > 0) pthread_cond_wait(&v->cv, &v->mu);
  pthread_mutex_unlock(&v->mu);
}

void vvfs_despertar(int idx) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return;
  pthread_mutex_lock(&v->mu);
  pthread_cond_broadcast(&v->cv);
  pthread_mutex_unlock(&v->mu);
}

// vvfs_get: el estado de las colas y, en vuelo, lo que Go tiene leído sin
// contestar (y lo que aún quedaba por repetir de una restauración anterior).
void vvfs_get(int idx, struct vvfs_state *s) {
  struct vvfs *v = vfs_de(idx);
  memset(s, 0, sizeof *s);
  if (!v) return;
  pthread_mutex_lock(&v->mu);
  *s = v->s;
  memset(s->vuelo, 0, sizeof s->vuelo);
  for (int c = 0; c < VFS_NQ; c++) {
    for (int h = 0; h < QMAX; h++)
      if (v->vuelo[c][h].vivo) s->vuelo[c][h / 8] |= (uint8_t)(1u << (h % 8));
    for (int k = 0; k < v->nrep[c]; k++) s->vuelo[c][v->rep[c][k] / 8] |= (uint8_t)(1u << (v->rep[c][k] % 8));
  }
  pthread_mutex_unlock(&v->mu);
}

void vvfs_reafirmar(int idx) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return;
  pthread_mutex_lock(&v->mu);
  hv_gic_set_spi(v->intid, v->s.m.nivel);
  pthread_mutex_unlock(&v->mu);
}

void vvfs_set(int idx, const struct vvfs_state *s) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return;
  pthread_mutex_lock(&v->mu);
  v->s = *s;
  memset(v->vuelo, 0, sizeof v->vuelo);
  int total = 0;
  for (int c = 0; c < VFS_NQ; c++) {
    v->nrep[c] = 0;
    // Solo heads de la cola: el fichero podría venir roto.
    uint32_t num = v->s.q[c].num <= QMAX ? v->s.q[c].num : 0;
    for (uint32_t h = 0; h < num; h++)
      if (s->vuelo[c][h / 8] & (1u << (h % 8))) v->rep[c][v->nrep[c]++] = (uint16_t)h;
    total += v->nrep[c];
  }
  memset(v->s.vuelo, 0, sizeof v->s.vuelo);
  pthread_mutex_unlock(&v->mu);
  if (total > 0) khv_log("virtio-fs %s: %d request(s) in flight at the snapshot are served again", v->tag, total);
}

// vvfs_prueba atiende las colas de idx en el hilo que llama, contestando a
// cada petición con lo que diga resp (las pruebas de prueba.c: el fuzz de
// las colas, sin Go).
void vvfs_prueba(int idx, const uint8_t *resp, size_t nresp) {
  struct vvfs *v = vfs_de(idx);
  if (!v) return;
  static uint8_t buf[(1 << 16) + 64];
  vfs_sin_pausa = true;
  for (int vueltas = 0; vueltas < 2 * QMAX; vueltas++) {
    int n = vfs_tomar(v, buf, sizeof buf, false);
    if (n <= 0) break;
    // La respuesta: la cabecera con la ficha y len según los datos (a veces
    // más grande que la cadena), y unos bytes detrás.
    uint8_t out[512];
    uint32_t l = VFS_HDR_OUT + (nresp > 0 ? resp[vueltas % nresp] : 0) * 2u;
    if (l > sizeof out) l = sizeof out;
    memset(out, 0xab, sizeof out);
    memcpy(out, &l, 4);
    memset(out + 4, 0, 4);
    memcpy(out + 8, buf + 8, 8);
    khv_vfs_responder(idx, out, l);
    // Una respuesta repetida o a destiempo no debe escribir nada.
    if (nresp > 1 && resp[vueltas % nresp] & 1) khv_vfs_responder(idx, out, l);
  }
  // Una respuesta que no es de nadie, y un aviso (unique 0).
  uint8_t raro[VFS_HDR_OUT] = {VFS_HDR_OUT, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8};
  khv_vfs_responder(idx, raro, sizeof raro);
  memset(raro + 8, 0, 8);
  khv_vfs_responder(idx, raro, sizeof raro);
  vfs_sin_pausa = false;
}
