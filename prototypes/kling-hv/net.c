// virtio-mmio net (versión 2, colas split rx=0 y tx=1). El otro extremo es un
// socket unix SOCK_DGRAM con una trama Ethernet por datagrama: el mismo
// contrato que VZFileHandleNetworkDeviceAttachment, así que la red de espacio
// de usuario de kling-vz (vz/internal/vnet) sirve tal cual al otro lado.
#include <errno.h>
#include <poll.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#include "khv.h"

#define QMAX 256
#define HDR 12  // virtio_net_hdr_v1 (con num_buffers)
#define F_NEXT 1

struct vdesc {
  uint64_t addr;
  uint32_t len;
  uint16_t flags, next;
};

static struct vnet_state s;
static int fd = -1;
static uint32_t intid;
static uint8_t mac[6];
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t hay_rx = PTHREAD_COND_INITIALIZER;
static bool activo;

static void irq(void) {
  uint8_t n = s.isr != 0;
  if (n != s.nivel) {
    s.nivel = n;
    hv_gic_set_spi(intid, n);
  }
}

static bool q_ptrs(struct vblk_queue *q, uint16_t **avail, uint8_t **used, struct vdesc **desc) {
  if (!q->ready || q->num == 0) return false;
  *avail = gpa_ptr(q->avail, 4 + 2 * q->num);
  *used = gpa_ptr(q->used, 4 + 8 * q->num);
  *desc = gpa_ptr(q->desc, 16 * q->num);
  return *avail && *used && *desc;
}

static void q_push(struct vblk_queue *q, uint8_t *used, uint16_t head, uint32_t len) {
  uint16_t uidx = *(uint16_t *)(used + 2);
  uint32_t *e = (uint32_t *)(used + 4 + 8 * (uidx % q->num));
  e[0] = head;
  e[1] = len;
  __atomic_thread_fence(__ATOMIC_RELEASE);
  *(volatile uint16_t *)(used + 2) = uidx + 1;
}

// TX: cada cadena es cabecera + trama; se manda la trama como un datagrama.
static void tx(void) {
  struct vblk_queue *q = &s.q[1];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!q_ptrs(q, &avail, &used, &desc)) return;
  bool hecho = false;
  static uint8_t buf[65536 + HDR];
  for (;;) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1]) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    size_t n = 0;
    for (uint16_t i = head, k = 0; k < q->num; i = desc[i].next, k++) {
      if (i >= q->num) break;
      void *d = gpa_ptr(desc[i].addr, desc[i].len);
      if (d && n + desc[i].len <= sizeof buf) {
        memcpy(buf + n, d, desc[i].len);
        n += desc[i].len;
      }
      if (!(desc[i].flags & F_NEXT)) break;
    }
    if (fd >= 0 && n > HDR) send(fd, buf + HDR, n - HDR, 0);  // ENOBUFS: se pierde, TCP reintenta
    q_push(q, used, head, 0);
    hecho = true;
  }
  if (hecho) {
    s.isr |= 1;
    irq();
  }
}

// RX: hilo que lee tramas del socket y las mete en los buffers del invitado.
static void *rx_hilo(void *a) {
  (void)a;
  static uint8_t trama[65536];
  for (;;) {
    ssize_t n = recv(fd, trama, sizeof trama, 0);
    if (n < 0) {
      if (errno == EINTR) continue;
      return NULL;
    }
    // Con la VM en pausa (restaurando o haciendo snapshot) no se toca la RAM:
    // la trama espera a que corra.
    while (g_pausa) usleep(100);
    pthread_mutex_lock(&mu);
    for (;;) {
      struct vblk_queue *q = &s.q[0];
      uint16_t *avail;
      uint8_t *used;
      struct vdesc *desc;
      if (!activo || !q_ptrs(q, &avail, &used, &desc)) break;  // sin driver: se descarta
      __atomic_thread_fence(__ATOMIC_ACQUIRE);
      if (q->last_avail == *(volatile uint16_t *)&avail[1]) {
        // Sin buffers libres: esperar a que el invitado reponga (notify de la cola 0).
        struct timespec ts;
        clock_gettime(CLOCK_REALTIME, &ts);
        ts.tv_nsec += 50000000;
        if (ts.tv_nsec >= 1000000000) ts.tv_sec++, ts.tv_nsec -= 1000000000;
        if (pthread_cond_timedwait(&hay_rx, &mu, &ts) == ETIMEDOUT) break;
        continue;
      }
      uint16_t head = avail[2 + q->last_avail % q->num];
      q->last_avail++;
      // Cabecera a cero con num_buffers = 1, y luego la trama.
      uint8_t hdr[HDR] = {0};
      hdr[10] = 1;
      size_t off = 0, total = HDR + (size_t)n;
      for (uint16_t i = head, k = 0; k < q->num && off < total; i = desc[i].next, k++) {
        if (i >= q->num) break;
        uint8_t *d = gpa_ptr(desc[i].addr, desc[i].len);
        if (!d) break;
        for (uint32_t j = 0; j < desc[i].len && off < total; j++, off++)
          d[j] = off < HDR ? hdr[off] : trama[off - HDR];
        if (!(desc[i].flags & F_NEXT)) break;
      }
      q_push(q, used, head, (uint32_t)off);
      s.isr |= 1;
      irq();
      break;
    }
    pthread_mutex_unlock(&mu);
    vcpu_kick_all();
  }
}

void vnet_init(int net_fd, const uint8_t m[6], uint32_t id) {
  fd = net_fd;
  memcpy(mac, m, 6);
  intid = id;
  activo = true;
  if (fd >= 0) {
    pthread_t t;
    pthread_create(&t, NULL, rx_hilo, NULL);
    pthread_detach(t);
  }
}

bool vnet_mmio(uint64_t off, bool escribir, uint64_t *val, int size) {
  pthread_mutex_lock(&mu);
  if (off >= 0x100) {  // configuración: mac[6], status u16
    if (!escribir) {
      uint8_t cfg[16] = {0};
      memcpy(cfg, mac, 6);
      cfg[6] = 1;  // VIRTIO_NET_S_LINK_UP
      uint64_t o = off - 0x100, r = 0;
      if (o + size <= sizeof cfg) memcpy(&r, cfg + o, size);
      *val = r;
    }
    pthread_mutex_unlock(&mu);
    return true;
  }
  uint32_t v = (uint32_t)*val;
  struct vblk_queue *q = s.queue_sel < 2 ? &s.q[s.queue_sel] : NULL;
  if (escribir) {
    switch (off) {
      case 0x014: s.dev_feat_sel = v; break;
      case 0x020:
        if (s.drv_feat_sel < 2) {
          s.drv_features &= ~(0xffffffffULL << (32 * s.drv_feat_sel));
          s.drv_features |= (uint64_t)v << (32 * s.drv_feat_sel);
        }
        break;
      case 0x024: s.drv_feat_sel = v; break;
      case 0x030: s.queue_sel = v; break;
      case 0x038: if (q && v <= QMAX) q->num = v; break;
      case 0x044: if (q) q->ready = v & 1; break;
      case 0x050:
        if (v == 1) tx();
        else if (v == 0) pthread_cond_broadcast(&hay_rx);
        break;
      case 0x064:
        s.isr &= ~v;
        irq();
        break;
      case 0x070:
        if (v == 0) {
          uint8_t nivel = s.nivel;
          memset(&s, 0, sizeof s);
          s.nivel = nivel;
          irq();
        } else s.status = v;
        break;
      case 0x080: if (q) q->desc = (q->desc & ~0xffffffffULL) | v; break;
      case 0x084: if (q) q->desc = (q->desc & 0xffffffffULL) | ((uint64_t)v << 32); break;
      case 0x090: if (q) q->avail = (q->avail & ~0xffffffffULL) | v; break;
      case 0x094: if (q) q->avail = (q->avail & 0xffffffffULL) | ((uint64_t)v << 32); break;
      case 0x0a0: if (q) q->used = (q->used & ~0xffffffffULL) | v; break;
      case 0x0a4: if (q) q->used = (q->used & 0xffffffffULL) | ((uint64_t)v << 32); break;
    }
  } else {
    uint64_t r = 0;
    switch (off) {
      case 0x000: r = 0x74726976; break;
      case 0x004: r = 2; break;
      case 0x008: r = 1; break;  // net
      case 0x00c: r = 0x4b4c4e47; break;
      case 0x010: r = s.dev_feat_sel == 0 ? (1u << 5) | (1u << 16) : s.dev_feat_sel == 1 ? 1 : 0; break;  // MAC, STATUS; VERSION_1
      case 0x034: r = q ? QMAX : 0; break;
      case 0x044: r = q ? q->ready : 0; break;
      case 0x060: r = s.isr; break;
      case 0x070: r = s.status; break;
    }
    *val = r;
  }
  pthread_mutex_unlock(&mu);
  return true;
}

void vnet_get(struct vnet_state *o) {
  pthread_mutex_lock(&mu);
  *o = s;
  pthread_mutex_unlock(&mu);
}

void vnet_reafirmar(void) {
  pthread_mutex_lock(&mu);
  hv_gic_set_spi(intid, s.nivel);
  pthread_mutex_unlock(&mu);
}

void vnet_set(const struct vnet_state *i) {
  pthread_mutex_lock(&mu);
  s = *i;
  pthread_mutex_unlock(&mu);
}
