//go:build darwin && arm64

// Dispositivos que no son virtio: la UART 16550 (la consola ttyS0 que kindling
// ya usa con Firecracker) y el reloj PL031 (el mismo RTC que da Firecracker en
// arm64: sin él el invitado de un arranque en frío nace en 1970).
#include <string.h>
#include <time.h>
#include <unistd.h>

#include "khv.h"

// ===================== UART 16550 =====================

// La salida va por líneas al fd de consola (una tubería que Go copia a stdout,
// donde se mezcla con los diagnósticos de kling-hv sin partir líneas). Una
// línea a medias (el prompt de un shell) sale al llenarse el búfer, en la
// siguiente pausa o al terminar.
static pthread_mutex_t u_mu = PTHREAD_MUTEX_INITIALIZER;
static struct uart_state u;
static uint32_t u_intid;
static int u_fd = -1;
static char u_buf[512];
static size_t u_n;

void uart_init(uint32_t intid, int fd) {
  u_intid = intid;
  u_fd = fd;
  memset(&u, 0, sizeof u);
}

static void u_vaciar(void) {
  size_t o = 0;
  while (u_fd >= 0 && o < u_n) {
    ssize_t w = write(u_fd, u_buf + o, u_n - o);
    if (w <= 0) break;  // sin lector: la consola se pierde, la VM sigue
    o += (size_t)w;
  }
  u_n = 0;
}

void uart_flush(void) {
  pthread_mutex_lock(&u_mu);
  u_vaciar();
  pthread_mutex_unlock(&u_mu);
}

static void u_irq(void) {
  uint8_t n = (u.ier & 0x02) && u.thr_pend;  // sin entrada: solo THRI
  if (n != u.nivel) {
    u.nivel = n;
    hv_gic_set_spi(u_intid, n);
  }
}

static void u_out(uint8_t c) {
  u_buf[u_n++] = (char)c;
  if (c == '\n' || u_n == sizeof u_buf) u_vaciar();
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
      case 0: r = dlab ? u.dll : 0; break;
      case 1: r = dlab ? u.dlm : u.ier; break;
      case 2:
        r = (u.fcr & 1) ? 0xc0 : 0;
        if ((u.ier & 0x02) && u.thr_pend) {
          r |= 0x02;
          u.thr_pend = 0;  // leer IIR con THRI la reconoce
        } else r |= 0x01;
        break;
      case 3: r = u.lcr; break;
      case 4: r = u.mcr; break;
      case 5: r = 0x60; break;  // THRE|TEMT
      case 6: r = (u.mcr & 0x10) ? ((u.mcr & 0x0f) << 4) : 0xb0; break;
      case 7: r = u.scr; break;
    }
    *val = r;
  }
  u_irq();
  pthread_mutex_unlock(&u_mu);
  return true;
}

void uart_get(struct uart_state *s) {
  pthread_mutex_lock(&u_mu);
  *s = u;
  pthread_mutex_unlock(&u_mu);
}

void uart_reafirmar(void) {
  pthread_mutex_lock(&u_mu);
  hv_gic_set_spi(u_intid, u.nivel);
  pthread_mutex_unlock(&u_mu);
}

void uart_set(const struct uart_state *s) {
  pthread_mutex_lock(&u_mu);
  u = *s;
  pthread_mutex_unlock(&u_mu);
}

// ===================== RTC PL031 =====================

// DR es la hora del host más offset (LR la fija). La alarma no interrumpe
// nunca: el kernel no la usa en kindling.
static pthread_mutex_t r_mu = PTHREAD_MUTEX_INITIALIZER;
static struct rtc_state rtc;

void rtc_init(void) {
  memset(&rtc, 0, sizeof rtc);
  rtc.cr = 1;
}

static uint32_t rtc_ahora(void) { return (uint32_t)((int64_t)time(NULL) + rtc.offset); }

bool rtc_mmio(uint64_t off, bool escribir, uint64_t *val, int size) {
  // Identificación AMBA (PrimeCell): periphid 0x00041031, cellid 0xb105f00d.
  static const uint8_t id[8] = {0x31, 0x10, 0x04, 0x00, 0x0d, 0xf0, 0x05, 0xb1};
  pthread_mutex_lock(&r_mu);
  uint32_t v = (uint32_t)*val;
  if (escribir) {
    switch (off) {
      case 0x004: rtc.mr = v; break;
      case 0x008: rtc.offset = (int64_t)v - (int64_t)time(NULL); break;
      case 0x00c: rtc.cr = 1; break;  // no se puede parar
      case 0x010: rtc.imsc = v & 1; break;
      case 0x01c: rtc.ris &= ~v; break;
    }
  } else {
    uint64_t r = 0;
    if (off >= 0xfe0 && off < 0x1000) r = id[(off - 0xfe0) / 4];
    else
      switch (off) {
        case 0x000: r = rtc_ahora(); break;
        case 0x004: r = rtc.mr; break;
        case 0x008: r = rtc_ahora(); break;
        case 0x00c: r = rtc.cr; break;
        case 0x010: r = rtc.imsc; break;
        case 0x014: r = rtc.ris; break;
        case 0x018: r = rtc.ris & rtc.imsc; break;
      }
    *val = r;
  }
  pthread_mutex_unlock(&r_mu);
  return true;
}

void rtc_get(struct rtc_state *s) {
  pthread_mutex_lock(&r_mu);
  *s = rtc;
  pthread_mutex_unlock(&r_mu);
}

void rtc_set(const struct rtc_state *s) {
  pthread_mutex_lock(&r_mu);
  rtc = *s;
  pthread_mutex_unlock(&r_mu);
}
