# Conductor de contextos de código del gateway de kindling (Python).
#
# Corre DENTRO del sandbox, con el usuario del inquilino, y solo usa la
# biblioteca estándar de Python 3. Dos modos:
#
#   conductor.py serve SOCKET
#       Escucha en un socket Unix y ejecuta cada bloque de código que llega en
#       un dict de globales propio (el estado persiste entre ejecuciones). Una
#       ejecución a la vez; las demás esperan en la cola del socket.
#   conductor.py run SOCKET PLAZO
#       Cliente de una línea: lee el código de stdin, lo manda al socket y
#       copia a stdout las líneas JSON que recibe (stdout, stderr, result,
#       error, done).
#
# Protocolo del socket: el cliente manda "PLAZO\n" (segundos, 0 = sin plazo),
# el código, y cierra su mitad de escritura. El conductor contesta con líneas
# JSON {"type": ...} y cierra.
import ast
import base64
import builtins
import io
import json
import os
import signal
import socket
import sys
import time
import traceback

ESPERA_LECTURA = 10.0  # un cliente que conecta y calla no bloquea al conductor
ESPERA_ESCRITURA = 30.0  # ni uno que no lee lo que se le manda
CONEXION = 5.0  # cuánto reintenta conectar el cliente (el conductor arranca)


class Abortar(BaseException):
    """El cliente se fue: se corta la ejecución sin tocar el estado."""


class Plazo(BaseException):
    """Vencido el plazo de la ejecución. BaseException para que un
    `except Exception` del código del usuario no se lo trague."""


class Cuenta:
    """Contador de ejecuciones del contexto (execution_count)."""
    n = 0
    ejecutando = False


def al_interrumpir(signum, frame):
    """SIGINT (DELETE .../execution): KeyboardInterrupt en la ejecución en
    curso; sin ninguna, se ignora."""
    if Cuenta.ejecutando:
        raise KeyboardInterrupt()


def resultados(valor):
    """Representaciones del valor por tipo MIME: text/plain siempre; text/html
    e image/png si el valor sabe darlas (_repr_html_, _repr_png_) o es una
    figura de matplotlib. Una representación que falla se omite."""
    res = {"text/plain": repr(valor)}
    try:
        f = getattr(valor, "_repr_html_", None)
        if callable(f):
            h = f()
            if isinstance(h, str):
                res["text/html"] = h
    except Exception:
        pass
    try:
        png = None
        f = getattr(valor, "_repr_png_", None)
        if callable(f):
            png = f()
        elif "matplotlib.figure" in sys.modules and isinstance(valor, sys.modules["matplotlib.figure"].Figure):
            b = io.BytesIO()
            valor.savefig(b, format="png")
            png = b.getvalue()
        if isinstance(png, (bytes, bytearray)) and png:
            res["image/png"] = base64.b64encode(bytes(png)).decode()
    except Exception:
        pass
    return res


class Emisor:
    def __init__(self, conn):
        self.conn = conn

    def evento(self, **ev):
        data = (json.dumps(ev) + "\n").encode()
        try:
            self.conn.sendall(data)
        except OSError:
            raise Abortar()


class Salida(io.TextIOBase):
    """Sustituye a sys.stdout/sys.stderr: agrupa las escrituras y las manda
    por líneas, o cada 100 ms si no hay salto de línea."""

    def __init__(self, emisor, flujo):
        self.emisor = emisor
        self.flujo = flujo
        self.buf = []
        self.ultimo = time.monotonic()

    def writable(self):
        return True

    def write(self, s):
        if not isinstance(s, str):
            raise TypeError("write() argument must be str, not %s" % type(s).__name__)
        if s:
            self.buf.append(s)
            ahora = time.monotonic()
            if "\n" in s or ahora - self.ultimo > 0.1 or len(self.buf) > 256:
                self.flush()
        return len(s)

    def flush(self):
        if self.buf:
            texto = "".join(self.buf)
            self.buf = []
            self.emisor.evento(type=self.flujo, text=texto)
        self.ultimo = time.monotonic()


def al_vencer(signum, frame):
    raise Plazo()


def traza(exc):
    tb = exc.__traceback__
    # Fuera los marcos del propio conductor: al usuario le importa su código.
    while tb is not None and tb.tb_frame.f_code.co_filename == __file__:
        tb = tb.tb_next
    return [l.rstrip("\n") for l in traceback.format_exception(type(exc), exc, tb)]


def ejecutar(globales, codigo, plazo, emisor):
    """Corre codigo en globales. Devuelve True si terminó sin error."""
    out = Salida(emisor, "stdout")
    err = Salida(emisor, "stderr")
    viejos = sys.stdout, sys.stderr, sys.stdin
    sys.stdout, sys.stderr, sys.stdin = out, err, io.StringIO("")
    ok = True
    Cuenta.n += 1
    Cuenta.ejecutando = True
    try:
        if plazo > 0:
            signal.setitimer(signal.ITIMER_REAL, plazo)
        try:
            arbol = ast.parse(codigo, "<codigo>", "exec")
            ultima = None
            if arbol.body and isinstance(arbol.body[-1], ast.Expr):
                ultima = ast.Expression(arbol.body.pop().value)
            exec(compile(arbol, "<codigo>", "exec"), globales)
            if ultima is not None:
                valor = eval(compile(ultima, "<codigo>", "eval"), globales)
                if valor is not None:
                    globales["_"] = valor
                    out.flush()
                    emisor.evento(type="result", text=repr(valor), results=resultados(valor),
                                  execution_count=Cuenta.n)
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            Cuenta.ejecutando = False
    except Abortar:
        raise
    except Plazo:
        ok = False
        emisor.evento(type="error", ename="TimeoutError",
                      evalue="execution timed out after %gs" % plazo, traceback=[])
    except SystemExit as e:
        ok = False
        emisor.evento(type="error", ename="SystemExit", evalue=str(e.code), traceback=[])
    except BaseException as e:  # noqa: B902 - el código del usuario puede lanzar cualquier cosa
        ok = False
        try:
            out.flush()
            err.flush()
        except Abortar:
            raise
        emisor.evento(type="error", ename=type(e).__name__, evalue=str(e), traceback=traza(e))
    finally:
        sys.stdout, sys.stderr, sys.stdin = viejos
    out.flush()
    err.flush()
    return ok


def leer_peticion(conn):
    conn.settimeout(ESPERA_LECTURA)
    datos = b""
    while True:
        trozo = conn.recv(65536)
        if not trozo:
            break
        datos += trozo
    cabecera, _, resto = datos.partition(b"\n")
    return float(cabecera.decode() or 0), resto.decode("utf-8", "replace")


def servir(ruta):
    signal.signal(signal.SIGALRM, al_vencer)
    signal.signal(signal.SIGINT, al_interrumpir)
    try:
        os.unlink(ruta)
    except FileNotFoundError:
        pass
    os.umask(0o077)
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(ruta)
    os.chmod(ruta, 0o600)
    srv.listen(8)
    globales = {"__name__": "__main__", "__builtins__": builtins}
    while True:
        conn, _ = srv.accept()
        with conn:
            try:
                plazo, codigo = leer_peticion(conn)
            except (OSError, ValueError):
                continue
            conn.settimeout(ESPERA_ESCRITURA)
            emisor = Emisor(conn)
            t0 = time.monotonic()
            try:
                ok = ejecutar(globales, codigo, plazo, emisor)
                emisor.evento(type="done", ok=ok, duration_ms=int((time.monotonic() - t0) * 1000),
                              execution_count=Cuenta.n)
            except Abortar:
                pass


def cliente(ruta, plazo):
    codigo = sys.stdin.buffer.read()
    limite = time.monotonic() + CONEXION
    while True:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            s.connect(ruta)
            break
        except OSError as e:
            s.close()
            if time.monotonic() > limite:
                print(json.dumps({"type": "error", "ename": "ContextError",
                                  "evalue": "the context is not answering: %s" % e,
                                  "traceback": []}), flush=True)
                return 2
            time.sleep(0.05)
    out = sys.stdout.buffer
    try:
        s.sendall(("%s\n" % plazo).encode() + codigo)
        s.shutdown(socket.SHUT_WR)
        pendiente = b""
        visto = False
        while True:
            trozo = s.recv(65536)
            if not trozo:
                break
            out.write(trozo)
            out.flush()
            pendiente += trozo
            *lineas, pendiente = pendiente.split(b"\n")
            for l in lineas:
                if b'"done"' in l:
                    try:
                        visto = visto or json.loads(l).get("type") == "done"
                    except ValueError:
                        pass
        if not visto:
            print(json.dumps({"type": "error", "ename": "ContextError",
                              "evalue": "the context ended the execution without finishing it",
                              "traceback": []}), flush=True)
            return 3
    except OSError as e:
        print(json.dumps({"type": "error", "ename": "ContextError", "evalue": str(e),
                          "traceback": []}), flush=True)
        return 3
    finally:
        s.close()
    return 0


def main(argv):
    if len(argv) == 3 and argv[1] == "serve":
        servir(argv[2])
        return 0
    if len(argv) == 4 and argv[1] == "run":
        return cliente(argv[2], argv[3])
    sys.stderr.write("usage: conductor.py serve SOCKET | run SOCKET TIMEOUT\n")
    return 64


if __name__ == "__main__":
    sys.exit(main(sys.argv))
