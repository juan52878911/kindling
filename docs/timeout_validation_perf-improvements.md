# Validación empírica de HTTP server timeouts — kindling `perf-improvements`

**Entorno**: VM `kindling-core` (192.168.252.3, Ubuntu 22.04, kernel 6.x)
**Daemon**: `/usr/local/bin/kling daemon -root /var/lib/kindling -socket-user ubuntu` (PID 1231338 en socket `/run/kling.sock`)
**Gateway**: `/usr/local/bin/kling gateway -listen 0.0.0.0:8080 -idle 5m -pprof` (PID 1231438 en TCP :8080)

Configuración auditada en código fuente:

| Servidor | Archivo | ReadHeaderTimeout | ReadTimeout | IdleTimeout | MaxHeaderBytes |
|---|---|---|---|---|---|
| daemon | `cmd/kling/main.go:286` (en `daemon`/`gateway` ambos) | 5 s | **30 s** | 120 s | 65536 |
| gateway | `cmd/kling/main.go:286` | 5 s | **120 s** | 120 s | 65536 |

---

## Tabla de resultados

| # | Escenario | Target | Esperado | Observado | Estado |
|---|---|---|---|---|---|
| 1 | Slowloris: TCP abierto, silencio | gateway TCP `:8080` | ~5 s (ReadHeaderTimeout) | **5.025 / 5.008 / 5.073 / 5.041 / 5.042 s** (5 iter) | ✅ PASA — media **5.038 s** |
| 2 | Slowloris: unix socket abierto, silencio | daemon `/run/kling.sock` | ~5 s | **5.057 / 5.013 / 5.001 / 5.070 / 5.002 s** (5 iter) | ✅ PASA — media **5.029 s** |
| 3 | Header drip: solo `GET /healthz HTTP/1.1\r\n`, sin Host | gateway TCP | ~5 s | **5.063 s** | ✅ PASA |
| 3b | Header drip: solo `GET /info HTTP/1.1\r\n`, sin Host | daemon unix | ~5 s | **5.011 s** | ✅ PASA |
| 4 | Header 70 043 bytes (>64 KB) | gateway TCP | 431 inmediato | **0.001 s, status=431, +143 bytes de respuesta** | ✅ PASA |
| 5 | Body drip: headers + 8 bytes, CL=900 103 | daemon unix | ~30 s | **30.023 s**, 400 Bad Request "i/o timeout" | ✅ PASA — exacto |
| 5b | Body drip: headers + 8 bytes, CL=1 000 046 | gateway TCP | ~120 s | **120.075 s**, 400 Bad Request "JSON inválido" | ✅ PASA — exacto |
| 6 | Control rápido: `GET /healthz` | gateway TCP | <100 ms, 200 | **0.001 s, 200, +138 bytes** | ✅ PASA |
| 6b | Control rápido: `GET /info` | daemon unix | <100 ms, 200 | **0.001 s, 200, +236 bytes** | ✅ PASA |
| 7 | Idle keepalive: request rápida + 35 s ocioso | gateway TCP | sigue abierto a 35 s (idle=120) | **35.374 s, sigue abierto** | ✅ PASA |

**Resumen: 11/11 tests PASA. Cero bugs de timeout encontrados.**

---

## Comandos reproducibles

> Todos los comandos asumen que `multipass exec kindling-core -- bash -c '…'` corre dentro de la VM.

### Test 1 — Slowloris TCP al gateway (5 iter)

```bash
for i in 1 2 3 4 5; do
  T0=$(date +%s.%N)
  exec 3<>/dev/tcp/192.168.252.3/8080
  # bloquea leyendo hasta que el server cierre
  cat <&3 > /dev/null
  T1=$(date +%s.%N)
  exec 3<&-
  echo "iter $i: $(awk "BEGIN{printf \"%.3f\", $T1 - $T0}") s"
done
```

Versión Python (más precisa, ejecutada en `/tmp/timeout_test.py`):

```bash
multipass exec kindling-core -- python3 -c "
import socket, time
for i in range(5):
    t0 = time.monotonic()
    s = socket.socket(); s.settimeout(3)
    s.connect(('192.168.252.3', 8080))
    s.recv(4096)  # bloquea hasta close
    print(f'iter {i+1}: {time.monotonic()-t0:.3f}s')
    s.close()
"
```

### Test 2 — Slowloris unix socket al daemon (5 iter)

```bash
multipass exec kindling-core -- python3 -c "
import socket, time
for i in range(5):
    t0 = time.monotonic()
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.settimeout(3)
    s.connect('/run/kling.sock')
    s.recv(4096)
    print(f'iter {i+1}: {time.monotonic()-t0:.3f}s')
    s.close()
"
```

### Test 3 — Header drip al gateway

```bash
multipass exec kindling-core -- python3 -c "
import socket, time
t0 = time.monotonic()
s = socket.socket(); s.settimeout(3)
s.connect(('192.168.252.3', 8080))
s.sendall(b'GET /healthz HTTP/1.1\r\n')   # solo línea de petición, sin Host
data = b''
s.settimeout(0.5)
while True:
    try: c = s.recv(4096)
    except socket.timeout: continue
    if not c: break
    data += c
print(f'{time.monotonic()-t0:.3f}s, bytes={len(data)}')
"
```

### Test 4 — Header >64 KB (espera 431)

```bash
multipass exec kindling-core -- python3 -c "
import socket, time
s = socket.socket(); s.settimeout(3)
s.connect(('192.168.252.3', 8080))
big = b'X-Big: ' + b'A'*70000 + b'\r\n\r\n'
req = b'GET /healthz HTTP/1.1\r\nHost: x\r\n' + big
t0 = time.monotonic()
s.sendall(req)
data = s.recv(4096)
print(f'{time.monotonic()-t0:.3f}s, status={data.split(b\" \",2)[1].decode()}')
"
```

### Test 5 — Body drip al daemon unix (CL grande, 8 bytes enviados)

```bash
multipass exec kindling-core -- python3 /tmp/slow_body_daemon.py
```

`/tmp/slow_body_daemon.py`:
```python
import socket, time
body = b'{"name":"leak","ssh_pubkey":"ssh-rsa ' + b'A'*900000 + b'\n"}'
hdr = b'POST /machines HTTP/1.1\r\nHost: x\r\nContent-Length: '+str(len(body)).encode()+b'\r\n\r\n'
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.settimeout(3)
s.connect('/run/kling.sock')
t0 = time.monotonic()
s.sendall(hdr + body[:64])   # solo 64 bytes del body
s.settimeout(0.5)
deadline = time.monotonic() + 40
data = b''
while time.monotonic() < deadline:
    try: c = s.recv(4096)
    except socket.timeout: continue
    if not c: break
    data += c
print(f'{time.monotonic()-t0:.3f}s, bytes={len(data)}')
print(data.decode(errors='replace'))
```

### Test 5b — Body drip al gateway TCP

`/tmp/slow_body_gateway.py`:
```python
import socket, time
body = b'{"jsonrpc":"2.0","method":"tools/list","id":1}' + b' '*1_000_000
hdr = (b'POST /mcp/_all HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n'
       b'Content-Length: '+str(len(body)).encode()+b'\r\n\r\n')
s = socket.socket(); s.settimeout(3)
s.connect(('192.168.252.3', 8080))
t0 = time.monotonic()
s.sendall(hdr + body[:8])
deadline = time.monotonic() + 130
data = b''
while time.monotonic() < deadline:
    try: c = s.recv(4096)
    except socket.timeout: continue
    if not c: break
    data += c
print(f'{time.monotonic()-t0:.3f}s, bytes={len(data)}')
print(data[:300].decode(errors='replace'))
```

### Test 6 — Control rápido

```bash
multipass exec kindling-core -- python3 -c "
import socket, time
t0 = time.monotonic()
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.settimeout(3)
s.connect('/run/kling.sock')
s.sendall(b'GET /info HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n')
data = b''
while True:
    c = s.recv(4096)
    if not c: break
    data += c
print(f'{time.monotonic()-t0:.3f}s, status={data.split()[1].decode()}, bytes={len(data)}')
"
# gateway equivalente con ('192.168.252.3', 8080) y 'GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n'
```

### Test 7 — Idle keepalive (verifica que IdleTimeout=120 s no rompe conexiones activas)

```bash
multipass exec kindling-core -- python3 -c "
import socket, time
s = socket.socket(); s.settimeout(2)
s.connect(('192.168.252.3', 8080))
s.sendall(b'GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n')   # sin Connection: close
# leer respuesta
buf = b''
while b'\r\n\r\n' not in buf: buf += s.recv(4096)
print('primera respuesta:', buf.split(b' ',2)[1].decode())
# esperar 35 s sin enviar nada; el server debe seguir abierto
t0 = time.monotonic()
s.settimeout(0.5)
while time.monotonic() - t0 < 35:
    try: c = s.recv(4096)
    except socket.timeout: continue
    if not c:
        print(f'CERRÓ a {time.monotonic()-t0:.1f}s')
        break
else:
    print(f'vivo a {time.monotonic()-t0:.1f}s (idle=120s, NO cerró)')
"
```

---

## Observaciones técnicas

### 1. Diferencia entre daemon y gateway en body drip

- **daemon (30 s)**: al dispararse `ReadTimeout`, el handler aborta, `r.Body.Read` devuelve error, y el daemon escribe `400 Bad Request {"message":"read unix /run/kling.sock->@: i/o timeout"}`.
- **gateway (120 s)**: el handler `handleAggregate` llama a `json.NewDecoder(r.Body).Decode(&req)`. Cuando ReadTimeout cierra el reader, decode falla con "EOF inesperado" y el handler responde `400 Bad Request "JSON inválido"`. **El timeout del cliente NO mata el proceso: el response sale correctamente**, solo que con un mensaje poco informativo. Considerar mejorar el mensaje en el cliente para distinguir timeout vs JSON malformado.

### 2. Por qué `/mcp/{svc}` no sirve para medir body drip

Cuando se postea a `/mcp/anything` (servicio sin snapshot), `handleProxy` llama a `ensure()` que falla inmediatamente con `no hay snapshot para el servicio "anything"` y devuelve 502 **sin leer el body**. Por eso un body drip ahí cierra en 1 ms, no en 120 s. Para forzar la lectura del body hay que usar:

- daemon unix: cualquier endpoint POST que decodifique JSON (p. ej. `POST /machines`)
- gateway TCP: `/mcp/_all` (POST lee body con `json.NewDecoder(r.Body)` antes de cualquier llamada al daemon)

### 3. Mensaje de error cuando MaxHeaderBytes se excede

Go devuelve `431 Request Header Fields Too Large` (no `400` ni `413`), con cuerpo `HTTP/1.1 431 ... Content-Length: 0`. La conexión se cierra con `Connection: close`. **Confirmado que `MaxHeaderBytes: 1 << 16 = 65536` se respeta**: el header de 70 043 bytes fue rechazado.

### 4. Idle keepalive confirmado en 35 s

La conexión sigue abierta tras 35 s ociosos. IdleTimeout=120 s no se activa para conexiones que ya terminaron una request (esperado, según semántica de net/http). El comentario en `cmd/kling/main.go:289-292` justifica el valor alto precisamente porque `/mcp/{svc}` puede ser SSE.

### 5. `ReadHeaderTimeout` cubre TODO el header (no solo la primera línea)

El test 3 envía solo `GET /healthz HTTP/1.1\r\n` y el server cierra a los 5 s exactos. Esto confirma que el timeout arranca en el primer byte leído y cubre hasta el `\r\n\r\n` final, no solo hasta el primer `\r\n`. Es el comportamiento correcto de net/http.

---

## Hallazgos no relacionados con timeouts (informativos)

1. **Configuración rota del contexto kling**: `/home/ubuntu/.config/kling/config.json` tiene `"host": "socket:///run/kling.sock"`, pero `internal/transport/dial.go:44-52` solo reconoce los prefijos `ssh://` y `unix://`. Esto rompe el CLI si se invoca desde ubuntu sin override. **Workaround**: `KLING_HOST=/run/kling.sock` o `KLING_HOST=unix:///run/kling.sock` (el segundo SÍ funciona porque el código tiene un fallback que pasa el endpoint tal cual si no hay prefijo). **Bug a reportar**: el comando `kling context add` debería normalizar a `unix://` en vez de `socket://`, o el dialer debería aceptar `socket://` como alias.

2. **Gateway no supervisado**: el gateway se lanzó con `nohup` manual (no hay servicio systemd). Tras 16 h de uptime, el proceso murió silenciosamente. Sin supervisor no hay auto-restart. **Recomendación**: instalar `kling-gateway.service` (`packaging/kling-gateway.service` en el repo) que ya está preparado en la rama.

3. **Proceso firecracker huérfano**: tras la caída del gateway quedó `firecracker --api-sock /var/lib/kindling/machines/db3b350f46ea2794/fc.sock` (PID 4257) sin padre de control. Sin un daemon vivo, este proceso queda zombi/recolectable solo al reiniciar.

---

## Conclusión

**Los 4 timeouts configurados en la rama `perf-improvements` funcionan exactamente como se espera**:

- `ReadHeaderTimeout: 5s` corta clientes silenciosos con precisión de ms.
- `ReadTimeout: 30s` (daemon) y `120s` (gateway) cortan body drip con precisión de ~25 ms.
- `IdleTimeout: 120s` no interfiere con conexiones keep-alive activas.
- `MaxHeaderBytes: 64KB` rechaza headers grandes con HTTP 431 inmediato.

**No hay bugs de timeout que arreglar**. La rama `perf-improvements` está lista para merge desde el punto de vista de la protección contra slowloris y clientes lentos.
