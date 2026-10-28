//go:build darwin && arm64

// virtio-vsock (virtio-mmio, id 19; colas rx=0, tx=1, eventos=2): el canal con
// el agente del invitado, el mismo contrato que kling-vz (docs/backend-vz.md).
// Solo conexiones que abre el host (CID 2) hacia un puerto del invitado (CID
// 3); una que pida el invitado se rechaza con RST.
//
// Cada conexión es un socketpair: un extremo lo usa Go como net.Conn y el otro
// lo bombea un hilo propio (vs_hilo) hacia los búferes de rx del invitado. Lo
// que manda el invitado (tx) se escribe en el socket desde el hilo del vCPU,
// sin bloquear: lo que no cabe se guarda hasta VS_BUF bytes, que es el crédito
// que se le anuncia, así que un invitado que respete el control de flujo nunca
// lo desborda (y uno que no, se lleva un RST).
//
// Las conexiones no van en el snapshot: al restaurar se manda al invitado
// VIRTIO_VSOCK_EVENT_TRANSPORT_RESET, que cierra las que creía vivas; los
// listeners del invitado siguen, y cada DialVsock abre una nueva.
#include <errno.h>
#include <fcntl.h>
#include <mach/mach_time.h>
#include <poll.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#include "khv.h"

#define VS_HOST_CID 2
#define VS_GUEST_CID 3
#define VS_MAXCONN 64
#define VS_BUF (256u * 1024)    // crédito que se anuncia al invitado por conexión
#define VS_PAYLOAD (64u * 1024) // tope de un paquete en cualquier sentido
#define VS_RST_COLA 32

enum { OP_REQUEST = 1, OP_RESPONSE, OP_RST, OP_SHUTDOWN, OP_RW, OP_CREDIT_UPDATE, OP_CREDIT_REQUEST };
#define TYPE_STREAM 1
#define SHUT_RCV_F 1u
#define SHUT_SEND_F 2u

struct vs_hdr {
  uint64_t src_cid, dst_cid;
  uint32_t src_port, dst_port, len;
  uint16_t type, op;
  uint32_t flags, buf_alloc, fwd_cnt;
} __attribute__((packed));
_Static_assert(sizeof(struct vs_hdr) == VS_HDR, "virtio_vsock_hdr is 44 bytes");

enum { C_LIBRE, C_CONECTANDO, C_ABIERTA };

struct vconn {
  int estado;
  uint32_t lport, gport;  // nuestro puerto (host) y el del invitado
  int fd;                 // nuestro extremo del socketpair (no bloqueante)
  uint32_t peer_buf, peer_fwd;  // crédito del invitado
  uint32_t tx_cnt;              // bytes mandados al invitado
  uint32_t fwd_cnt;             // bytes del invitado ya escritos en fd
  uint32_t fwd_avisado;         // fwd_cnt del último paquete que se le mandó
  uint8_t *pend;                // del invitado, pendiente de escribir en fd
  uint32_t npend;
  bool p_request, p_credit, p_rst;
  uint32_t p_shut;    // flags de un SHUTDOWN por mandar
  bool host_eof;      // nuestro extremo dio EOF: ya no se lee de fd
  bool guest_eof;     // el invitado no manda más: shutdown(SHUT_WR) cuando se vacíe pend
  bool guest_sordo;   // el invitado no recibe más
  bool cerrar;        // cierre completo del invitado: RST en cuanto se vacíe pend
  int resultado;      // para quien marca: 0 esperando, 1 abierta, -1 rechazada
  uint64_t t_cierre;  // mach_absolute_time() del SHUTDOWN completo que se mandó
};

static struct vvsock_state s;
static uint32_t intid;
static bool activo;
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;  // quien marca espera aquí; también la reanudación
static int despertador[2] = {-1, -1};
static struct vconn conns[VS_MAXCONN];
static uint32_t sig_puerto = 49152;
static struct {
  uint32_t src, dst;
} rst_cola[VS_RST_COLA];
static int n_rst;
static bool reset_pend;  // mandar TRANSPORT_RESET en cuanto haya un búfer de eventos
static mach_timebase_info_data_t tb;

static void irq(void) { vmmio_irq(&s.m, intid); }

static void despertar_hilo(void) {
  if (despertador[1] >= 0) {
    char b = 1;
    (void)!write(despertador[1], &b, 1);
  }
}

static uint64_t ms_abs(uint64_t ms) { return ms * 1000000ULL * tb.denom / tb.numer; }

static uint32_t credito(const struct vconn *c) {
  uint32_t en_vuelo = c->tx_cnt - c->peer_fwd;
  return en_vuelo >= c->peer_buf ? 0 : c->peer_buf - en_vuelo;
}

static void liberar(struct vconn *c) {
  if (c->fd >= 0) close(c->fd);
  free(c->pend);
  int estado = c->estado;
  memset(c, 0, sizeof *c);
  c->fd = -1;
  c->estado = C_LIBRE;
  c->resultado = -1;
  if (estado == C_CONECTANDO) pthread_cond_broadcast(&cv);
}

static struct vconn *buscar(uint32_t lport, uint32_t gport) {
  for (int i = 0; i < VS_MAXCONN; i++)
    if (conns[i].estado != C_LIBRE && conns[i].lport == lport && conns[i].gport == gport) return &conns[i];
  return NULL;
}

static void encolar_rst(uint32_t src, uint32_t dst) {
  if (n_rst < VS_RST_COLA) {
    rst_cola[n_rst].src = src;
    rst_cola[n_rst].dst = dst;
    n_rst++;
  }
}

// Escribe en fd lo pendiente; con guest_eof y nada pendiente, cierra el
// sentido de escritura. Con mu.
static void vaciar(struct vconn *c) {
  while (c->npend > 0) {
    ssize_t w = write(c->fd, c->pend, c->npend);
    if (w < 0) {
      if (errno == EAGAIN || errno == EINTR) break;
      c->p_rst = true;  // el otro extremo se cerró
      c->npend = 0;
      break;
    }
    memmove(c->pend, c->pend + w, c->npend - (uint32_t)w);
    c->npend -= (uint32_t)w;
    c->fwd_cnt += (uint32_t)w;
  }
  if (c->fwd_cnt - c->fwd_avisado >= VS_BUF / 4) c->p_credit = true;
  if (c->guest_eof && c->npend == 0) shutdown(c->fd, SHUT_WR);
  if (c->cerrar && c->npend == 0) c->p_rst = true;
}

// ---------- tx: lo que manda el invitado (hilo del vCPU, con mu) ----------

static void rw(struct vconn *c, const uint8_t *d, uint32_t n) {
  if (c->estado != C_ABIERTA || c->guest_eof) return;
  if (c->npend == 0) {
    ssize_t w = write(c->fd, d, n);
    if (w < 0 && errno != EAGAIN && errno != EINTR) {
      c->p_rst = true;
      return;
    }
    if (w > 0) {
      d += w;
      n -= (uint32_t)w;
      c->fwd_cnt += (uint32_t)w;
    }
  }
  if (n > 0) {
    if (c->npend + n > VS_BUF) {  // se saltó el crédito
      c->p_rst = true;
      return;
    }
    if (!c->pend && !(c->pend = malloc(VS_BUF))) {
      c->p_rst = true;
      return;
    }
    memcpy(c->pend + c->npend, d, n);
    c->npend += n;
  }
  if (c->fwd_cnt - c->fwd_avisado >= VS_BUF / 4) c->p_credit = true;
}

static void paquete(const struct vs_hdr *h, const uint8_t *d) {
  if (h->type != TYPE_STREAM || h->src_cid != VS_GUEST_CID || h->dst_cid != VS_HOST_CID) return;
  struct vconn *c = buscar(h->dst_port, h->src_port);
  if (!c) {
    if (h->op != OP_RST) encolar_rst(h->dst_port, h->src_port);
    return;
  }
  c->peer_buf = h->buf_alloc;
  c->peer_fwd = h->fwd_cnt;
  switch (h->op) {
    case OP_RESPONSE:
      if (c->estado == C_CONECTANDO) {
        c->estado = C_ABIERTA;
        c->resultado = 1;
        pthread_cond_broadcast(&cv);
      }
      break;
    case OP_RST: liberar(c); break;
    case OP_SHUTDOWN:
      if (h->flags & SHUT_SEND_F) c->guest_eof = true;
      if (h->flags & SHUT_RCV_F) c->guest_sordo = true;
      // Como Linux: quien recibe el cierre completo contesta con RST, aquí
      // después de entregar lo pendiente.
      c->cerrar = c->guest_eof && c->guest_sordo;
      vaciar(c);
      break;
    case OP_RW: rw(c, d, h->len); break;
    case OP_CREDIT_REQUEST: c->p_credit = true; break;
    case OP_CREDIT_UPDATE: break;  // el crédito ya se apuntó
    case OP_REQUEST:               // el invitado no puede abrir conexiones con el host
    default: c->p_rst = true;
  }
}

static void tx(void) {
  struct vq *q = &s.q[1];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;
  static uint8_t buf[VS_HDR + VS_PAYLOAD];
  bool hecho = false;
  int vueltas = 0;
  for (;;) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1] || vueltas++ >= (int)q->num) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    uint32_t n = vq_leer(q, desc, head, buf, sizeof buf);
    if (n >= VS_HDR) {
      struct vs_hdr h;
      memcpy(&h, buf, VS_HDR);
      if (h.len <= n - VS_HDR) paquete(&h, buf + VS_HDR);
    }
    vq_push(q, used, head, 0);
    hecho = true;
  }
  if (hecho) {
    s.m.isr |= 1;
    irq();
  }
  despertar_hilo();
}

// ---------- rx: lo que va al invitado (hilo vs_hilo, con mu) ----------

// Mete un paquete en el siguiente búfer de rx. false si no hay búfer o no cabe
// (el que llama ya miró la capacidad con rx_capacidad).
static bool rx_poner(struct vs_hdr *h, const uint8_t *d) {
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return false;
  __atomic_thread_fence(__ATOMIC_ACQUIRE);
  if (q->last_avail == *(volatile uint16_t *)&avail[1]) return false;
  uint16_t head = avail[2 + q->last_avail % q->num];
  static uint8_t buf[VS_HDR + VS_PAYLOAD];
  memcpy(buf, h, VS_HDR);
  if (h->len) memcpy(buf + VS_HDR, d, h->len);
  uint32_t total = VS_HDR + h->len;
  if (vq_capacidad(q, desc, head) < total) return false;
  q->last_avail++;
  vq_escribir(q, desc, head, buf, total);
  vq_push(q, used, head, total);
  return true;
}

// Bytes que caben en el siguiente búfer de rx (0 si no hay).
static uint32_t rx_capacidad(void) {
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return 0;
  __atomic_thread_fence(__ATOMIC_ACQUIRE);
  if (q->last_avail == *(volatile uint16_t *)&avail[1]) return 0;
  return vq_capacidad(q, desc, avail[2 + q->last_avail % q->num]);
}

static void cabecera(struct vs_hdr *h, struct vconn *c, uint16_t op, uint32_t len, uint32_t flags) {
  memset(h, 0, sizeof *h);
  h->src_cid = VS_HOST_CID;
  h->dst_cid = VS_GUEST_CID;
  h->src_port = c->lport;
  h->dst_port = c->gport;
  h->len = len;
  h->type = TYPE_STREAM;
  h->op = op;
  h->flags = flags;
  h->buf_alloc = VS_BUF;
  h->fwd_cnt = c->fwd_cnt;
  c->fwd_avisado = c->fwd_cnt;
}

static bool evento_reset(void) {
  struct vq *q = &s.q[2];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return false;
  __atomic_thread_fence(__ATOMIC_ACQUIRE);
  if (q->last_avail == *(volatile uint16_t *)&avail[1]) return false;
  uint16_t head = avail[2 + q->last_avail % q->num];
  uint32_t id = 0;  // VIRTIO_VSOCK_EVENT_TRANSPORT_RESET
  if (vq_capacidad(q, desc, head) < sizeof id) return false;
  q->last_avail++;
  vq_escribir(q, desc, head, &id, sizeof id);
  vq_push(q, used, head, sizeof id);
  return true;
}

// Manda lo que haya: el reset, los RST sueltos, los paquetes de control de
// cada conexión y los datos de las legibles (listo[i]). Devuelve si puso algo.
static bool bombear(const bool *listo) {
  bool puesto = false;
  if (reset_pend && evento_reset()) {
    reset_pend = false;
    puesto = true;
  }
  struct vs_hdr h;
  while (n_rst > 0) {
    memset(&h, 0, sizeof h);
    h.src_cid = VS_HOST_CID;
    h.dst_cid = VS_GUEST_CID;
    h.src_port = rst_cola[0].src;
    h.dst_port = rst_cola[0].dst;
    h.type = TYPE_STREAM;
    h.op = OP_RST;
    if (!rx_poner(&h, NULL)) return puesto;
    puesto = true;
    memmove(rst_cola, rst_cola + 1, sizeof rst_cola[0] * (size_t)(--n_rst));
  }
  static uint8_t datos[VS_PAYLOAD];
  for (int i = 0; i < VS_MAXCONN; i++) {
    struct vconn *c = &conns[i];
    if (c->estado == C_LIBRE) continue;
    if (c->p_rst) {
      cabecera(&h, c, OP_RST, 0, 0);
      if (!rx_poner(&h, NULL)) return puesto;
      puesto = true;
      liberar(c);
      continue;
    }
    if (c->p_request) {
      khv_qos_impulso();  // una conexión nueva del host: trabajo (vm.c, QoS)
      cabecera(&h, c, OP_REQUEST, 0, 0);
      if (!rx_poner(&h, NULL)) return puesto;
      puesto = true;
      c->p_request = false;
    }
    if (c->estado != C_ABIERTA) continue;
    // Datos: tantos paquetes como dejen el crédito y los búferes.
    while (listo[i] && !c->host_eof && !c->guest_sordo) {
      uint32_t cap = rx_capacidad(), cr = credito(c);
      if (cap <= VS_HDR) return puesto;
      if (cr == 0) break;
      uint32_t n = cap - VS_HDR;
      if (n > cr) n = cr;
      if (n > VS_PAYLOAD) n = VS_PAYLOAD;
      ssize_t r = read(c->fd, datos, n);
      if (r < 0) {
        if (errno != EAGAIN && errno != EINTR) c->p_rst = true;
        break;
      }
      if (r == 0) {
        c->host_eof = true;
        c->p_shut |= SHUT_SEND_F;
        break;
      }
      khv_qos_impulso();
      cabecera(&h, c, OP_RW, (uint32_t)r, 0);
      if (!rx_poner(&h, datos)) {  // no pasa: la capacidad ya se miró con mu
        c->p_rst = true;
        break;
      }
      c->tx_cnt += (uint32_t)r;
      puesto = true;
    }
    if (c->p_shut) {
      // Con el host sin leer ni escribir (nuestro extremo cerrado del todo,
      // que es lo que hace Close en Go) el cierre es completo.
      uint32_t f = c->p_shut;
      if (c->host_eof && c->npend == 0 && c->guest_eof) f |= SHUT_RCV_F;
      cabecera(&h, c, OP_SHUTDOWN, 0, f);
      if (!rx_poner(&h, NULL)) return puesto;
      puesto = true;
      c->p_shut = 0;
      // Tras un cierre completo el invitado debe contestar con RST.
      if ((f & SHUT_RCV_F) && !c->t_cierre) c->t_cierre = mach_absolute_time();
    }
    if (c->p_credit) {
      cabecera(&h, c, OP_CREDIT_UPDATE, 0, 0);
      if (!rx_poner(&h, NULL)) return puesto;
      puesto = true;
      c->p_credit = false;
    }
  }
  return puesto;
}

static void *vs_hilo(void *a) {
  (void)a;
  khv_qos_dispositivo();
  struct pollfd pf[1 + VS_MAXCONN];
  int idx[VS_MAXCONN];
  bool listo[VS_MAXCONN];
  for (;;) {
    int n = 0;
    int plazo = -1;
    pthread_mutex_lock(&mu);
    pf[n].fd = despertador[0];
    pf[n].events = POLLIN;
    n++;
    for (int i = 0; i < VS_MAXCONN; i++) {
      struct vconn *c = &conns[i];
      if (c->estado != C_ABIERTA) continue;
      short ev = 0;
      if (!c->host_eof && !c->guest_sordo && credito(c) > 0) ev |= POLLIN;
      if (c->npend) ev |= POLLOUT;
      if (c->t_cierre) plazo = 500;
      if (!ev) continue;
      idx[n - 1] = i;
      pf[n].fd = c->fd;
      pf[n].events = ev;
      n++;
    }
    pthread_mutex_unlock(&mu);
    int r = poll(pf, (nfds_t)n, plazo);
    if (r < 0 && errno != EINTR) return NULL;
    if (pf[0].revents & POLLIN) {
      char b[64];
      while (read(despertador[0], b, sizeof b) > 0) {
      }
    }
    pthread_mutex_lock(&mu);
    // Con la VM en pausa no se toca la RAM del invitado.
    while (g_pausa) pthread_cond_wait(&cv, &mu);
    memset(listo, 0, sizeof listo);
    for (int k = 1; k < n; k++) {
      struct vconn *c = &conns[idx[k - 1]];
      if (c->estado != C_ABIERTA || c->fd != pf[k].fd) continue;
      if (pf[k].revents & POLLOUT) vaciar(c);
      if (pf[k].revents & (POLLIN | POLLHUP | POLLERR)) listo[idx[k - 1]] = true;
    }
    // Un cierre al que el invitado no contesta con RST en 2 s se da por hecho.
    uint64_t ahora = mach_absolute_time();
    for (int i = 0; i < VS_MAXCONN; i++)
      if (conns[i].estado != C_LIBRE && conns[i].t_cierre && ahora - conns[i].t_cierre > ms_abs(2000))
        conns[i].p_rst = true;
    if (activo && bombear(listo)) {
      s.m.isr |= 1;
      irq();
    }
    pthread_mutex_unlock(&mu);
  }
}

// ---------- API ----------

void vsock_init(uint32_t id) {
  intid = id;
  mach_timebase_info(&tb);
  memset(&s, 0, sizeof s);
  for (int i = 0; i < VS_MAXCONN; i++) {
    conns[i].fd = -1;
    conns[i].estado = C_LIBRE;
  }
  if (pipe(despertador) == 0) {
    for (int i = 0; i < 2; i++) {
      fcntl(despertador[i], F_SETFL, O_NONBLOCK);
      fcntl(despertador[i], F_SETFD, FD_CLOEXEC);
    }
  }
  activo = true;
  pthread_t t;
  pthread_create(&t, NULL, vs_hilo, NULL);
  pthread_detach(t);
}

static void cerrar_todas(void) {
  for (int i = 0; i < VS_MAXCONN; i++)
    if (conns[i].estado != C_LIBRE) liberar(&conns[i]);
  n_rst = 0;
}

bool vsock_mmio(uint64_t off, bool escribir, uint64_t *val, int size) {
  pthread_mutex_lock(&mu);
  if (off >= 0x100) {  // configuración: guest_cid (le64)
    if (!escribir) {
      uint64_t cid = VS_GUEST_CID, o = off - 0x100, r = 0;
      if (o + (uint64_t)size <= sizeof cid) memcpy(&r, (uint8_t *)&cid + o, (size_t)size);
      *val = r;
    }
    pthread_mutex_unlock(&mu);
    return true;
  }
  uint32_t cola = 0;
  switch (vmmio_acceso(&s.m, s.q, 3, 19, 0, off, escribir, val, &cola)) {
    case VM_NOTIFY:
      if (cola == 1) tx();
      else despertar_hilo();  // búferes nuevos de rx o de eventos
      break;
    case VM_ISR: irq(); break;
    case VM_RESET: {
      uint8_t nivel = s.m.nivel;
      cerrar_todas();
      memset(&s, 0, sizeof s);
      s.m.nivel = nivel;
      reset_pend = false;
      irq();
      break;
    }
  }
  pthread_mutex_unlock(&mu);
  return true;
}

int khv_vsock_dial(uint32_t port, int plazo_ms) {
  pthread_mutex_lock(&mu);
  if (!activo) {
    pthread_mutex_unlock(&mu);
    return -ENODEV;
  }
  struct vconn *c = NULL;
  for (int i = 0; i < VS_MAXCONN && !c; i++)
    if (conns[i].estado == C_LIBRE) c = &conns[i];
  if (!c) {
    pthread_mutex_unlock(&mu);
    return -EMFILE;
  }
  int sv[2];
  if (socketpair(AF_UNIX, SOCK_STREAM, 0, sv) != 0) {
    int e = errno;
    pthread_mutex_unlock(&mu);
    return -e;
  }
  int uno = 1, tam = (int)VS_BUF;
  for (int i = 0; i < 2; i++) {
    setsockopt(sv[i], SOL_SOCKET, SO_NOSIGPIPE, &uno, sizeof uno);
    setsockopt(sv[i], SOL_SOCKET, SO_SNDBUF, &tam, sizeof tam);
    setsockopt(sv[i], SOL_SOCKET, SO_RCVBUF, &tam, sizeof tam);
    fcntl(sv[i], F_SETFD, FD_CLOEXEC);
  }
  fcntl(sv[0], F_SETFL, O_NONBLOCK);
  do {
    if (++sig_puerto < 49152) sig_puerto = 49152;
  } while (buscar(sig_puerto, port));
  memset(c, 0, sizeof *c);
  c->estado = C_CONECTANDO;
  c->lport = sig_puerto;
  c->gport = port;
  c->fd = sv[0];
  c->p_request = true;
  despertar_hilo();
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  uint64_t fin = (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec + (uint64_t)plazo_ms * 1000000ULL;
  ts.tv_sec = (time_t)(fin / 1000000000ULL);
  ts.tv_nsec = (long)(fin % 1000000000ULL);
  int ret = -ETIMEDOUT;
  for (;;) {
    if (c->estado == C_ABIERTA && c->fd == sv[0]) {
      ret = sv[1];
      break;
    }
    if (c->estado != C_CONECTANDO || c->fd != sv[0]) {
      ret = -ECONNREFUSED;
      break;
    }
    if (pthread_cond_timedwait(&cv, &mu, &ts) == ETIMEDOUT) {
      if (c->estado == C_CONECTANDO && c->fd == sv[0]) {
        c->p_request = false;
        c->p_rst = true;  // que el invitado olvide la petición
        despertar_hilo();
      }
      ret = -ETIMEDOUT;
      if (c->estado == C_ABIERTA && c->fd == sv[0]) ret = sv[1];
      break;
    }
  }
  pthread_mutex_unlock(&mu);
  if (ret < 0) close(sv[1]);
  return ret;
}

void vsock_quieto(void) {
  pthread_mutex_lock(&mu);
  pthread_mutex_unlock(&mu);
}

void vsock_despertar(void) {
  pthread_mutex_lock(&mu);
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  despertar_hilo();
}

void vsock_get(struct vvsock_state *o) {
  pthread_mutex_lock(&mu);
  *o = s;
  pthread_mutex_unlock(&mu);
}

void vsock_reafirmar(void) {
  pthread_mutex_lock(&mu);
  hv_gic_set_spi(intid, s.m.nivel);
  pthread_mutex_unlock(&mu);
}

// Tras restaurar: las colas siguen donde las dejó el invitado y sus
// conexiones ya no existen aquí, así que se le avisa con TRANSPORT_RESET.
void vsock_set(const struct vvsock_state *i) {
  pthread_mutex_lock(&mu);
  cerrar_todas();
  s = *i;
  reset_pend = (s.m.status & 4) != 0;  // DRIVER_OK: hay un driver que lo espera
  pthread_mutex_unlock(&mu);
}
