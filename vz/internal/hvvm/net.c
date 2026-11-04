//go:build darwin && arm64

// virtio-mmio net (colas split rx=0 y tx=1). El otro extremo es un socket unix
// SOCK_DGRAM con una trama Ethernet por datagrama: el contrato de
// VZFileHandleNetworkDeviceAttachment, así que la red de espacio de usuario de
// kling-vz (vz/internal/vnet) sirve tal cual al otro lado.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#include "khv.h"
#include "vq_iov.h"

#define HDR 12  // virtio_net_hdr_v1 (con num_buffers)
#define TRAMA_MAX 65536

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
// rx_en_curso: el hilo de recepción está dentro de recvmsg, sin mu, escribiendo
// en un búfer de rx del invitado que ya tomó (rx_una). Mientras, ni la pausa
// (vnet_quieto), ni un estado nuevo (vnet_set), ni el driver tocando la
// configuración o reiniciando el dispositivo siguen: esperan en cv_rx. Así
// el recvmsg no tiene mu tomado y el vCPU puede mandar (tx) a la vez.
static bool rx_en_curso;
static pthread_cond_t cv_rx = PTHREAD_COND_INITIALIZER;
// El hilo de recepción y cómo pararlo (vnet_parar): rx_desp despierta su poll.
static pthread_t rx_t;
static bool rx_vivo;
static bool rx_parar;
static int rx_desp[2] = {-1, -1};

static void irq(void) { vmmio_irq(&s.m, intid); }

// tx_mandar manda la trama de la cadena head como un datagrama, sin copiarla:
// sendmsg con los búferes de lectura del invitado (sin la cabecera). Una
// cadena que no vale, sin trama o mayor que TRAMA_MAX se descarta. Nunca
// bloquea (MSG_DONTWAIT): con el otro extremo lleno (ENOBUFS, EAGAIN) la
// trama se pierde y TCP reintenta; esperar dejaría el vCPU dentro del MMIO.
static void tx_mandar(const struct vq *q, const struct vdesc *desc, uint16_t head) {
  static struct iovec iov[QMAX], datos[QMAX];
  uint64_t total;
  int n = vq_iov(q, desc, head, false, iov, QMAX, &total);
  if (fd < 0 || n <= 0 || total <= HDR || total > TRAMA_MAX + HDR) return;
  struct msghdr m = {.msg_iov = datos, .msg_iovlen = iov_tramo(iov, n, HDR, total - HDR, datos)};
  (void)!sendmsg(fd, &m, MSG_DONTWAIT);
}

// TX: cada cadena es cabecera + trama; se manda la trama como un datagrama.
// Desde el hilo del vCPU (notify de la cola 1), con mu.
static void tx(void) {
  struct vq *q = &s.q[1];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;
  bool hecho = false;
  uint16_t viejo = *(volatile uint16_t *)(used + 2);
  for (uint32_t vueltas = 0;; vueltas++) {  // ver b_procesar en virtio.c
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1]) {
      if (vueltas < 2 * q->num && vq_pedir_aviso(&s.m, q, avail, used)) continue;
      break;
    }
    if (vueltas >= 2 * q->num) {
      // Lo que quede, en el siguiente aviso (que se pide ya).
      vq_pedir_aviso_ya(&s.m, q, avail, used);
      break;
    }
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    tx_mandar(q, desc, head);
    vq_push(q, used, head, 0);
    hecho = true;
  }
  // Con EVENT_IDX el driver de Linux pide la interrupción de tx solo cuando
  // le hace falta liberar (napi_tx), no por cada trama.
  if (hecho && vq_interrumpir(&s.m, q, avail, viejo, *(volatile uint16_t *)(used + 2))) {
    __atomic_fetch_add(&khv_avisos[KHV_N_NET_IRQ], 1, __ATOMIC_RELAXED);
    s.m.isr |= 1;
    irq();
  }
}

// rx_avisar interrumpe al invitado, si lo pide, por las tramas puestas desde
// que used valía viejo. Con mu.
static void rx_avisar(uint16_t viejo) {
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;
  // En NAPI el driver no quiere una interrupción por trama: la pide al acabar.
  if (vq_interrumpir(&s.m, q, avail, viejo, *(volatile uint16_t *)(used + 2))) {
    __atomic_fetch_add(&khv_avisos[KHV_N_NET_IRQ], 1, __ATOMIC_RELAXED);
    s.m.isr |= 1;
    irq();
  }
}

// used_idx es el índice de used ahora (0 sin driver).
static uint16_t used_idx(void) {
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  return vq_ptrs(q, &avail, &used, &desc) ? *(volatile uint16_t *)(used + 2) : 0;
}

enum { RX_PUESTA, RX_VACIO, RX_SIN_BUF, RX_ERROR };

// rx_una recibe la siguiente trama del socket directamente en el siguiente
// búfer de rx del invitado (recvmsg con sus búferes como iovec, detrás de la
// cabecera): una sola pasada por la cadena y ninguna copia intermedia. Con
// mu y la VM corriendo; lo suelta durante recvmsg (ver rx_en_curso).
// RX_VACIO: no hay trama (el búfer no se gasta); RX_SIN_BUF: no hay búfer, y
// la trama sigue en el socket; RX_ERROR: recv falló (en *err). Sin driver la trama se tira. Una cadena que no vale, o
// más corta que la trama, no recibe media trama (ni media cabecera): el
// búfer vuelve con 0 bytes y la trama se pierde entera, que es lo que un
// driver espera de un paquete que no le cabe.
static int rx_una(int *err) {
  static struct iovec iov[QMAX], datos[QMAX];
  static uint8_t tirar[TRAMA_MAX];
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  struct msghdr m = {0};
  struct iovec t = {.iov_base = tirar, .iov_len = sizeof tirar};
  if (!vq_ptrs(q, &avail, &used, &desc)) {
    m.msg_iov = &t;
    m.msg_iovlen = 1;
    ssize_t r = recvmsg(fd, &m, MSG_DONTWAIT);
    if (r >= 0) return RX_PUESTA;
    *err = errno;
    return errno == EAGAIN ? RX_VACIO : RX_ERROR;
  }
  __atomic_thread_fence(__ATOMIC_ACQUIRE);
  // Sin búferes: con EVENT_IDX se pide el aviso del siguiente (mientras los
  // haya, el driver repone sin avisar) y se vuelve a mirar.
  if (q->last_avail == *(volatile uint16_t *)&avail[1] && !vq_pedir_aviso(&s.m, q, avail, used)) return RX_SIN_BUF;
  uint16_t head = avail[2 + q->last_avail % q->num];
  uint64_t cap = 0;
  int n = vq_iov(q, desc, head, true, iov, QMAX, &cap);
  bool vale = n > 0 && cap > HDR;
  if (vale) {
    m.msg_iov = datos;
    m.msg_iovlen = iov_tramo(iov, n, HDR, cap - HDR, datos);
  } else {
    m.msg_iov = &t;  // se recibe para tirarla
    m.msg_iovlen = 1;
  }
  rx_en_curso = true;
  pthread_mutex_unlock(&mu);
  ssize_t r = recvmsg(fd, &m, MSG_DONTWAIT);
  int e = errno;
  pthread_mutex_lock(&mu);
  rx_en_curso = false;
  pthread_cond_broadcast(&cv_rx);
  if (r < 0) {
    *err = e;
    return e == EAGAIN ? RX_VACIO : RX_ERROR;
  }
  // La cola es la misma que antes de soltar mu: quien la cambia espera a
  // rx_en_curso.
  uint32_t len = 0;
  if (vale && !(m.msg_flags & MSG_TRUNC)) {
    uint8_t cab[HDR] = {0};
    cab[10] = 1;  // num_buffers
    iov_poner(iov, n, cab, HDR);
    len = HDR + (uint32_t)r;
  }
  q->last_avail++;
  vq_push(q, used, head, len);
  return RX_PUESTA;
}

// esperar_rx espera a que el hilo de recepción salga de recvmsg (ver
// rx_en_curso). Con mu.
static void esperar_rx(void) {
  while (rx_en_curso) pthread_cond_wait(&cv_rx, &mu);
}

// rx_errorf deja en stderr por qué termina el hilo de recepción: sin él la
// red del invitado se quedaba muda sin rastro. Antes vacía la consola, como
// los diagnósticos de vm.c, para no partirle una línea.
static void rx_errorf(const char *que, int e) {
  uart_flush();
  dprintf(2, "kling-hv: net: the receive thread stops (%s: %s); the guest gets no more frames\n", que, strerror(e));
}

// rx_vaciar mete en el invitado todas las tramas que haya en el socket, con
// una interrupción por tanda (la que pida el driver) y no una por trama. Sin
// búferes libres espera a que el invitado reponga (notify de la cola 0), como
// mucho 1 s por trama; luego la tira, como antes, para que un invitado que no
// repone no deje al otro extremo lleno para siempre. Con mu; false si recv
// falló (en *err) y el hilo debe terminar.
static bool rx_vaciar(int *err) {
  uint16_t viejo = used_idx();
  int intentos = 0;
  bool impulso = false;
  for (;;) {
    // Con la VM en pausa (restaurando o haciendo snapshot) no se toca la
    // RAM: la trama espera a que corra (o a vnet_parar).
    if (g_pausa && !rx_parar) {
      rx_avisar(viejo);
      while (g_pausa && !rx_parar) pthread_cond_wait(&cv, &mu);
      viejo = used_idx();
    }
    if (rx_parar) break;
    int r = rx_una(err);
    if (r == RX_PUESTA) {
      intentos = 0;
      if (!impulso) {
        khv_qos_impulso();  // el host le manda trabajo al invitado (vm.c, QoS)
        impulso = true;
      }
      // En una ráfaga larga el driver no espera al final para ver las
      // primeras: se le avisa cada 32 (si lo pide).
      if ((uint16_t)(used_idx() - viejo) >= 32) {
        rx_avisar(viejo);
        viejo = used_idx();
      }
      continue;
    }
    if (r == RX_ERROR) {
      if (*err == EINTR) continue;
      rx_avisar(viejo);
      return false;
    }
    if (r == RX_VACIO) break;
    // RX_SIN_BUF: si no hay trama esperando, no hay nada que esperar (el
    // aviso del siguiente búfer ya está pedido). Mirarlo sin mu.
    rx_avisar(viejo);
    pthread_mutex_unlock(&mu);
    struct pollfd pf = {.fd = fd, .events = POLLIN};
    bool hay = poll(&pf, 1, 0) > 0 && (pf.revents & POLLIN);
    pthread_mutex_lock(&mu);
    if (!hay) return true;
    if (intentos++ >= 20) {
      // Se tira la trama que espera, sin gastar búfer.
      static uint8_t tirar[TRAMA_MAX];
      if (recv(fd, tirar, sizeof tirar, MSG_DONTWAIT) < 0 && errno != EAGAIN && errno != EINTR) {
        *err = errno;
        return false;
      }
      intentos = 0;
    } else {
      struct timespec ts;
      clock_gettime(CLOCK_REALTIME, &ts);
      ts.tv_nsec += 50000000;
      if (ts.tv_nsec >= 1000000000) ts.tv_sec++, ts.tv_nsec -= 1000000000;
      pthread_cond_timedwait(&cv, &mu, &ts);
    }
    viejo = used_idx();
  }
  rx_avisar(viejo);
  return true;
}

// RX: lee tramas del socket y las mete en los búferes del invitado. Espera en
// poll sobre el socket y rx_desp, no en recv: vnet_parar lo despierta y lo
// espera antes de que Go cierre el fd. Cerrar el fd con el hilo dentro de
// recv no lo despierta en macOS, y su número podía reusarse con el hilo aún
// leyendo de él (otro socket del proceso).
static void *rx_hilo(void *a) {
  (void)a;
  khv_qos_dispositivo();
  for (;;) {
    struct pollfd pf[2] = {{.fd = fd, .events = POLLIN}, {.fd = rx_desp[0], .events = POLLIN}};
    int r = poll(pf, 2, -1);
    if (__atomic_load_n(&rx_parar, __ATOMIC_ACQUIRE)) return NULL;
    if (r < 0) {
      if (errno == EINTR) continue;
      rx_errorf("poll", errno);
      return NULL;
    }
    if (pf[1].revents & POLLIN) {
      char b[16];
      while (read(rx_desp[0], b, sizeof b) > 0) {
      }
    }
    if (!(pf[0].revents & (POLLIN | POLLHUP | POLLERR))) continue;
    // Un HUP sin nada que leer no se va: seguir sería girar en vacío.
    bool colgado = (pf[0].revents & (POLLHUP | POLLERR)) && !(pf[0].revents & POLLIN);
    int err = 0;
    pthread_mutex_lock(&mu);
    bool ok = rx_vaciar(&err);
    pthread_mutex_unlock(&mu);
    if (!ok) {
      rx_errorf("recv", err);
      return NULL;
    }
    if (colgado) {
      rx_errorf("recv", EPIPE);
      return NULL;
    }
  }
}

void vnet_init(int net_fd, const uint8_t m[6], int mtu_enlace, uint32_t id) {
  fd = net_fd;
  memcpy(mac, m, 6);
  // Las tramas caben en los búferes de tx y rx (64 KiB) con cualquier MTU que
  // admite el spec (hasta 65535).
  mtu = mtu_enlace > 1500 && mtu_enlace <= 65535 ? (uint16_t)mtu_enlace : 0;
  intid = id;
  rx_parar = false;
  if (fd < 0) return;
  if (rx_desp[0] < 0 && pipe(rx_desp) == 0) {
    for (int i = 0; i < 2; i++) {
      fcntl(rx_desp[i], F_SETFL, O_NONBLOCK);
      fcntl(rx_desp[i], F_SETFD, FD_CLOEXEC);
    }
  }
  if (rx_desp[0] < 0) {
    rx_errorf("pipe", errno);
    return;
  }
  rx_vivo = pthread_create(&rx_t, NULL, rx_hilo, NULL) == 0;
}

// vnet_parar saca al hilo de recepción y lo espera: después nadie usa el fd
// de la red y Go puede cerrarlo (vnet.Net.Close) sin que el número, reusado,
// le llegue a un recv de este hilo o a un send de tx. Idempotente; la llama
// machine.Stop tras khv_stop.
void vnet_parar(void) {
  pthread_mutex_lock(&mu);
  __atomic_store_n(&rx_parar, true, __ATOMIC_RELEASE);
  pthread_cond_broadcast(&cv);
  bool esperar = rx_vivo;
  rx_vivo = false;
  pthread_mutex_unlock(&mu);
  if (esperar) {
    char b = 1;
    (void)!write(rx_desp[1], &b, 1);
    pthread_join(rx_t, NULL);
  }
  pthread_mutex_lock(&mu);
  fd = -1;  // tx (vCPU, con mu) ya no manda nada
  pthread_mutex_unlock(&mu);
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
  // Una escritura que no es un notify (0x50) ni el ACK de una interrupción
  // (0x64) puede cambiar las colas o reiniciar el dispositivo: no con una
  // trama entrando en un búfer de rx (ver rx_en_curso).
  if (escribir && off != 0x50 && off != 0x64) esperar_rx();
  uint32_t cola = 0;
  // VIRTIO_NET_F_MAC (5), VIRTIO_NET_F_STATUS (16) y, con una MTU mayor que
  // 1500, VIRTIO_NET_F_MTU (3): el virtio_net de Linux la adopta solo, como con
  // el adjunto de vz. Sin VIRTIO_NET_F_MRG_RXBUF el invitado reserva búferes
  // del tamaño de la MTU (ver spec.DefaultMTU).
  // VIRTIO_RING_F_EVENT_IDX (29): avisos e interrupciones solo cuando hacen
  // falta (ver vq_interrumpir y vq_pedir_aviso en virtio.c).
  uint64_t feat = (1u << 5) | (1u << 16) | khv_event_idx | (mtu ? (1u << 3) : 0);
  switch (vmmio_acceso(&s.m, s.q, 2, 1, feat, off, escribir, val, &cola)) {
    case VM_NOTIFY:
      if (cola == 1) {
        __atomic_fetch_add(&khv_avisos[KHV_N_NET_TX], 1, __ATOMIC_RELAXED);
        tx();
      } else if (cola == 0) {
        __atomic_fetch_add(&khv_avisos[KHV_N_NET_RX], 1, __ATOMIC_RELAXED);
        pthread_cond_broadcast(&cv);
      }
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

// vnet_quieto: con g_pausa ya puesto, tomar mu y esperar a rx_en_curso
// garantiza que el hilo de rx no está a mitad de escribir en el invitado (lo
// hace con mu tomado, o dentro de recvmsg con rx_en_curso).
void vnet_quieto(void) {
  pthread_mutex_lock(&mu);
  esperar_rx();
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
  esperar_rx();
  s = *i;
  pthread_mutex_unlock(&mu);
}
