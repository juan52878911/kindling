// Dispositivos emulados: UART 16550 (la consola ttyS0 que ya usa kindling con
// Firecracker) y virtio-mmio block (versión 2, cola split, síncrono).
#include <fcntl.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/uio.h>
#include <unistd.h>

#include "khv.h"

// ===================== UART 16550 =====================

static pthread_mutex_t u_mu = PTHREAD_MUTEX_INITIALIZER;
static struct uart_state u;
static uint32_t u_intid;
static char rx[4096];
static size_t rx_r, rx_w;  // anillo de recepción
bool g_uart_tiempos;
static bool u_inicio_linea = true;
#define NMARCAS 4
static const char *u_marca[NMARCAS];
static int u_marca_arg[NMARCAS];
static void (*u_marca_cb[NMARCAS])(int);
static char u_hist[128];  // últimos bytes escritos, para buscar las marcas
static size_t u_nhist;

void uart_init(uint32_t intid) {
  u_intid = intid;
  memset(&u, 0, sizeof u);
}

void uart_add_marca(const char *marca, void (*cb)(int), int arg) {
  for (int i = 0; i < NMARCAS; i++)
    if (!u_marca[i]) {
      u_marca[i] = marca;
      u_marca_cb[i] = cb;
      u_marca_arg[i] = arg;
      return;
    }
}

static bool rx_hay(void) { return rx_r != rx_w; }

// Recalcula el nivel de la IRQ y lo pasa al GIC si cambió. Con u_mu tomado.
static void u_irq(void) {
  uint8_t n = ((u.ier & 0x01) && rx_hay()) || ((u.ier & 0x02) && u.thr_pend);
  if (n != u.nivel) {
    u.nivel = n;
    hv_gic_set_spi(u_intid, n);
  }
}

static void u_out(uint8_t c) {
  if (g_uart_tiempos && u_inicio_linea) printf("[%9.3f] ", ms_desde_inicio());
  u_inicio_linea = (c == '\n');
  putchar(c);
  if (c == '\n') fflush(stdout);
  if (u_nhist == sizeof u_hist) {
    memmove(u_hist, u_hist + 1, sizeof u_hist - 1);
    u_nhist--;
  }
  u_hist[u_nhist++] = (char)c;
  for (int i = 0; i < NMARCAS; i++) {
    if (!u_marca[i]) continue;
    size_t l = strlen(u_marca[i]);
    if (l <= u_nhist && memcmp(u_hist + u_nhist - l, u_marca[i], l) == 0) u_marca_cb[i](u_marca_arg[i]);
  }
}

bool uart_mmio(uint64_t off, bool escribir, uint64_t *val) {
  pthread_mutex_lock(&u_mu);
  bool dlab = u.lcr & 0x80;
  uint8_t v = (uint8_t)*val;
  if (escribir) {
    switch (off) {
      case 0:
        if (dlab) u.dll = v;
        else {
          u_out(v);
          u.thr_pend = 1;
        }
        break;
      case 1:
        if (dlab) u.dlm = v;
        else {
          // Habilitar THRI con el transmisor vacío dispara la interrupción
          // en seguida (el driver 8250 lo comprueba al arrancar).
          if ((v & 0x02) && !(u.ier & 0x02)) u.thr_pend = 1;
          u.ier = v & 0x0f;
        }
        break;
      case 2: u.fcr = v; break;
      case 3: u.lcr = v; break;
      case 4: u.mcr = v; break;
      case 7: u.scr = v; break;
    }
  } else {
    uint64_t r = 0;
    switch (off) {
      case 0:
        if (dlab) r = u.dll;
        else if (rx_hay()) {
          r = (uint8_t)rx[rx_r];
          rx_r = (rx_r + 1) % sizeof rx;
        }
        break;
      case 1: r = dlab ? u.dlm : u.ier; break;
      case 2:
        r = (u.fcr & 1) ? 0xc0 : 0;
        if ((u.ier & 0x01) && rx_hay()) r |= 0x04;
        else if ((u.ier & 0x02) && u.thr_pend) {
          r |= 0x02;
          u.thr_pend = 0;  // leer IIR con THRI la reconoce
        } else r |= 0x01;
        break;
      case 3: r = u.lcr; break;
      case 4: r = u.mcr; break;
      case 5: r = 0x60 | (rx_hay() ? 1 : 0); break;  // THRE|TEMT|DR
      case 6: r = (u.mcr & 0x10) ? ((u.mcr & 0x0f) << 4) : 0xb0; break;
      case 7: r = u.scr; break;
    }
    *val = r;
  }
  u_irq();
  pthread_mutex_unlock(&u_mu);
  return true;
}

void uart_rx_push(const char *s, size_t n) {
  pthread_mutex_lock(&u_mu);
  for (size_t i = 0; i < n; i++) {
    size_t sig = (rx_w + 1) % sizeof rx;
    if (sig == rx_r) break;
    rx[rx_w] = s[i];
    rx_w = sig;
  }
  u_irq();
  pthread_mutex_unlock(&u_mu);
  vcpu_kick_all();
}

static void *stdin_hilo(void *a) {
  (void)a;
  char b[256];
  ssize_t n;
  while ((n = read(0, b, sizeof b)) > 0) uart_rx_push(b, (size_t)n);
  return NULL;
}

void uart_start_stdin(void) {
  pthread_t t;
  pthread_create(&t, NULL, stdin_hilo, NULL);
  pthread_detach(t);
}

void uart_get(struct uart_state *s) {
  pthread_mutex_lock(&u_mu);
  *s = u;
  pthread_mutex_unlock(&u_mu);
}

void uart_set(const struct uart_state *s) {
  pthread_mutex_lock(&u_mu);
  u = *s;
  // El nivel guardado ya está dentro del estado del GIC restaurado.
  pthread_mutex_unlock(&u_mu);
}

// ===================== virtio-mmio block =====================

#define VIRTQ_DESC_F_NEXT 1
#define VIRTQ_DESC_F_WRITE 2
#define QMAX 256

struct vdesc {
  uint64_t addr;
  uint32_t len;
  uint16_t flags, next;
};

struct vblk {
  int fd;
  bool ro;
  uint64_t sectores;
  uint32_t intid;
  struct vblk_state s;
  pthread_mutex_t mu;
};

static struct vblk blks[VIRTIO_MAX];
static int nblks;

int vblk_count(void) { return nblks; }

void vblk_init(int idx, const char *ruta, bool ro, uint32_t intid) {
  struct vblk *b = &blks[idx];
  b->fd = open(ruta, ro ? O_RDONLY : O_RDWR);
  if (b->fd < 0) morir("no se puede abrir el disco %s", ruta);
  struct stat st;
  fstat(b->fd, &st);
  b->sectores = (uint64_t)st.st_size / 512;
  b->ro = ro;
  b->intid = intid;
  memset(&b->s, 0, sizeof b->s);
  pthread_mutex_init(&b->mu, NULL);
  if (idx + 1 > nblks) nblks = idx + 1;
}

static uint64_t dev_features(struct vblk *b, uint32_t sel) {
  // VIRTIO_BLK_F_FLUSH (9), VIRTIO_BLK_F_RO (5); sel 1: VIRTIO_F_VERSION_1.
  if (sel == 0) return (1u << 9) | (b->ro ? (1u << 5) : 0);
  if (sel == 1) return 1;
  return 0;
}

static void b_irq(struct vblk *b) {
  uint8_t n = b->s.isr != 0;
  if (n != b->s.nivel) {
    b->s.nivel = n;
    hv_gic_set_spi(b->intid, n);
  }
}

// Atiende todas las peticiones pendientes de la cola 0.
static void b_procesar(struct vblk *b) {
  struct vblk_queue *q = &b->s.q;
  if (!q->ready || q->num == 0) return;
  uint16_t *avail = gpa_ptr(q->avail, 4 + 2 * q->num);
  uint8_t *used = gpa_ptr(q->used, 4 + 8 * q->num);
  struct vdesc *desc = gpa_ptr(q->desc, 16 * q->num);
  if (!avail || !used || !desc) return;
  bool hecho = false;
  for (;;) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    uint16_t aidx = *(volatile uint16_t *)&avail[1];
    if (q->last_avail == aidx) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;

    // Recorre la cadena: cabecera, datos, estado (1 byte, el último).
    struct vdesc ch[QMAX + 2];
    int n = 0;
    for (uint16_t i = head;; i = desc[i].next) {
      if (i >= q->num || n >= QMAX) break;
      ch[n++] = desc[i];
      if (!(desc[i].flags & VIRTQ_DESC_F_NEXT)) break;
    }
    uint32_t escrito = 0;
    uint8_t estado = 0;
    struct {
      uint32_t type, reserved;
      uint64_t sector;
    } hdr;
    void *hp = n >= 2 ? gpa_ptr(ch[0].addr, sizeof hdr) : NULL;
    uint8_t *sp = n >= 2 ? gpa_ptr(ch[n - 1].addr, 1) : NULL;
    if (!hp || !sp || ch[0].len < sizeof hdr) {
      estado = 1;
    } else {
      memcpy(&hdr, hp, sizeof hdr);
      uint64_t off = hdr.sector * 512;
      for (int k = 1; k < n - 1 && estado == 0; k++) {
        void *d = gpa_ptr(ch[k].addr, ch[k].len);
        if (!d) {
          estado = 1;
          break;
        }
        ssize_t r = 0;
        switch (hdr.type) {
          case 0:  // IN
            r = pread(b->fd, d, ch[k].len, (off_t)off);
            if (r >= 0 && (uint32_t)r < ch[k].len) memset((uint8_t *)d + r, 0, ch[k].len - r);
            if (r >= 0) r = ch[k].len, escrito += ch[k].len;
            break;
          case 1:  // OUT
            if (b->ro) r = -1;
            else r = pwrite(b->fd, d, ch[k].len, (off_t)off);
            break;
          case 8: {  // GET_ID
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
      if (hdr.type == 4 && estado == 0) {  // FLUSH
        if (fsync(b->fd) != 0) estado = 1;
      }
    }
    if (sp) *sp = estado;
    escrito += 1;
    uint16_t uidx = *(uint16_t *)(used + 2);
    uint32_t *e = (uint32_t *)(used + 4 + 8 * (uidx % q->num));
    e[0] = head;
    e[1] = escrito;
    __atomic_thread_fence(__ATOMIC_RELEASE);
    *(volatile uint16_t *)(used + 2) = uidx + 1;
    hecho = true;
  }
  if (hecho) {
    b->s.isr |= 1;
    b_irq(b);
  }
}

bool vblk_mmio(int idx, uint64_t off, bool escribir, uint64_t *val, int size) {
  if (idx >= nblks) {
    if (!escribir) *val = 0;
    return true;
  }
  struct vblk *b = &blks[idx];
  struct vblk_state *s = &b->s;
  pthread_mutex_lock(&b->mu);
  if (off >= 0x100) {  // espacio de configuración: capacity (u64) al principio
    if (!escribir) {
      uint8_t cfg[64] = {0};
      memcpy(cfg, &b->sectores, 8);
      uint32_t segmax = 128;
      memcpy(cfg + 12, &segmax, 4);
      uint64_t o = off - 0x100, r = 0;
      if (o + size <= sizeof cfg) memcpy(&r, cfg + o, size);
      *val = r;
    }
    pthread_mutex_unlock(&b->mu);
    return true;
  }
  uint32_t v = (uint32_t)*val;
  if (escribir) {
    switch (off) {
      case 0x014: s->dev_feat_sel = v; break;
      case 0x020:
        if (s->drv_feat_sel < 2) {
          s->drv_features &= ~(0xffffffffULL << (32 * s->drv_feat_sel));
          s->drv_features |= (uint64_t)v << (32 * s->drv_feat_sel);
        }
        break;
      case 0x024: s->drv_feat_sel = v; break;
      case 0x030: s->queue_sel = v; break;
      case 0x038: if (s->queue_sel == 0 && v <= QMAX) s->q.num = v; break;
      case 0x044: if (s->queue_sel == 0) s->q.ready = v & 1; break;
      case 0x050: if (v == 0) b_procesar(b); break;
      case 0x064:
        s->isr &= ~v;
        b_irq(b);
        break;
      case 0x070:
        if (v == 0) {  // reset
          uint8_t nivel = s->nivel;
          memset(s, 0, sizeof *s);
          s->nivel = nivel;
          b_irq(b);
        } else s->status = v;
        break;
      case 0x080: if (s->queue_sel == 0) s->q.desc = (s->q.desc & ~0xffffffffULL) | v; break;
      case 0x084: if (s->queue_sel == 0) s->q.desc = (s->q.desc & 0xffffffffULL) | ((uint64_t)v << 32); break;
      case 0x090: if (s->queue_sel == 0) s->q.avail = (s->q.avail & ~0xffffffffULL) | v; break;
      case 0x094: if (s->queue_sel == 0) s->q.avail = (s->q.avail & 0xffffffffULL) | ((uint64_t)v << 32); break;
      case 0x0a0: if (s->queue_sel == 0) s->q.used = (s->q.used & ~0xffffffffULL) | v; break;
      case 0x0a4: if (s->queue_sel == 0) s->q.used = (s->q.used & 0xffffffffULL) | ((uint64_t)v << 32); break;
    }
  } else {
    uint64_t r = 0;
    switch (off) {
      case 0x000: r = 0x74726976; break;  // "virt"
      case 0x004: r = 2; break;
      case 0x008: r = 2; break;  // block
      case 0x00c: r = 0x4b4c4e47; break;
      case 0x010: r = dev_features(b, s->dev_feat_sel); break;
      case 0x034: r = s->queue_sel == 0 ? QMAX : 0; break;
      case 0x044: r = s->queue_sel == 0 ? s->q.ready : 0; break;
      case 0x060: r = s->isr; break;
      case 0x070: r = s->status; break;
      case 0x0fc: r = 0; break;
    }
    *val = r;
  }
  pthread_mutex_unlock(&b->mu);
  return true;
}

void vblk_get(int idx, struct vblk_state *s) {
  pthread_mutex_lock(&blks[idx].mu);
  *s = blks[idx].s;
  pthread_mutex_unlock(&blks[idx].mu);
}

void vblk_set(int idx, const struct vblk_state *s) {
  pthread_mutex_lock(&blks[idx].mu);
  blks[idx].s = *s;
  pthread_mutex_unlock(&blks[idx].mu);
}
