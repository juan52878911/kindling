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
#include <sys/event.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#include "khv.h"
#include "vq_iov.h"

#define VS_HOST_CID 2
#define VS_GUEST_CID 3
#define VS_MAXCONN 64
#define VS_BUF (256u * 1024)    // crédito que se anuncia al invitado por conexión
#define VS_PAYLOAD (64u * 1024) // tope de un paquete en cualquier sentido
#define VS_RST_COLA 32
// Lo más que mete en el invitado una vuelta de bombear, con mu tomado: el
// vCPU que notifica espera en vsock_mmio, y la pausa en vsock_quieto. El
// crédito lo da el invitado (buf_alloc, hasta 4 GiB por conexión) y puede
// reponer búferes sin avisar, así que sin tope la vuelta dura lo que el
// escritor del host quiera. Lo que quede sale en la vuelta siguiente: el fd
// sigue legible y poll vuelve al momento.
#define VS_TANDA_MAX (1u << 20)

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
  bool vigilada;      // su cierre completo lo avisa kq (ver vigilar)
  // gen sube al liberar el slot y al tomarlo: quien marca reconoce así su
  // conexión. El fd no basta: liberar lo cierra y el dial siguiente que tome
  // el slot puede recibir el mismo número.
  uint32_t gen;
};

static struct vvsock_state s;
static uint32_t intid;
// activo: el dispositivo atiende (lo pone vsock_init; lo quita vs_hilo si
// muere). hilo_vivo: vs_hilo corre. Con mu.
static bool activo, hilo_vivo;
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;  // quien marca espera aquí; también la reanudación
static int despertador[2] = {-1, -1};
// kq avisa, sin sondear, de que Go cerró del todo su extremo de una conexión
// que el poll ya no mira (ver vigilar). -1 si no se pudo crear: entonces la
// sonda de cada segundo (sondear_cerradas).
static int kq = -1;
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

// El fd de una conexión no se cierra mientras vs_hilo lo espera en poll:
// close de un fd que otro hilo tiene en poll puede quedarse esperando en el
// kernel para siempre (visto en macOS 26.5: el hilo de la prueba de tx
// colgado en close con mu tomado, y vs_hilo en poll sin plazo; con un RST del
// invitado le pasaría al vCPU). Con en_poll > 0, liberar deja el fd en
// diferidos y despierta al hilo, que lo cierra al salir de poll. Con mu.
static int en_poll;  // hilos de vsock dentro de poll (uno; dos en las pruebas)
static int diferidos[2 * VS_MAXCONN];
static int n_diferidos;

static void cerrar_fd(int fd) {
  if (en_poll > 0) {
    despertar_hilo();
    if (n_diferidos < (int)(sizeof diferidos / sizeof diferidos[0])) {
      diferidos[n_diferidos++] = fd;
      return;
    }
    // Sin hueco (no debería pasar: el hilo vacía la lista en cada vuelta):
    // cerrar ya, con el hilo ya avisado para que salga de poll.
  }
  close(fd);
}

// Con mu, y con en_poll a 0: ningún hilo los tiene en poll.
static void cerrar_diferidos(void) {
  for (int i = 0; i < n_diferidos; i++) close(diferidos[i]);
  n_diferidos = 0;
}

static uint64_t ms_abs(uint64_t ms) { return ms * 1000000ULL * tb.denom / tb.numer; }

static uint32_t credito(const struct vconn *c) {
  uint32_t en_vuelo = c->tx_cnt - c->peer_fwd;
  return en_vuelo >= c->peer_buf ? 0 : c->peer_buf - en_vuelo;
}

static void liberar(struct vconn *c) {
  if (c->fd >= 0) cerrar_fd(c->fd);
  free(c->pend);
  int estado = c->estado;
  uint32_t gen = c->gen + 1;
  memset(c, 0, sizeof *c);
  c->gen = gen;
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

// iov_sacar copia a dst los primeros len bytes de iov (que ya se sabe que los
// tiene).
static void iov_sacar(void *dst, const struct iovec *iov, int n, size_t len) {
  uint8_t *o = dst;
  for (int j = 0; j < n && len > 0; j++) {
    size_t d = iov[j].iov_len < len ? iov[j].iov_len : len;
    memcpy(o, iov[j].iov_base, d);
    o += d;
    len -= d;
  }
}

// rw escribe en fd los n bytes de datos que manda el invitado, directamente
// desde sus búferes (d, nd iovec, ya medidos y dentro de la RAM); lo que no
// cabe va a pend.
static void rw(struct vconn *c, const struct iovec *d, int nd, uint32_t n) {
  if (c->estado != C_ABIERTA || c->guest_eof) return;
  uint32_t salto = 0;
  if (c->npend == 0 && n > 0) {  // writev sin iovec da EINVAL
    ssize_t w = writev(c->fd, d, nd);
    if (w < 0 && errno != EAGAIN && errno != EINTR) {
      c->p_rst = true;
      return;
    }
    if (w > 0) {
      salto = (uint32_t)w;
      c->fwd_cnt += (uint32_t)w;
    }
  }
  uint32_t resto = n - salto;
  if (resto > 0) {
    if (c->npend + resto > VS_BUF) {  // se saltó el crédito
      c->p_rst = true;
      return;
    }
    if (!c->pend && !(c->pend = malloc(VS_BUF))) {
      c->p_rst = true;
      return;
    }
    static struct iovec tramo[QMAX];
    int k = iov_tramo(d, nd, salto, resto, tramo);
    iov_sacar(c->pend + c->npend, tramo, k, resto);
    c->npend += resto;
  }
  if (c->fwd_cnt - c->fwd_avisado >= VS_BUF / 4) c->p_credit = true;
}

// mira resume lo de una conexión que decide qué hace vs_hilo: qué fd espera
// en poll (armar_poll, por_cerrar) y qué manda (bombear, vaciar). Si un
// paquete del invitado no lo cambia, el hilo no tiene nada nuevo que hacer
// por él y no hace falta despertarlo. Cualquier campo que lean esas
// funciones y cambie tx tiene que estar aquí: si no, el hilo se queda
// esperando en poll con el conjunto viejo (un despertar perdido).
static uint32_t mira(const struct vconn *c) {
  return (uint32_t)c->estado | (uint32_t)c->p_request << 2 | (uint32_t)c->p_credit << 3 |
         (uint32_t)c->p_rst << 4 | (uint32_t)(c->p_shut != 0) << 5 | (uint32_t)(c->npend > 0) << 6 |
         (uint32_t)(credito(c) > 0) << 7 | (uint32_t)c->host_eof << 8 | (uint32_t)c->guest_eof << 9 |
         (uint32_t)c->guest_sordo << 10 | (uint32_t)c->cerrar << 11;
}

// paquete atiende un paquete del invitado con su cabecera h y sus datos (d,
// nd iovec, h->len bytes). Devuelve si cambió algo que vs_hilo tenga que ver:
// un RST por mandar, o lo que resume mira (crédito que se abre, una conexión
// que se abre o se libera, pend que se llena o se vacía, un cierre...).
static bool paquete(const struct vs_hdr *h, const struct iovec *d, int nd) {
  if (h->type != TYPE_STREAM || h->src_cid != VS_GUEST_CID || h->dst_cid != VS_HOST_CID) return false;
  struct vconn *c = buscar(h->dst_port, h->src_port);
  if (!c) {
    int n = n_rst;
    if (h->op != OP_RST) encolar_rst(h->dst_port, h->src_port);
    return n_rst != n;
  }
  uint32_t antes = mira(c), gen = c->gen;
  c->peer_buf = h->buf_alloc;
  c->peer_fwd = h->fwd_cnt;
  switch (h->op) {
    case OP_RESPONSE:
      if (c->estado == C_CONECTANDO) {
        c->estado = C_ABIERTA;
        c->resultado = 1;
        pthread_cond_broadcast(&cv);
        // La primera conexión aceptada: el agente ya escucha (ver traza.c).
        khv_traza_hito(KHV_H_AGENTE);
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
    case OP_RW: rw(c, d, nd, h->len); break;
    case OP_CREDIT_REQUEST: c->p_credit = true; break;
    case OP_CREDIT_UPDATE: break;  // el crédito ya se apuntó
    case OP_REQUEST:               // el invitado no puede abrir conexiones con el host
    default: c->p_rst = true;
  }
  return c->gen != gen || mira(c) != antes;
}

static void tx(void) {
  struct vq *q = &s.q[1];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;
  // Los búferes del invitado como iovec: la cabecera se copia (se lee una
  // vez, el invitado puede cambiarla mientras tanto) y los datos van de su
  // RAM al socket sin pasar por un búfer intermedio. Los topes, los de
  // vq_leer: cadena válida, dentro de la RAM y de VS_HDR + VS_PAYLOAD como
  // mucho; si no, el paquete se descarta entero.
  static struct iovec iov[QMAX], datos[QMAX];
  bool hecho = false, despertar = false;
  int vueltas = 0;
  for (;;) {
    __atomic_thread_fence(__ATOMIC_ACQUIRE);
    if (q->last_avail == *(volatile uint16_t *)&avail[1] || vueltas++ >= (int)q->num) break;
    uint16_t head = avail[2 + q->last_avail % q->num];
    q->last_avail++;
    uint64_t n = 0;
    int ni = vq_iov(q, desc, head, false, iov, QMAX, &n);
    if (ni > 0 && n >= VS_HDR && n <= VS_HDR + VS_PAYLOAD) {
      struct vs_hdr h;
      iov_sacar(&h, iov, ni, VS_HDR);
      if (h.len <= n - VS_HDR && paquete(&h, datos, iov_tramo(iov, ni, VS_HDR, h.len, datos))) despertar = true;
    }
    vq_push(q, used, head, 0);
    hecho = true;
  }
  if (hecho) {
    s.m.isr |= 1;
    irq();
  }
  // Solo si vs_hilo tiene algo nuevo (paquete): antes era en cada aviso, y
  // cada paquete de datos del invitado costaba una vuelta del hilo (write al
  // pipe, poll, armar_poll con todas las conexiones) sin nada que hacer.
  if (despertar) despertar_hilo();
}

// ---------- rx: lo que va al invitado (hilo vs_hilo, con mu) ----------

// El siguiente búfer de rx del invitado, como iovec: rx_mirar lo prepara (una
// sola pasada por la cadena) sin gastarlo, y rx_cerrar lo devuelve lleno.
static struct {
  uint16_t head;
  int n;
  uint64_t cap;
  struct iovec iov[QMAX];
} rxb;

// rx_mirar prepara rxb con el siguiente búfer de rx; false si no hay (o no
// hay driver). Una cadena que no vale queda con cap 0: no cabe nada. Con mu.
static bool rx_mirar(void) {
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return false;
  __atomic_thread_fence(__ATOMIC_ACQUIRE);
  if (q->last_avail == *(volatile uint16_t *)&avail[1]) return false;
  rxb.head = avail[2 + q->last_avail % q->num];
  rxb.n = vq_iov(q, desc, rxb.head, true, rxb.iov, QMAX, &rxb.cap);
  if (rxb.n < 0) rxb.n = 0, rxb.cap = 0;
  return true;
}

// rx_cerrar escribe la cabecera h al principio del búfer de rx_mirar (los
// datos, si los hay, ya están detrás) y lo devuelve al invitado. Con mu.
static void rx_cerrar(const struct vs_hdr *h) {
  struct vq *q = &s.q[0];
  uint16_t *avail;
  uint8_t *used;
  struct vdesc *desc;
  if (!vq_ptrs(q, &avail, &used, &desc)) return;
  iov_poner(rxb.iov, rxb.n, h, VS_HDR);
  q->last_avail++;
  vq_push(q, used, rxb.head, VS_HDR + h->len);
}

// Mete un paquete de control (sin datos) en el siguiente búfer de rx. false
// si no hay búfer o no cabe.
static bool rx_poner(struct vs_hdr *h) {
  if (!rx_mirar() || rxb.cap < VS_HDR) return false;
  rx_cerrar(h);
  return true;
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
// cada conexión y los datos de las legibles (listo[i]), hasta VS_TANDA_MAX
// bytes de datos o hasta que llegue una pausa. Devuelve si puso algo.
// bombear_desde es la conexión por la que empieza: la siguiente a la que
// agotó la tanda, para que una que siempre tiene datos no deje sin turno a
// las de detrás.
static int bombear_desde;

static bool bombear(const bool *listo) {
  bool puesto = false;
  uint64_t tanda = 0;
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
    if (!rx_poner(&h)) return puesto;
    puesto = true;
    memmove(rst_cola, rst_cola + 1, sizeof rst_cola[0] * (size_t)(--n_rst));
  }
  static struct iovec datos[QMAX];
  for (int k = 0; k < VS_MAXCONN; k++) {
    int i = (bombear_desde + k) % VS_MAXCONN;
    struct vconn *c = &conns[i];
    if (c->estado == C_LIBRE) continue;
    if (c->p_rst) {
      cabecera(&h, c, OP_RST, 0, 0);
      if (!rx_poner(&h)) return puesto;
      puesto = true;
      liberar(c);
      continue;
    }
    if (c->p_request) {
      khv_qos_impulso();  // una conexión nueva del host: trabajo (vm.c, QoS)
      cabecera(&h, c, OP_REQUEST, 0, 0);
      if (!rx_poner(&h)) return puesto;
      puesto = true;
      c->p_request = false;
    }
    if (c->estado != C_ABIERTA) continue;
    // Datos: tantos paquetes como dejen el crédito y los búferes, leídos del
    // socket directamente en el búfer del invitado, detrás de la cabecera.
    while (listo[i] && !c->host_eof && !c->guest_sordo) {
      uint32_t cr = credito(c);
      if (!rx_mirar() || rxb.cap <= VS_HDR) return puesto;
      if (cr == 0) break;
      uint64_t n = rxb.cap - VS_HDR;
      if (n > cr) n = cr;
      if (n > VS_PAYLOAD) n = VS_PAYLOAD;
      ssize_t r = readv(c->fd, datos, iov_tramo(rxb.iov, rxb.n, VS_HDR, n, datos));
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
      rx_cerrar(&h);
      c->tx_cnt += (uint32_t)r;
      puesto = true;
      tanda += (uint64_t)r;
      if (tanda >= VS_TANDA_MAX || g_pausa) {
        bombear_desde = (i + 1) % VS_MAXCONN;
        return puesto;
      }
    }
    if (c->p_shut) {
      // Con el host sin leer ni escribir (nuestro extremo cerrado del todo,
      // que es lo que hace Close en Go) el cierre es completo.
      uint32_t f = c->p_shut;
      if (c->host_eof && c->npend == 0 && c->guest_eof) f |= SHUT_RCV_F;
      cabecera(&h, c, OP_SHUTDOWN, 0, f);
      if (!rx_poner(&h)) return puesto;
      puesto = true;
      c->p_shut = 0;
      // Tras un cierre completo el invitado debe contestar con RST.
      if ((f & SHUT_RCV_F) && !c->t_cierre) c->t_cierre = mach_absolute_time();
    }
    if (c->p_credit) {
      cabecera(&h, c, OP_CREDIT_UPDATE, 0, 0);
      if (!rx_poner(&h)) return puesto;
      puesto = true;
      c->p_credit = false;
    }
  }
  return puesto;
}

// por_cerrar: la conexión ya no tiene nada que leer ni escribir (host_eof, o
// el invitado ya no recibe) y falta saber si Go cierra su extremo del todo.
static bool por_cerrar(const struct vconn *c) {
  if (c->estado != C_ABIERTA || c->npend || !(c->host_eof || c->guest_sordo)) return false;
  if ((c->p_shut & SHUT_RCV_F) && (c->p_shut & SHUT_SEND_F)) return false;
  return !c->t_cierre;
}

// vigilar apunta el fd de una conexión por_cerrar en kq: EVFILT_WRITE con
// EV_CLEAR avisa al apuntarlo y luego solo cuando el socket cambia; con el
// otro extremo cerrado del todo el aviso trae EV_EOF (ya no se puede mandar
// nada), lo mismo que la sonda veía como POLLHUP sin POLLOUT. Un medio cierre
// de Go (CloseWrite) no lo pone. Si ya estaba cerrado, el aviso llega al
// apuntarlo. Cerrar el fd (liberar) lo quita de kq. false si no se pudo (sin
// kq): entonces la mira sondear_cerradas. Con mu.
static bool vigilar(struct vconn *c, int i) {
  if (c->vigilada) return true;
  if (kq < 0) return false;
  struct kevent ev;
  EV_SET(&ev, (uintptr_t)c->fd, EVFILT_WRITE, EV_ADD | EV_CLEAR, 0, 0, (void *)(intptr_t)i);
  if (kevent(kq, &ev, 1, NULL, 0, NULL) != 0) return false;
  c->vigilada = true;
  return true;
}

// armar_poll llena pf con los fd de las conexiones abiertas que esperan algo
// y devuelve cuántos; idx[k] es el slot de pf[k]. Con mu. Una conexión sin
// nada que leer ni escribir (host_eof, o el invitado ya no recibe) no entra:
// en macOS poll no avisa de POLLHUP con events = 0, y pedir POLLOUT la
// tendría siempre lista. A esas las vigila kq (vigilar); sin kq, las mira
// sondear_cerradas cada segundo.
static int armar_poll(struct pollfd *pf, int *idx, int *plazo) {
  int n = 0;
  for (int i = 0; i < VS_MAXCONN; i++) {
    struct vconn *c = &conns[i];
    if (c->estado != C_ABIERTA) continue;
    short ev = 0;
    if (!c->host_eof && !c->guest_sordo && credito(c) > 0) ev |= POLLIN;
    if (c->npend) ev |= POLLOUT;
    if (c->t_cierre) *plazo = 500;
    if (!ev) {
      if (por_cerrar(c) && !vigilar(c, i) && (*plazo < 0 || *plazo > 1000)) *plazo = 1000;
      continue;
    }
    idx[n] = i;
    pf[n].fd = c->fd;
    pf[n].events = ev;
    pf[n].revents = 0;
    n++;
  }
  return n;
}

// sondear_cerradas mira, sin esperar, las conexiones por_cerrar que kq no
// vigila (solo pasa sin kq): con el otro extremo cerrado del todo (close en
// Go) POLLOUT devuelve solo POLLHUP; con un medio cierre (CloseWrite)
// devuelve POLLOUT. Al primero se le pide al invitado el cierre completo, que
// contesta con RST (o el plazo de 2 s lo da por hecho) y libera el slot.
// Antes esas conexiones no se miraban y, si el invitado tampoco mandaba RST,
// el slot y el fd quedaban ocupados para siempre: 64 así y ya no había exec.
// Con mu.
static void sondear_cerradas(void) {
  for (int i = 0; i < VS_MAXCONN; i++) {
    struct vconn *c = &conns[i];
    if (!por_cerrar(c) || c->vigilada) continue;
    struct pollfd pf = {.fd = c->fd, .events = POLLOUT};
    if (poll(&pf, 1, 0) > 0 && (pf.revents & (POLLHUP | POLLERR)) && !(pf.revents & POLLOUT))
      c->p_shut |= SHUT_SEND_F | SHUT_RCV_F;
  }
}

// atender_kq recoge, sin esperar, los avisos de kq: a la conexión cuyo
// extremo de Go se cerró del todo se le pide al invitado el cierre completo,
// como hacía sondear_cerradas. Un aviso sin EV_EOF (el socket cambió por
// otra cosa) no dice nada. El slot se comprueba por su fd y por vigilada: el
// aviso de un fd ya cerrado no llega (close lo quita de kq), y un slot
// reusado empieza sin vigilada. Con mu.
static void atender_kq(void) {
  if (kq < 0) return;
  struct kevent evs[16];
  const struct timespec cero = {0, 0};
  for (;;) {
    int n = kevent(kq, NULL, 0, evs, 16, &cero);
    for (int k = 0; k < n; k++) {
      intptr_t i = (intptr_t)evs[k].udata;
      if (i < 0 || i >= VS_MAXCONN || !(evs[k].flags & EV_EOF)) continue;
      struct vconn *c = &conns[i];
      if (!c->vigilada || c->fd < 0 || (uintptr_t)c->fd != evs[k].ident || !por_cerrar(c)) continue;
      c->p_shut |= SHUT_SEND_F | SHUT_RCV_F;
    }
    if (n < 16) return;
  }
}

// tras_poll aplica lo que poll vio en los n fd de armar_poll. Con mu.
static void tras_poll(const struct pollfd *pf, const int *idx, int n, bool *listo) {
  memset(listo, 0, sizeof(bool) * VS_MAXCONN);
  for (int k = 0; k < n; k++) {
    struct vconn *c = &conns[idx[k]];
    if (c->estado != C_ABIERTA || c->fd != pf[k].fd) continue;
    // Con algo pendiente para Go y su extremo cerrado del todo, poll da
    // POLLHUP sin POLLOUT: vaciar se entera (EPIPE, y RST al invitado). Antes
    // el fd volvía listo en cada vuelta sin que nadie lo atendiera, y el hilo
    // giraba en vacío.
    if ((pf[k].revents & POLLOUT) || (c->npend && (pf[k].revents & (POLLHUP | POLLERR)))) vaciar(c);
    if (pf[k].revents & (POLLIN | POLLHUP | POLLERR)) listo[idx[k]] = true;
  }
  atender_kq();
  sondear_cerradas();
}

// vs_vueltas cuenta las vueltas de vs_hilo (para ver en las pruebas que sigue
// vivo).
static unsigned vs_vueltas;

static void cerrar_todas(void);

static void *vs_hilo(void *a) {
  (void)a;
  khv_qos_dispositivo();
  // pf[0] es el despertador, pf[1] kq (un kqueue se espera con poll: está
  // listo cuando tiene avisos) y luego van las conexiones de armar_poll.
  struct pollfd pf[2 + VS_MAXCONN];
  int idx[VS_MAXCONN];
  bool listo[VS_MAXCONN];
  for (;;) {
    int plazo = -1;
    pthread_mutex_lock(&mu);
    pf[0] = (struct pollfd){.fd = despertador[0], .events = POLLIN};
    pf[1] = (struct pollfd){.fd = kq, .events = POLLIN};  // sin kq (-1), poll lo salta
    int n = armar_poll(pf + 2, idx, &plazo) + 2;
    en_poll++;
    pthread_mutex_unlock(&mu);
    __atomic_fetch_add(&vs_vueltas, 1, __ATOMIC_RELEASE);
    int r = khv_poll(KHV_POLL_VSOCK, pf, (nfds_t)n, plazo);
    int e = errno;
    pthread_mutex_lock(&mu);
    if (--en_poll == 0) cerrar_diferidos();
    if (r < 0 && e == EAGAIN) {
      // poll no pudo reservar memoria; otra llamada puede ir bien. Terminar
      // dejaba al invitado sin agente (y sin canal) para siempre.
      pthread_mutex_unlock(&mu);
      usleep(1000);
      continue;
    }
    if (r < 0 && e != EINTR) {
      // Sin este hilo no sale nada hacia el invitado: el dispositivo queda
      // muerto. Se sueltan las conexiones (quien espera un dial se entera ya)
      // y los dials siguientes fallan con -ENODEV al momento; antes cada uno
      // esperaba su plazo entero (2 s) a un hilo que no iba a contestar.
      // vsock_init lo vuelve a arrancar.
      activo = false;
      hilo_vivo = false;
      cerrar_todas();
      pthread_cond_broadcast(&cv);
      pthread_mutex_unlock(&mu);
      // Que no se pierda en silencio, como en net.c.
      uart_flush();
      dprintf(2, "kling-hv: vsock: the device thread stops (poll: %s); the guest gets no more packets\n", strerror(e));
      return NULL;
    }
    if (pf[0].revents & POLLIN) {
      char b[64];
      while (read(despertador[0], b, sizeof b) > 0) {
      }
    }
    // Con la VM en pausa no se toca la RAM del invitado.
    while (g_pausa) pthread_cond_wait(&cv, &mu);
    tras_poll(pf + 2, idx, n - 2, listo);
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
  if (kq < 0) kq = kqueue();  // un kqueue no pasa a los hijos: no hace falta FD_CLOEXEC
  // Un hilo por proceso: las pruebas llaman a vsock_init más de una vez, y
  // un segundo vs_hilo con su propio poll dejaba diferidos sin cerrar. Otro
  // solo si el anterior murió (vs_hilo).
  pthread_mutex_lock(&mu);
  activo = true;
  bool arrancar = !hilo_vivo;
  hilo_vivo = true;
  pthread_mutex_unlock(&mu);
  if (!arrancar) return;
  if (despertador[0] < 0 && pipe(despertador) == 0) {
    for (int i = 0; i < 2; i++) {
      fcntl(despertador[i], F_SETFL, O_NONBLOCK);
      fcntl(despertador[i], F_SETFD, FD_CLOEXEC);
    }
  }
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

// reservar toma un slot libre para un dial a port, le abre su socketpair (sv[0]
// para el slot, sv[1] para quien marca) y devuelve el slot, con su generación
// en *gen; NULL y -errno en *err si no puede. Con mu.
static struct vconn *reservar(uint32_t port, int sv[2], uint32_t *gen, int *err) {
  struct vconn *c = NULL;
  for (int i = 0; i < VS_MAXCONN && !c; i++)
    if (conns[i].estado == C_LIBRE) c = &conns[i];
  if (!c) {
    *err = -EMFILE;
    return NULL;
  }
  if (socketpair(AF_UNIX, SOCK_STREAM, 0, sv) != 0) {
    *err = -errno;
    return NULL;
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
  uint32_t g = c->gen + 1;
  memset(c, 0, sizeof *c);
  c->gen = g;
  c->estado = C_CONECTANDO;
  c->lport = sig_puerto;
  c->gport = port;
  c->fd = sv[0];
  c->p_request = true;
  *gen = g;
  return c;
}

int khv_vsock_dial(uint32_t port, int plazo_ms) {
  pthread_mutex_lock(&mu);
  if (!activo) {
    pthread_mutex_unlock(&mu);
    return -ENODEV;
  }
  int sv[2], err = 0;
  uint32_t gen;
  struct vconn *c = reservar(port, sv, &gen, &err);
  if (!c) {
    pthread_mutex_unlock(&mu);
    return err;
  }
  despertar_hilo();
  uint64_t fin = khv_ahora_ns() + (uint64_t)plazo_ms * 1000000ULL;
  // El slot es nuestro mientras su generación sea gen: si cambia, se liberó
  // (rechazo, RST, reinicio) y quizá ya es de otro dial.
  int ret = -ETIMEDOUT;
  for (;;) {
    if (!activo) {  // vs_hilo murió: ya soltó el slot
      ret = -ENODEV;
      break;
    }
    if (c->gen != gen || c->estado != C_CONECTANDO) {
      ret = c->gen == gen && c->estado == C_ABIERTA ? sv[1] : -ECONNREFUSED;
      break;
    }
    if (khv_esperar_hasta(&cv, &mu, fin) == ETIMEDOUT) {
      if (c->gen == gen && c->estado == C_CONECTANDO) {
        // El slot se suelta ya. Antes quedaba con p_rst esperando un búfer
        // de rx para mandar el RST: con un invitado que no repone (colgado,
        // sin driver) no llegaba nunca, y 64 dials así agotaban los slots.
        // Si la petición llegó a salir, el RST va por la cola sin slot; un
        // RESPONSE tardío a ese puerto ya no encuentra conexión y recibe
        // otro RST (paquete).
        uint32_t lport = c->lport, gport = c->gport;
        bool enviada = !c->p_request;
        liberar(c);
        if (enviada) encolar_rst(lport, gport);
        despertar_hilo();
      }
      ret = -ETIMEDOUT;
      if (c->gen == gen && c->estado == C_ABIERTA) ret = sv[1];
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

// ---------- solo para las pruebas (hvvm_darwin_test.go) ----------

#define PRUEBA_PUERTO 4242

struct prueba_dial {
  int ret;
};

static void *prueba_marcar(void *arg) {
  ((struct prueba_dial *)arg)->ret = khv_vsock_dial(PRUEBA_PUERTO, 2000);
  return NULL;
}

// khv_vsock_prueba_reuso reproduce dos dials que se cruzan en un slot, sin
// invitado: el primero (A) espera respuesta; su slot se libera (el RST del
// invitado) y, antes de que A despierte, otro dial (B) toma el mismo slot y el
// invitado le contesta. A tiene que ver que el slot ya no es suyo
// (-ECONNREFUSED) y no tocar la conexión de B. Devuelve el error que dio A; 1
// si A se quedó con un fd (creyó suya la de B), -EPROTO si tocó la conexión
// de B, -EBUSY si A no llegó a esperar. En *coincide_fd, si B recibió el mismo número
// de fd que A tenía (el caso en que el fd solo no los distinguía). No puede
// convivir con una VM de verdad en el mismo proceso.
int khv_vsock_prueba_reuso(int *coincide_fd) {
  if (!activo) vsock_init(53);
  struct prueba_dial a = {.ret = 1};
  pthread_t t;
  if (pthread_create(&t, NULL, prueba_marcar, &a) != 0) return -EAGAIN;
  struct vconn *c = NULL;
  for (int i = 0; i < 2000 && !c; i++) {
    pthread_mutex_lock(&mu);
    for (int j = 0; j < VS_MAXCONN && !c; j++)
      if (conns[j].estado == C_CONECTANDO && conns[j].gport == PRUEBA_PUERTO) c = &conns[j];
    if (!c) {
      pthread_mutex_unlock(&mu);
      usleep(1000);
    }
  }
  if (!c) {
    pthread_join(t, NULL);
    return -EBUSY;
  }
  // Con mu desde el hallazgo: A no puede despertar hasta soltarlo.
  int fd_a = c->fd;
  liberar(c);
  int sv[2], err = 0;
  uint32_t gen;
  struct vconn *b = reservar(PRUEBA_PUERTO, sv, &gen, &err);
  if (!b) {
    pthread_mutex_unlock(&mu);
    pthread_join(t, NULL);
    return err;
  }
  *coincide_fd = b == c && sv[0] == fd_a;
  b->estado = C_ABIERTA;  // el OP_RESPONSE del invitado para B
  b->p_request = false;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  pthread_join(t, NULL);
  pthread_mutex_lock(&mu);
  bool tocada = b->gen != gen || b->estado != C_ABIERTA || b->p_rst;
  liberar(b);
  pthread_mutex_unlock(&mu);
  close(sv[1]);
  if (a.ret >= 0) {
    close(a.ret);
    return 1;
  }
  return tocada ? -EPROTO : a.ret;
}

// khv_vsock_prueba_cierre reproduce la fuga de un slot: el invitado ya dijo
// que no recibe más (SHUT_RCV) y aquí ya no hay nada que leer ni escribir,
// así que la conexión no entraba en el poll; Go cierra su extremo y nadie se
// entera. medio: Go solo cierra su escritura (CloseWrite), que NO debe
// cerrar la conexión. Devuelve 1 si, tras cerrar Go, una pasada dejó pedido
// el cierre completo (SEND|RCV) al invitado, 0 si no, negativo si no se pudo
// preparar o si la conexión no quedó en kq (-ETIME: sin la sonda de cada
// segundo nadie la habría mirado) o se pidió el cierre antes de que Go
// cerrara (-EPROTO). Como en la VM, la conexión ya está vigilada cuando Go
// cierra: el aviso llega por kq, no al apuntarla.
int khv_vsock_prueba_cierre(int medio) {
  if (!activo) vsock_init(53);
  pthread_mutex_lock(&mu);
  int sv[2], err = 0;
  uint32_t gen;
  struct vconn *c = reservar(PRUEBA_PUERTO + 1, sv, &gen, &err);
  if (!c) {
    pthread_mutex_unlock(&mu);
    return err ? err : -EBUSY;
  }
  c->estado = C_ABIERTA;
  c->p_request = false;
  c->guest_sordo = true;
  c->host_eof = true;
  struct pollfd pf[VS_MAXCONN];
  int idx[VS_MAXCONN];
  bool listo[VS_MAXCONN];
  int plazo = -1;
  // Primera pasada con los dos extremos abiertos: la apunta en kq, que no
  // debe dar el cierre por hecho.
  int n = armar_poll(pf, idx, &plazo);
  if (n > 0) poll(pf, (nfds_t)n, 0);
  tras_poll(pf, idx, n, listo);
  int pedido = -ETIME;
  if (c->vigilada && plazo < 0) {
    pedido = (c->p_shut & SHUT_SEND_F) && (c->p_shut & SHUT_RCV_F) ? -EPROTO : 0;
    if (medio) shutdown(sv[1], SHUT_WR);
    else close(sv[1]);
    // El aviso lo encola el kernel al cerrar; se espera en kq como vs_hilo,
    // con un plazo (corto con medio, que no debe avisar de cierre).
    for (int i = 0; pedido == 0 && i < 10; i++) {
      struct pollfd pk = {.fd = kq, .events = POLLIN};
      poll(&pk, 1, medio ? 10 : 100);
      plazo = -1;
      n = armar_poll(pf, idx, &plazo);
      if (n > 0) poll(pf, (nfds_t)n, 0);
      tras_poll(pf, idx, n, listo);
      if ((c->p_shut & SHUT_SEND_F) && (c->p_shut & SHUT_RCV_F)) pedido = 1;
    }
  } else if (medio) {
    shutdown(sv[1], SHUT_WR);
  } else {
    close(sv[1]);
  }
  liberar(c);
  pthread_mutex_unlock(&mu);
  if (medio) close(sv[1]);
  return pedido;
}

struct prueba_plazo {
  uint32_t puerto;
  int ret;
};

static void *prueba_marcar_plazo(void *arg) {
  struct prueba_plazo *p = arg;
  p->ret = khv_vsock_dial(p->puerto, 100);
  return NULL;
}

// khv_vsock_prueba_plazo reproduce un dial que vence sin que el invitado
// reponga búferes de rx (no hay invitado: rx_poner siempre falla). enviada:
// la petición ya había salido (se simula quitando p_request), así que el RST
// tiene que quedar en la cola sin slot. Devuelve 1 si el dial dio -ETIMEDOUT,
// el slot quedó libre y el RST está (o no) en la cola según enviada; 0 si el
// slot sigue ocupado, -EPROTO si el RST no es el esperado y otro negativo si
// no se pudo preparar. No puede convivir con una VM de verdad en el proceso.
int khv_vsock_prueba_plazo(int enviada) {
  if (!activo) vsock_init(53);
  struct prueba_plazo p = {.puerto = PRUEBA_PUERTO + 2 + (enviada ? 1 : 0), .ret = 1};
  pthread_mutex_lock(&mu);
  n_rst = 0;
  pthread_mutex_unlock(&mu);
  pthread_t t;
  if (pthread_create(&t, NULL, prueba_marcar_plazo, &p) != 0) return -EAGAIN;
  struct vconn *c = NULL;
  for (int i = 0; i < 2000 && !c; i++) {
    pthread_mutex_lock(&mu);
    for (int j = 0; j < VS_MAXCONN && !c; j++)
      if (conns[j].estado == C_CONECTANDO && conns[j].gport == p.puerto) c = &conns[j];
    if (!c) {
      pthread_mutex_unlock(&mu);
      usleep(1000);
    }
  }
  if (!c) {
    pthread_join(t, NULL);
    return -EBUSY;
  }
  uint32_t lport = c->lport;
  if (enviada) c->p_request = false;  // como si bombear ya la hubiera puesto
  pthread_mutex_unlock(&mu);
  pthread_join(t, NULL);
  if (p.ret >= 0) {
    close(p.ret);
    return -EISCONN;
  }
  if (p.ret != -ETIMEDOUT) return p.ret;
  pthread_mutex_lock(&mu);
  int r = 1;
  for (int j = 0; j < VS_MAXCONN; j++)
    if (conns[j].estado != C_LIBRE && conns[j].gport == p.puerto) r = 0;
  bool hay_rst = false;
  for (int j = 0; j < n_rst; j++)
    if (rst_cola[j].src == lport && rst_cola[j].dst == p.puerto) hay_rst = true;
  if (r == 1 && hay_rst != (enviada != 0)) r = -EPROTO;
  n_rst = 0;
  pthread_mutex_unlock(&mu);
  return r;
}

#define PRUEBA_TX 6

static void prueba_w(uint64_t off, uint64_t v) { vsock_mmio(off, true, &v, 4); }

// khv_vsock_prueba_tx manda, como el invitado, PRUEBA_TX paquetes RW por la
// cola de tx a una conexión abierta, cada uno con la cabecera y los datos en
// tres búferes: uno de 4000 bytes y los demás de VS_PAYLOAD. Pasan del
// socket (VS_BUF) y Go no lee todavía, así que el resto va a pend y lo tiene
// que vaciar vs_hilo cuando Go lea: si el aviso de que hay pend no lo
// despertara, se quedaría en poll sin mirar POLLOUT. enviado trae los
// 4000 + (PRUEBA_TX - 1) * VS_PAYLOAD bytes; lee desde Go (el otro extremo)
// hasta out (cap bytes) y devuelve cuántos llegaron en plazo, o negativo si
// no se pudo preparar; *uso_pend dice si algo tuvo que esperar en pend. Con
// su propia RAM y sin pausa, y lo deja todo como estaba. No puede convivir
// con una VM de verdad.
int khv_vsock_prueba_tx(uint8_t *out, uint32_t cap, const uint8_t *enviado, int *uso_pend) {
  *uso_pend = 0;
  if (!activo) vsock_init(53);
  const uint64_t tam = 1 << 20;
  uint8_t *ram = mmap(NULL, tam, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
  if (ram == MAP_FAILED) return -ENOMEM;
  pthread_mutex_lock(&mu);
  uint8_t *ram_antes = g_ram;
  uint64_t tam_antes = g_ram_size;
  bool pausa_antes = g_pausa;
  g_ram = ram;
  g_ram_size = tam;
  g_pausa = false;  // sin VM empieza en pausa: vs_hilo no haría nada
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  // La cola de tx (1): tabla en +0, avail en +0x1000, used en +0x2000.
  prueba_w(0x070, 0);
  prueba_w(0x030, 1);
  prueba_w(0x038, 8);
  prueba_w(0x080, (uint32_t)RAM_BASE);
  prueba_w(0x090, (uint32_t)(RAM_BASE + 0x1000));
  prueba_w(0x0a0, (uint32_t)(RAM_BASE + 0x2000));
  prueba_w(0x084, 0);
  prueba_w(0x094, 0);
  prueba_w(0x0a4, 0);
  prueba_w(0x044, 1);
  prueba_w(0x070, 0xf);

  pthread_mutex_lock(&mu);
  int sv[2], err = 0;
  uint32_t gen;
  struct vconn *c = reservar(PRUEBA_PUERTO + 5, sv, &gen, &err);
  if (c) {
    c->estado = C_ABIERTA;
    c->p_request = false;
  }
  uint32_t lport = c ? c->lport : 0;
  pthread_mutex_unlock(&mu);
  int ret = c ? 0 : (err ? err : -EBUSY);
  if (c) {
    fcntl(sv[1], F_SETFL, O_NONBLOCK);
    struct vdesc *d = (struct vdesc *)(ram);
    uint16_t *av = (uint16_t *)(ram + 0x1000);
    uint64_t o = 0;
    if (4000 + (PRUEBA_TX - 1) * VS_PAYLOAD > cap) ret = -E2BIG;
    for (int p = 0; p < PRUEBA_TX && ret == 0; p++) {
      uint32_t len = p == 0 ? 4000 : VS_PAYLOAD;
      struct vs_hdr h = {.src_cid = VS_GUEST_CID, .dst_cid = VS_HOST_CID, .src_port = PRUEBA_PUERTO + 5,
                         .dst_port = lport, .len = len, .type = TYPE_STREAM, .op = OP_RW,
                         .buf_alloc = VS_BUF};
      // tx copia o escribe los datos antes de volver del notify: la RAM se
      // puede reusar en el paquete siguiente.
      uint64_t cab = 0x3000;
      memcpy(ram + cab, &h, VS_HDR);
      // Dos trozos de datos, con un hueco entre ellos.
      uint64_t datos = 0x10000;
      uint32_t a = len / 4, b = len - a;
      memcpy(ram + datos, enviado + o, a);
      memcpy(ram + datos + a + 0x800, enviado + o + a, b);
      uint16_t base = (uint16_t)(p % 2 * 3);
      d[base] = (struct vdesc){RAM_BASE + cab, VS_HDR, VIRTQ_DESC_F_NEXT, (uint16_t)(base + 1)};
      d[base + 1] = (struct vdesc){RAM_BASE + datos, a, VIRTQ_DESC_F_NEXT, (uint16_t)(base + 2)};
      d[base + 2] = (struct vdesc){RAM_BASE + datos + a + 0x800, b, 0, 0};
      o += len;
      av[2 + p % 8] = base;
      __atomic_store_n(&av[1], (uint16_t)(p + 1), __ATOMIC_RELEASE);
      prueba_w(0x050, 1);  // notify de la cola de tx
    }
    pthread_mutex_lock(&mu);
    *uso_pend = c->gen == gen && c->npend > 0;  // Go aún no lee: nadie la vacía
    pthread_mutex_unlock(&mu);
    // Go lee: lo que no cupo sale de pend por el hilo.
    uint32_t leido = 0;
    for (int i = 0; ret == 0 && leido < o && i < 200; i++) {
      ssize_t r = read(sv[1], out + leido, cap - leido);
      if (r > 0) leido += (uint32_t)r;
      else {
        struct pollfd pf = {.fd = sv[1], .events = POLLIN};
        poll(&pf, 1, 10);
      }
    }
    if (ret == 0) ret = (int)leido;
    pthread_mutex_lock(&mu);
    if (c->gen == gen) liberar(c);
    pthread_mutex_unlock(&mu);
    close(sv[1]);
  }
  prueba_w(0x070, 0);
  pthread_mutex_lock(&mu);
  g_ram = ram_antes;
  g_ram_size = tam_antes;
  g_pausa = pausa_antes;
  pthread_mutex_unlock(&mu);
  munmap(ram, tam);
  return ret;
}

// khv_vsock_prueba_poll: un poll que falla con EAGAIN (khv_poll) no puede
// matar a vs_hilo. 0 si el hilo sigue dando vueltas después.
int khv_vsock_prueba_poll(void) {
  pthread_mutex_lock(&mu);
  bool pausa_antes = g_pausa;
  g_pausa = false;  // sin VM empieza en pausa: vs_hilo no daría vueltas
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  unsigned antes = __atomic_load_n(&vs_vueltas, __ATOMIC_ACQUIRE);
  __atomic_store_n(&khv_poll_fallos[KHV_POLL_VSOCK], 1, __ATOMIC_RELEASE);
  despertar_hilo();
  // El fallo se gasta en un poll; luego otro aviso tiene que dar otra vuelta.
  int r = -1;
  for (int i = 0; i < 1000 && r; i++) {
    if (!__atomic_load_n(&khv_poll_fallos[KHV_POLL_VSOCK], __ATOMIC_ACQUIRE) &&
        __atomic_load_n(&vs_vueltas, __ATOMIC_ACQUIRE) - antes >= 3)
      r = 0;
    else {
      despertar_hilo();
      usleep(1000);
    }
  }
  __atomic_store_n(&khv_poll_fallos[KHV_POLL_VSOCK], 0, __ATOMIC_RELEASE);
  pthread_mutex_lock(&mu);
  g_pausa = pausa_antes;
  pthread_mutex_unlock(&mu);
  return r;
}

// khv_vsock_prueba_tanda_rx: con una conexión que tiene en su socket varios
// MiB del host, crédito de sobra y 256 búferes de rx del invitado, una vuelta
// de bombear mete como mucho VS_TANDA_MAX (más un paquete), o un solo paquete
// si la VM está en pausa. *lleno dice cuánto había en el socket, *movido
// cuánto entró en esa vuelta y *ns lo que tuvo mu. 0, o negativo si no se
// pudo preparar. Con su propia RAM, y lo deja todo como estaba. No puede
// convivir con una VM.
#define PRUEBA_RX_N 256
#define PRUEBA_RX_BUF (VS_HDR + VS_PAYLOAD)
int khv_vsock_prueba_tanda_rx(bool pausa, uint64_t *lleno, uint64_t *movido, int64_t *ns) {
  *lleno = *movido = 0;
  *ns = -1;
  if (!activo) vsock_init(53);
  const uint64_t base = 0x10000, tam = base + (uint64_t)PRUEBA_RX_N * PRUEBA_RX_BUF;
  uint8_t *ram = mmap(NULL, tam, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
  if (ram == MAP_FAILED) return -ENOMEM;
  // Los búferes de rx (cola 0): tabla en +0, avail en +0x1000, used en
  // +0x2000, y los datos detrás.
  struct vdesc *d = (struct vdesc *)ram;
  uint16_t *av = (uint16_t *)(ram + 0x1000);
  for (int i = 0; i < PRUEBA_RX_N; i++) {
    d[i] = (struct vdesc){RAM_BASE + base + (uint64_t)i * PRUEBA_RX_BUF, PRUEBA_RX_BUF, VIRTQ_DESC_F_WRITE, 0};
    av[2 + i] = (uint16_t)i;
  }
  av[1] = PRUEBA_RX_N;
  pthread_mutex_lock(&mu);
  uint8_t *ram_antes = g_ram;
  uint64_t tam_antes = g_ram_size;
  bool pausa_antes = g_pausa;
  g_ram = ram;
  g_ram_size = tam;
  g_pausa = true;  // vs_hilo no bombea mientras se prepara
  pthread_mutex_unlock(&mu);
  prueba_w(0x070, 0);
  prueba_w(0x030, 0);
  prueba_w(0x038, PRUEBA_RX_N);
  prueba_w(0x080, (uint32_t)RAM_BASE);
  prueba_w(0x090, (uint32_t)(RAM_BASE + 0x1000));
  prueba_w(0x0a0, (uint32_t)(RAM_BASE + 0x2000));
  prueba_w(0x084, 0);
  prueba_w(0x094, 0);
  prueba_w(0x0a4, 0);
  prueba_w(0x044, 1);
  prueba_w(0x070, 0xf);

  pthread_mutex_lock(&mu);
  int sv[2], err = 0;
  uint32_t gen;
  struct vconn *c = reservar(PRUEBA_PUERTO + 8, sv, &gen, &err);
  int ret = c ? 0 : (err ? err : -EBUSY);
  if (c) {
    c->estado = C_ABIERTA;
    c->p_request = false;
    c->peer_buf = 64u << 20;
    // El host llena el socket, con mu tomado: vs_hilo no lo vacía antes.
    int grande = 4 << 20;
    setsockopt(sv[0], SOL_SOCKET, SO_RCVBUF, &grande, sizeof grande);
    setsockopt(sv[1], SOL_SOCKET, SO_SNDBUF, &grande, sizeof grande);
    fcntl(sv[1], F_SETFL, O_NONBLOCK);
    static uint8_t trozo[64 * 1024];
    for (;;) {
      ssize_t w = write(sv[1], trozo, sizeof trozo);
      if (w <= 0) break;
      *lleno += (uint64_t)w;
    }
    g_pausa = pausa;
    bool listo[VS_MAXCONN] = {0};
    listo[c - conns] = true;
    uint32_t antes = c->tx_cnt;
    uint64_t t0 = clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
    bombear(listo);
    *ns = (int64_t)(clock_gettime_nsec_np(CLOCK_UPTIME_RAW) - t0);
    *movido = c->tx_cnt - antes;
    g_pausa = true;
    liberar(c);
    close(sv[1]);
  }
  pthread_mutex_unlock(&mu);
  prueba_w(0x070, 0);
  pthread_mutex_lock(&mu);
  g_ram = ram_antes;
  g_ram_size = tam_antes;
  g_pausa = pausa_antes;
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  munmap(ram, tam);
  return ret;
}

// khv_vsock_prueba_muerto: un poll que falla sin remedio (EBADF) termina
// vs_hilo, y el vsock se da por muerto: un dial devuelve -ENODEV al momento
// (lo que tarda, en *dial_ns), no tras su plazo. Luego vsock_init vuelve a
// arrancar el hilo. 0 si cuadra; 1 si el fallo no llegó a inyectarse; 2 si
// el dial no dio -ENODEV; 3 si el hilo no volvió a dar vueltas. No puede
// convivir con una VM.
int khv_vsock_prueba_muerto(int64_t *dial_ns) {
  *dial_ns = -1;
  if (!activo) vsock_init(53);
  pthread_mutex_lock(&mu);
  bool pausa_antes = g_pausa;
  g_pausa = false;  // sin VM empieza en pausa: vs_hilo no daría vueltas
  pthread_cond_broadcast(&cv);
  pthread_mutex_unlock(&mu);
  __atomic_store_n(&khv_poll_errno[KHV_POLL_VSOCK], EBADF, __ATOMIC_RELEASE);
  __atomic_store_n(&khv_poll_fallos[KHV_POLL_VSOCK], 1, __ATOMIC_RELEASE);
  int r = 1;
  for (int i = 0; i < 1000 && r; i++) {
    if (!__atomic_load_n(&khv_poll_fallos[KHV_POLL_VSOCK], __ATOMIC_ACQUIRE)) r = 0;
    else {
      despertar_hilo();
      usleep(1000);
    }
  }
  __atomic_store_n(&khv_poll_fallos[KHV_POLL_VSOCK], 0, __ATOMIC_RELEASE);
  __atomic_store_n(&khv_poll_errno[KHV_POLL_VSOCK], 0, __ATOMIC_RELEASE);
  if (r == 0) {
    usleep(10000);  // que el hilo acabe de salir
    uint64_t t0 = clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
    int fd = khv_vsock_dial(PRUEBA_PUERTO + 7, 300);
    *dial_ns = (int64_t)(clock_gettime_nsec_np(CLOCK_UPTIME_RAW) - t0);
    if (fd != -ENODEV) r = 2;
    if (fd >= 0) close(fd);
  }
  vsock_init(53);
  unsigned antes = __atomic_load_n(&vs_vueltas, __ATOMIC_ACQUIRE);
  bool vuelve = false;
  for (int i = 0; i < 1000 && !vuelve; i++) {
    despertar_hilo();
    usleep(1000);
    vuelve = __atomic_load_n(&vs_vueltas, __ATOMIC_ACQUIRE) - antes >= 2;
  }
  if (r == 0 && !vuelve) r = 3;
  pthread_mutex_lock(&mu);
  g_pausa = pausa_antes;
  pthread_mutex_unlock(&mu);
  return r;
}

// khv_vsock_prueba_fd_en_poll: liberar una conexión cuyo fd espera vs_hilo
// en poll (sin plazo) no lo cierra desde este hilo, que podría quedarse en
// close para siempre (cerrar_fd): lo cierra el hilo al salir de poll, y Go ve
// el fin. 0 si cuadra; 1 si el fd ya estaba cerrado al volver de liberar; 2
// si Go no vio el fin en 2 s; 3 si el hilo no llegó a esperar el fd en poll;
// otro negativo si no se pudo preparar. No puede convivir con una VM.
int khv_vsock_prueba_fd_en_poll(void) {
  if (!activo) vsock_init(53);
  pthread_mutex_lock(&mu);
  bool pausa_antes = g_pausa;
  g_pausa = false;  // sin VM empieza en pausa
  pthread_cond_broadcast(&cv);
  int sv[2], err = 0;
  uint32_t gen;
  struct vconn *c = reservar(PRUEBA_PUERTO + 6, sv, &gen, &err);
  if (!c) {
    g_pausa = pausa_antes;
    pthread_mutex_unlock(&mu);
    return err ? err : -EBUSY;
  }
  c->estado = C_ABIERTA;
  c->p_request = false;
  c->peer_buf = VS_BUF;  // con crédito: el hilo espera POLLIN en su fd
  unsigned antes = __atomic_load_n(&vs_vueltas, __ATOMIC_ACQUIRE);
  despertar_hilo();
  pthread_mutex_unlock(&mu);
  // Dos vueltas después del aviso ya armó poll con la conexión (la primera
  // pudo armarse antes); en_poll dice que sigue dentro. Hasta entonces se le
  // avisa en cada intento, o se quedaría en poll sin dar la segunda.
  int r = 3;
  for (int i = 0; i < 2000 && r == 3; i++) {
    pthread_mutex_lock(&mu);
    if (en_poll > 0 && __atomic_load_n(&vs_vueltas, __ATOMIC_ACQUIRE) - antes >= 2) r = 0;
    else pthread_mutex_unlock(&mu);
    if (r == 3) {
      despertar_hilo();
      usleep(1000);
    }
  }
  if (r == 0) {  // con mu
    struct stat st0, st1;
    int fd = c->fd;
    bool abierto = fstat(fd, &st0) == 0;
    liberar(c);
    if (!abierto || fstat(fd, &st1) != 0 || st1.st_ino != st0.st_ino) r = 1;
    pthread_mutex_unlock(&mu);
  } else {
    pthread_mutex_lock(&mu);
    if (c->gen == gen) liberar(c);
    pthread_mutex_unlock(&mu);
  }
  if (r == 0) {
    fcntl(sv[1], F_SETFL, O_NONBLOCK);
    r = 2;
    for (int i = 0; i < 200 && r == 2; i++) {
      char b;
      if (read(sv[1], &b, 1) == 0) r = 0;
      else {
        struct pollfd pf = {.fd = sv[1], .events = POLLIN};
        poll(&pf, 1, 10);
      }
    }
  }
  close(sv[1]);
  pthread_mutex_lock(&mu);
  g_pausa = pausa_antes;
  pthread_mutex_unlock(&mu);
  return r;
}
