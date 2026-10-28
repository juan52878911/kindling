//go:build darwin && arm64

// virtio-mmio net (colas split rx=0 y tx=1). El otro extremo es un socket unix
// SOCK_DGRAM con una trama Ethernet por datagrama: el contrato de
// VZFileHandleNetworkDeviceAttachment, así que la red de espacio de usuario de
// kling-vz (vz/internal/vnet) sirve tal cual al otro lado.
#include <errno.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#include "khv.h"

#define HDR 12  // virtio_net_hdr_v1 (con num_buffers)

static struct vnet_state s;
static int fd = -1;
static uint32_t intid;
static uint8_t mac[6];
// MTU que se anuncia al invitado (VIRTIO_NET_F_MTU), la misma con la que se
// montó la pila de usuario (vnet.Config.MTU); 0 = no se anuncia (1500).
static uint16_t mtu;
// mu protege el estado y la RAM que toca el hilo de recepción: con g_pausa
// puesto (bajo mu, en vnet_quieto) el hilo no escribe en el invitado.
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;  // hay búferes de rx, o se reanudó

static void irq(void) { vmmio_irq(&s.m, intid); }

// TX: cada cadena es cabecera + trama; se manda la trama como un datagrama.
// Desde el hilo del vCPU (notify de la cola 1), con mu.
static void tx(void) {
  struct vq *q = &s.q[1];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;
  bool hecho = false;
  static uint8_t buf[65536 + HDR];
  for (uint32_t vueltas = 0; vueltas < q->num; vueltas++) {  // ver b_procesar en virtio.c
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1]) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    size_t n = vq_leer(q, desc, head, buf, sizeof buf);  // 0 si la cadena no vale o no cabe
    if (fd >= 0 && n > HDR) send(fd, buf + HDR, n - HDR, 0);  // ENOBUFS: se pierde, TCP reintenta
    vq_push(q, used, head, 0);
    hecho = true;
  }
  if (hecho) {
    s.m.isr |= 1;
    irq();
  }
}

// Mete la trama en el siguiente búfer de rx; false si no hay (o no hay driver).
// Con mu.
static bool rx_entregar(const uint8_t *trama, size_t n, bool *sin_buf) {
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  *sin_buf = false;
  if (!vq_ptrs(q, &avail, &used, &desc)) return false;
  __atomic_thread_fence(__ATOMIC_ACQUIRE);
  if (q->last_avail == *(volatile uint16_t *)&avail[1]) {
    *sin_buf = true;
    return false;
  }
  uint16_t head = avail[2 + q->last_avail % q->num];
  q->last_avail++;
  static uint8_t pkt[HDR + 65536];
  if (n > sizeof pkt - HDR) n = sizeof pkt - HDR;
  memset(pkt, 0, HDR);
  pkt[10] = 1;  // num_buffers
  memcpy(pkt + HDR, trama, n);
  // Lo que no quepa en la cadena se pierde (trama truncada), como antes.
  uint32_t off = vq_escribir(q, desc, head, pkt, (uint32_t)(HDR + n));
  vq_push(q, used, head, off);
  s.m.isr |= 1;
  irq();
  return true;
}

// RX: lee tramas del socket y las mete en los búferes del invitado.
static void *rx_hilo(void *a) {
  (void)a;
  khv_qos_dispositivo();
  static uint8_t trama[65536];
  for (;;) {
    ssize_t n = recv(fd, trama, sizeof trama, 0);
    if (n < 0) {
      if (errno == EINTR) continue;
      return NULL;
    }
    khv_qos_impulso();  // el host le manda trabajo al invitado (vm.c, QoS)
    pthread_mutex_lock(&mu);
    for (int intentos = 0;; intentos++) {
      // Con la VM en pausa (restaurando o haciendo snapshot) no se toca la
      // RAM: la trama espera a que corra.
      while (g_pausa) pthread_cond_wait(&cv, &mu);
      bool sin_buf;
      if (rx_entregar(trama, (size_t)n, &sin_buf) || !sin_buf || intentos >= 20) break;
      // Sin búferes libres: esperar a que el invitado reponga (notify de la
      // cola 0), como mucho 1 s en total; luego la trama se pierde.
      struct timespec ts;
      clock_gettime(CLOCK_REALTIME, &ts);
      ts.tv_nsec += 50000000;
      if (ts.tv_nsec >= 1000000000) ts.tv_sec++, ts.tv_nsec -= 1000000000;
      pthread_cond_timedwait(&cv, &mu, &ts);
    }
    pthread_mutex_unlock(&mu);
  }
}

void vnet_init(int net_fd, const uint8_t m[6], int mtu_enlace, uint32_t id) {
  fd = net_fd;
  memcpy(mac, m, 6);
  // Las tramas caben en los búferes de tx y rx (64 KiB) con cualquier MTU que
  // admite el spec (hasta 65535).
  mtu = mtu_enlace > 1500 && mtu_enlace <= 65535 ? (uint16_t)mtu_enlace : 0;
  intid = id;
  if (fd >= 0) {
    pthread_t t;
    pthread_create(&t, NULL, rx_hilo, NULL);
    pthread_detach(t);
  }
}

bool vnet_mmio(uint64_t off, bool escribir, uint64_t *val, int size) {
  pthread_mutex_lock(&mu);
  if (off >= 0x100) {  // configuración: mac[6], status u16, max_virtqueue_pairs u16, mtu u16
    if (!escribir) {
      uint8_t cfg[16] = {0};
      memcpy(cfg, mac, 6);
      cfg[6] = 1;  // VIRTIO_NET_S_LINK_UP
      cfg[8] = 1;  // un par de colas
      memcpy(cfg + 10, &mtu, 2);
      uint64_t o = off - 0x100, r = 0;
      if (o + (uint64_t)size <= sizeof cfg) memcpy(&r, cfg + o, (size_t)size);
      *val = r;
    }
    pthread_mutex_unlock(&mu);
    return true;
  }
  uint32_t cola = 0;
  // VIRTIO_NET_F_MAC (5), VIRTIO_NET_F_STATUS (16) y, con una MTU mayor que
  // 1500, VIRTIO_NET_F_MTU (3): el virtio_net de Linux la adopta solo, como con
  // el adjunto de vz. Sin VIRTIO_NET_F_MRG_RXBUF el invitado reserva búferes
  // del tamaño de la MTU (ver spec.DefaultMTU).
  uint64_t feat = (1u << 5) | (1u << 16) | (mtu ? (1u << 3) : 0);
  switch (vmmio_acceso(&s.m, s.q, 2, 1, feat, off, escribir, val, &cola)) {
    case VM_NOTIFY:
      if (cola == 1) tx();
      else if (cola == 0) pthread_cond_broadcast(&cv);
      break;
    case VM_ISR: irq(); break;
    case VM_RESET: {
      uint8_t nivel = s.m.nivel;
      memset(&s, 0, sizeof s);
      s.m.nivel = nivel;
      irq();
      break;
    }
  }
  pthread_mutex_unlock(&mu);
  return true;
}

// vnet_quieto: con g_pausa ya puesto, tomar mu garantiza que el hilo de rx no
// está a mitad de escribir en el invitado (lo hace con mu tomado).
void vnet_quieto(void) {
  pthread_mutex_lock(&mu);
  pthread_mutex_unlock(&mu);
}

void vnet_despertar(void) {
  pthread_mutex_lock(&mu);
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
}

void vnet_get(struct vnet_state *o) {
  pthread_mutex_lock(&mu);
  *o = s;
  pthread_mutex_unlock(&mu);
}

void vnet_reafirmar(void) {
  pthread_mutex_lock(&mu);
  hv_gic_set_spi(intid, s.m.nivel);
  pthread_mutex_unlock(&mu);
}

void vnet_set(const struct vnet_state *i) {
  pthread_mutex_lock(&mu);
  s = *i;
  pthread_mutex_unlock(&mu);
}
