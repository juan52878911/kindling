//go:build darwin && arm64

// Trazas de kling-hv: intervalos de os_signpost por fase (crear la VM, cargar
// el kernel, restaurar, freeze) e hitos (el primer hv_vcpu_run, la primera
// escritura en la UART, el primer acceso a virtio, la primera respuesta del
// agente por vsock). Se ven con Instruments o con
//
//   xctrace record --template 'os_signpost' --attach <pid de kling-hv>
//
// (ver docs/backend-hv.md). Sin nadie trazando, os_signpost_enabled es falso y
// solo queda la marca de tiempo de cada fase (unas pocas por arranque); con
// KLING_HV_TRACE=1, además, una línea en la consola de kling-hv con el
// desglose al responder el agente (arranque o restauración) y otra tras cada
// freeze.
#include <libproc.h>
#include <mach/mach_time.h>
#include <os/log.h>
#include <os/signpost.h>
#include <stdarg.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/time.h>
#include <unistd.h>

#include "khv.h"

static os_log_t tlog;
static bool resumen;  // KLING_HV_TRACE=1
static bool restaura;
static mach_timebase_info_data_t ttb;
static uint64_t t0;          // mach_absolute_time de khv_create
static double proceso_ms = -1;  // de que arrancó el proceso a khv_create
// Por fase: inicio y duración (unidades de mach_absolute_time; inicio 0 = no
// pasó). Las escribe el hilo que hace la fase y las lee quien escribe el
// resumen después de que pasen: sin cerrojo, como t_primer_run de vm.c.
static uint64_t f_ini[KHV_F_N], f_dur[KHV_F_N];
static uint64_t h_t[KHV_H_N];
volatile uint8_t khv_hitos_vistos[KHV_H_N];

static double a_ms(uint64_t d) { return (double)d * ttb.numer / ttb.denom / 1e6; }

void khv_traza_iniciar(uint64_t t_inicio) {
  mach_timebase_info(&ttb);
  t0 = t_inicio;
  tlog = os_log_create("dev.kindling.hv", "fases");
  const char *e = getenv("KLING_HV_TRACE");
  resumen = e && *e && strcmp(e, "0") != 0;
  // El arranque del proceso (reloj de pared, en µs) frente a ahora: lo que se
  // fue en lanzar kling-hv, el runtime de Go, el confinamiento y el API
  // antes de crear la VM.
  struct proc_bsdinfo bi;
  struct timeval ahora;
  if (proc_pidinfo(getpid(), PROC_PIDTBSDINFO, 0, &bi, sizeof bi) == (int)sizeof bi && gettimeofday(&ahora, NULL) == 0) {
    double d = (double)((int64_t)ahora.tv_sec - (int64_t)bi.pbi_start_tvsec) * 1e3 +
               (double)((int64_t)ahora.tv_usec - (int64_t)bi.pbi_start_tvusec) / 1e3;
    if (d >= 0) proceso_ms = d;
  }
}

void khv_traza_restaurando(void) { restaura = true; }

bool khv_traza_activa(void) { return tlog && os_signpost_enabled(tlog); }

uint64_t khv_traza_empieza(int fase) {
  uint64_t t = mach_absolute_time();
  if (fase < 0 || fase >= KHV_F_N) return t;
  f_ini[fase] = t;
  if (tlog && os_signpost_enabled(tlog)) {
    switch (fase) {
#define KHV_EMPIEZA_(e, s) \
  case e: os_signpost_interval_begin(tlog, OS_SIGNPOST_ID_EXCLUSIVE, s); break;
      KHV_FASES(KHV_EMPIEZA_)
#undef KHV_EMPIEZA_
    }
  }
  return t;
}

void khv_traza_acaba(int fase, uint64_t t) {
  if (fase < 0 || fase >= KHV_F_N) return;
  f_dur[fase] = mach_absolute_time() - t;
  if (tlog && os_signpost_enabled(tlog)) {
    switch (fase) {
#define KHV_ACABA_(e, s) \
  case e: os_signpost_interval_end(tlog, OS_SIGNPOST_ID_EXCLUSIVE, s); break;
      KHV_FASES(KHV_ACABA_)
#undef KHV_ACABA_
    }
  }
}

void khv_traza_hito_(int h) {
  if (h < 0 || h >= KHV_H_N) return;
  // Dos hilos a la vez (dos vCPU en su primer acceso a virtio): se queda el
  // primero.
  uint8_t no = 0;
  if (!__atomic_compare_exchange_n(&khv_hitos_vistos[h], &no, 1, false, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE)) return;
  h_t[h] = mach_absolute_time();
  if (tlog && os_signpost_enabled(tlog)) {
    switch (h) {
#define KHV_HITO_(e, s) \
  case e: os_signpost_event_emit(tlog, OS_SIGNPOST_ID_EXCLUSIVE, s); break;
      KHV_HITOS(KHV_HITO_)
#undef KHV_HITO_
    }
  }
  if (h == KHV_H_AGENTE && resumen) {
    char b[768];
    khv_traza_resumen(b, sizeof b);
    khv_log("%s", b);
  }
}

void khv_traza_salida(int cpu, unsigned ec, uint64_t ipa, uint64_t ns) {
  if (!tlog) return;
  os_signpost_event_emit(tlog, OS_SIGNPOST_ID_EXCLUSIVE, "salida-cara", "cpu=%d ec=%#x ipa=%#llx us=%llu", cpu, ec,
                         (unsigned long long)ipa, (unsigned long long)(ns / 1000));
}

static const char *const nombres_f[KHV_F_N] = {
#define KHV_NOMBRE_(e, s) s,
    KHV_FASES(KHV_NOMBRE_)
#undef KHV_NOMBRE_
};

static size_t pon(char *b, size_t n, size_t o, const char *fmt, ...) __attribute__((format(printf, 4, 5)));
static size_t pon(char *b, size_t n, size_t o, const char *fmt, ...) {
  if (o >= n) return o;
  va_list ap;
  va_start(ap, fmt);
  int r = vsnprintf(b + o, n - o, fmt, ap);
  va_end(ap);
  return r < 0 ? o : o + (size_t)r;
}

// Las fases de antes de soltar la VM, por orden; «espera» es lo que no cae en
// ninguna (Go y el API entre khv_create y el InstanceStart o el resume). Tras
// soltar, los hitos desde ese momento, y el total de khv_create al agente.
void khv_traza_resumen(char *b, size_t n) {
  if (n == 0) return;
  b[0] = 0;
  size_t o = pon(b, n, 0, "trace %s ms:", restaura ? "restore" : "boot");
  if (proceso_ms >= 0) o = pon(b, n, o, " process %.1f |", proceso_ms);
  uint64_t soltar = h_t[KHV_H_SOLTAR];
  uint64_t hecho = 0;
  bool usada[KHV_F_N] = {0};
  for (;;) {
    int k = -1;
    for (int i = 0; i < KHV_F_N; i++)
      if (!usada[i] && f_ini[i] && i != KHV_F_CPU && i < KHV_F_PAUSA && (!soltar || f_ini[i] <= soltar) &&
          (k < 0 || f_ini[i] < f_ini[k]))
        k = i;
    if (k < 0) break;
    usada[k] = true;
    o = pon(b, n, o, " %s %.2f", nombres_f[k], a_ms(f_dur[k]));
    hecho += f_dur[k];
  }
  if (soltar) {
    uint64_t pre = soltar - t0;
    o = pon(b, n, o, " wait %.2f |", pre > hecho ? a_ms(pre - hecho) : 0.0);
    if (f_ini[KHV_F_CPU]) o = pon(b, n, o, " %s %.2f", nombres_f[KHV_F_CPU], a_ms(f_dur[KHV_F_CPU]));
    static const char *const nombres_h[KHV_H_N] = {
#define KHV_NOMBRE_(e, s) s,
        KHV_HITOS(KHV_NOMBRE_)
#undef KHV_NOMBRE_
    };
    for (int i = KHV_H_SOLTAR + 1; i < KHV_H_N; i++)
      if (h_t[i] >= soltar) o = pon(b, n, o, " ->%s %.2f", nombres_h[i], a_ms(h_t[i] - soltar));
    if (h_t[KHV_H_AGENTE]) o = pon(b, n, o, " | total %.2f", a_ms(h_t[KHV_H_AGENTE] - t0));
  }
  (void)o;
}

void khv_traza_freeze(void) {
  if (!resumen) return;
  char b[512];
  size_t o = pon(b, sizeof b, 0, "trace freeze ms:");
  for (int i = KHV_F_PAUSA; i < KHV_F_N; i++)
    if (f_ini[i]) o = pon(b, sizeof b, o, " %s %.2f", nombres_f[i], a_ms(f_dur[i]));
  khv_log("%s", b);
}

void khv_traza_prueba(char *buf, size_t n) {
  memset(f_ini, 0, sizeof f_ini);
  memset(f_dur, 0, sizeof f_dur);
  memset(h_t, 0, sizeof h_t);
  memset((void *)khv_hitos_vistos, 0, sizeof khv_hitos_vistos);
  restaura = false;
  bool r = resumen;
  resumen = false;  // sin línea en la consola
  khv_traza_iniciar(mach_absolute_time());
  resumen = false;
  for (int f = KHV_F_RAM; f <= KHV_F_VCPUS; f++) khv_traza_acaba(f, khv_traza_empieza(f));
  khv_traza_hito(KHV_H_SOLTAR);
  khv_traza_hito(KHV_H_PRIMER_RUN);
  khv_traza_hito(KHV_H_AGENTE);
  khv_traza_hito(KHV_H_AGENTE);  // la segunda no cuenta
  khv_traza_resumen(buf, n);
  resumen = r;
}
