# Salida filtrada por nombre: un proxy SNI passthrough (diseño)

> **Estado: diseño, sin implementar.** Hoy `-egress allowlist` filtra por IP. Este
> documento propone un modo opcional que filtra por el nombre del servidor TLS sin
> descifrar nada. El límite actual y cómo mitigarlo ya están en
> [`SECURITY.md`](../SECURITY.md), «Lo que NO está resuelto».

## El problema

`allowlist` siembra en el ipset (Linux, `internal/net/dnsresolver.go`) o en el conjunto
de `kling-vz` (macOS, `vz/internal/egress`) las IPs que resuelve un dominio permitido.
La decisión se toma por dirección: una conexión a esa IP sale, lleve el SNI que lleve.
Con un dominio servido desde un CDN o un hosting compartido, la misma IP atiende a
miles de sitios, y el invitado puede hablar con uno suyo en ese borde. Desde
`multiinquilino.go` la CLI avisa de los sufijos conocidos (`github.io`,
`amazonaws.com`…), pero no puede ver qué dominio propio vive detrás de un CDN.

## La idea

Mismo patrón que el proxy de credenciales (`internal/net/credproxy.go`), sin su TLS:

1. **El resolver miente a propósito.** En el modo nuevo, la consulta A de un dominio
   permitido se contesta con la IP del lado host del veth (la `credIP` de hoy) en vez
   de con las reales, y nada entra en el ipset. El invitado no conoce ninguna IP
   externa a la que ir directamente; una conexión a una IP que no sea la del host cae
   en el DROP de siempre.
2. **Un proxy en el host escucha en esa IP, puerto 443.** Lee el ClientHello sin
   contestarlo (`peek`, como mucho 16 KiB y 2 s), extrae el SNI y aplica la misma regla
   que `isAllowed` (igualdad o subdominio). Si no casa, cierra; si casa, resuelve él
   el nombre con el resolver del host, descarta las IPs que `isBlockedIP` ya rechaza
   (privadas, enlace local, metadatos) y abre la conexión al destino. Desde ahí copia
   bytes en los dos sentidos (`io.Copy`/`splice`), empezando por el ClientHello que
   leyó. Ni CA propia, ni certificados, ni descifrado: el invitado ve el certificado
   real del servidor y el proxy solo ve metadatos.
3. **Se elige por máquina.** Un valor nuevo de egress, `allowlist-sni`, para que una
   allowlist de hoy no cambie de comportamiento. Con él, `-allow dominio` admite
   también `dominio:puerto` para TLS fuera del 443 (8443, por ejemplo).

En macOS el sitio natural es `kling-vz`: ya termina cada flujo TCP del invitado en su
pila gVisor, así que puede leer el ClientHello en el reenviador sin DNAT ni listener
aparte. En Linux, una regla DNAT del namespace manda `HostIP:443` al proxy, que puede
vivir en el mismo proceso que el de credenciales; las dos funciones comparten el
resolver y el registro de auditoría (`kling machine audit` enseñaría el SNI rechazado).

## Lo que no es TLS sobre TCP

- **HTTP plano (80).** Bloqueado por defecto en este modo. Una opción `-allow-http`
  podría pasarlo leyendo la cabecera `Host` de cada petición HTTP/1.x de la conexión
  (no solo de la primera: con keep-alive la segunda puede ir a otro sitio), y cerrando
  ante HTTP/2 en claro o un `CONNECT`. Es más superficie de parseo; mejor no tenerlo si
  no hace falta.
- **QUIC / HTTP/3 (UDP 443).** Bloqueado. El SNI va en el paquete Initial, cifrado con
  claves que se derivan del Destination Connection ID y por tanto descifrable sin
  secretos, pero un proxy QUIC es mucho más código que un `peek` de TCP. Los clientes
  que lo intentan vuelven solos a TCP cuando UDP no contesta.
- **Otros puertos TCP sin TLS** (SSH, Postgres sin TLS…): DROP, como hoy con un destino
  no listado. Para bases de datos ya está el proxy de credenciales de `kling db`.

## ECH (Encrypted Client Hello)

Con ECH el SNI real viaja cifrado y el ClientHello exterior lleva el `public_name` del
proveedor. Tres medidas, de menos a más estricta:

- El resolver del modo nuevo no contesta registros **HTTPS/SVCB** (tipos 65 y 64), que
  es donde un cliente honesto encuentra la configuración ECH. Sin ella el cliente no
  usa ECH y el SNI exterior es el real.
- Un invitado hostil puede traer su propia configuración ECH y poner en el exterior un
  SNI permitido mientras el interior apunta a su sitio en el mismo proveedor. El proxy
  no lo distingue de un ECH «GREASE» (de relleno, que algunos navegadores mandan
  siempre): por diseño son indistinguibles. Por eso el modo **rechaza todo
  ClientHello con la extensión `encrypted_client_hello` (0xfe0d)**. Rompe a un
  Chromium que mande GREASE ECH; curl, Go, Python, Node y OpenSSL no lo mandan por
  defecto, y son lo que corre en una microVM de agente.
- Quitar la extensión no es una salida: el ClientHello entra en el transcript del
  handshake y el servidor detectaría el cambio.

## Lo que sigue abierto con este modo

- **Domain fronting.** El SNI permitido llega al CDN, y dentro del TLS el `Host` pide
  otro sitio. Varios CDN grandes (Cloudflare, Fastly, CloudFront) dicen rechazar ya ese
  desajuste SNI/`Host` entre clientes distintos, pero no todos los proveedores, y el
  proxy no lo puede ver sin MITM. Para una API con
  clave, la credencial (que sí termina TLS y mira el `Host`) sigue siendo la respuesta.
- **Un sufijo multiinquilino permitido** (`-allow github.io`) sigue dejando entrar a
  todos sus inquilinos: el SNI casa. El aviso de la CLI sigue haciendo falta; con SNI,
  en cambio, `-allow juan.github.io` sí queda en ese sitio.
- **El túnel por DNS** de un dominio permitido no cambia (SECURITY.md).

## Coste y pruebas

Cada conexión TLS pasa por el host: un salto de copia más y dos descriptores por
conexión. Hay que medir la latencia del primer byte y el rendimiento antes de
recomendarlo por defecto. Pruebas mínimas: SNI permitido
pasa y llega el certificado real; SNI no listado cierra; IP directa (sin pasar por el
resolver) no sale; ClientHello con 0xfe0d cierra; ClientHello partido en varios
segmentos TCP se lee entero; un nombre permitido que resuelve a una IP privada no se
conecta; UDP 443 no sale.
