// Cadenas de descriptores como iovec, para que la red y el vsock lean y
// escriban la RAM del invitado directamente con recvmsg/readv/sendmsg, sin
// copiar cada paquete a un búfer intermedio ni recorrer la cadena una vez
// para medirla y otra para llenarla (vq_capacidad + vq_escribir en virtio.c).
// Solo funciones estáticas: lo incluyen net.c y vsock.c.
#ifndef KHV_VQ_IOV_H
#define KHV_VQ_IOV_H

#include <string.h>
#include <sys/uio.h>

#include "khv.h"

// vq_iov recorre una sola vez la cadena de head y pone en iov los búferes de
// escritura (escritura = true: los que llena el dispositivo) o los de lectura
// (false), en orden, con su suma en *total. Los mismos topes que vq_cadena:
// fuera de la tabla, más larga que la cola (un ciclo), indirecta o con un
// búfer que se sale de la RAM, la cadena no vale y devuelve -1; también si
// tiene más de max búferes del sentido pedido. Cada descriptor se lee una
// sola vez: el invitado puede cambiarlo mientras tanto.
static inline int vq_iov(const struct vq *q, const struct vdesc *desc, uint16_t head, bool escritura,
                         struct iovec *iov, int max, uint64_t *total) {
  int n = 0;
  uint64_t suma = 0;
  uint16_t i = head;
  for (uint32_t vistos = 0;; vistos++) {
    if (i >= q->num || vistos >= q->num) return -1;
    struct vdesc d;
    memcpy(&d, (const void *)&desc[i], sizeof d);
    if (d.flags & VIRTQ_DESC_F_INDIRECT) return -1;
    if (((d.flags & VIRTQ_DESC_F_WRITE) != 0) == escritura && d.len > 0) {
      void *p = gpa_ptr(d.addr, d.len);
      if (!p || n >= max) return -1;
      iov[n].iov_base = p;
      iov[n].iov_len = d.len;
      n++;
      suma += d.len;
    }
    if (!(d.flags & VIRTQ_DESC_F_NEXT)) break;
    i = d.next;
  }
  *total = suma;
  return n;
}

// iov_tramo pone en dst (con sitio para n) el tramo de src que empieza en el
// byte salto y mide como mucho tope bytes. Devuelve cuántos iovec usó.
static inline int iov_tramo(const struct iovec *src, int n, size_t salto, size_t tope, struct iovec *dst) {
  int k = 0;
  for (int j = 0; j < n && tope > 0; j++) {
    size_t l = src[j].iov_len;
    if (salto >= l) {
      salto -= l;
      continue;
    }
    size_t d = l - salto < tope ? l - salto : tope;
    dst[k].iov_base = (uint8_t *)src[j].iov_base + salto;
    dst[k].iov_len = d;
    k++;
    tope -= d;
    salto = 0;
  }
  return k;
}

// iov_poner copia len bytes de src al principio de iov (que ya se sabe que
// caben).
static inline void iov_poner(const struct iovec *iov, int n, const void *src, size_t len) {
  const uint8_t *s = src;
  for (int j = 0; j < n && len > 0; j++) {
    size_t d = iov[j].iov_len < len ? iov[j].iov_len : len;
    memcpy(iov[j].iov_base, s, d);
    s += d;
    len -= d;
  }
}

#endif
