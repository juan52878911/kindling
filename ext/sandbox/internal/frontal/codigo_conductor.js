// Conductor de contextos de código del gateway de kindling (JavaScript).
//
// Corre DENTRO del sandbox, con el usuario del inquilino, y solo usa los
// módulos de node. Mismo protocolo que codigo_conductor.py:
//
//   conductor.js serve SOCKET   escucha en un socket Unix; cada bloque de
//       código corre en un contexto vm propio (let/const/var/funciones
//       persisten entre ejecuciones). Una ejecución a la vez.
//   conductor.js run SOCKET PLAZO   cliente de una línea: lee el código de
//       stdin, lo manda al socket y copia a stdout las líneas JSON recibidas.
//
// Socket: el cliente manda "PLAZO\n" (segundos, 0 = sin plazo), el código y
// cierra su mitad de escritura; el conductor contesta líneas JSON y cierra.
'use strict';
const net = require('net');
const vm = require('vm');
const fs = require('fs');
const util = require('util');

const ESPERA_LECTURA = 10000; // un cliente que conecta y calla no bloquea

function evento(conn, ev) {
  if (!conn.destroyed) conn.write(JSON.stringify(ev) + '\n');
}

// resultados: text/plain siempre; text/html e image/png si el valor sabe
// darlos (_repr_html_, _repr_png_), como el conductor de Python.
function resultados(valor) {
  const res = { 'text/plain': util.inspect(valor) };
  try {
    if (valor && typeof valor._repr_html_ === 'function') {
      const h = valor._repr_html_();
      if (typeof h === 'string') res['text/html'] = h;
    }
    if (valor && typeof valor._repr_png_ === 'function') {
      const p = valor._repr_png_();
      if (Buffer.isBuffer(p) || p instanceof Uint8Array) res['image/png'] = Buffer.from(p).toString('base64');
      else if (typeof p === 'string' && p) res['image/png'] = p;
    }
  } catch (e) { /* una representación que falla se omite */ }
  return res;
}

function servir(ruta) {
  let cuenta = 0;
  let actual = null; // la conexión de la ejecución en curso
  let ejecutando = false;
  let interrumpir = null;
  const fmt = (...a) => util.format(...a) + '\n';
  const consola = {
    log: (...a) => salida('stdout', fmt(...a)),
    info: (...a) => salida('stdout', fmt(...a)),
    debug: (...a) => salida('stdout', fmt(...a)),
    error: (...a) => salida('stderr', fmt(...a)),
    warn: (...a) => salida('stderr', fmt(...a)),
  };
  function salida(flujo, texto) {
    if (actual) evento(actual, { type: flujo, text: texto });
  }
  const proceso = Object.create(process);
  for (const [nombre, flujo] of [['stdout', 'stdout'], ['stderr', 'stderr']]) {
    Object.defineProperty(proceso, nombre, {
      value: { write: (t) => { salida(flujo, String(t)); return true; }, isTTY: false },
    });
  }
  const ctx = vm.createContext({
    console: consola, process: proceso, require, Buffer, URL, TextEncoder, TextDecoder,
    setTimeout, clearTimeout, setInterval, clearInterval, setImmediate, clearImmediate,
    queueMicrotask, structuredClone,
  });

  process.on('SIGINT', () => {
    // Sin ejecución, nada que interrumpir. Con una asíncrona, se rechaza su
    // espera; una síncrona la corta breakOnSigint de vm.
    if (ejecutando && interrumpir) interrumpir();
  });

  // Cola: una ejecución a la vez.
  let cola = Promise.resolve();
  const srv = net.createServer({ allowHalfOpen: true }, (conn) => {
    conn.on('error', () => {});
    const trozos = [];
    const plazoLectura = setTimeout(() => conn.destroy(), ESPERA_LECTURA);
    conn.on('data', (d) => trozos.push(d));
    conn.on('end', () => {
      clearTimeout(plazoLectura);
      const datos = Buffer.concat(trozos).toString('utf8');
      const i = datos.indexOf('\n');
      const plazo = parseFloat(datos.slice(0, i) || '0');
      if (Number.isNaN(plazo)) return conn.destroy();
      cola = cola.then(() => ejecutar(conn, datos.slice(i + 1), plazo)).catch(() => {});
    });
  });
  async function ejecutar(conn, codigo, plazo) {
    const t0 = Date.now();
    let ok = true;
    actual = conn;
    ejecutando = true;
    cuenta++;
    const n = cuenta;
    try {
      let valor = vm.runInContext(codigo, ctx, {
        filename: '<codigo>', timeout: plazo > 0 ? Math.ceil(plazo * 1000) : undefined,
        breakOnSigint: true,
      });
      if (valor && typeof valor.then === 'function') {
        // Una promesa se espera, con el mismo plazo y la misma interrupción.
        let temporizador;
        const carreras = [valor, new Promise((_, rechaza) => {
          interrumpir = () => rechaza(Object.assign(new Error('Script execution was interrupted by SIGINT'), { code: 'ERR_SCRIPT_EXECUTION_INTERRUPTED' }));
        })];
        if (plazo > 0) {
          carreras.push(new Promise((_, rechaza) => {
            temporizador = setTimeout(() => rechaza(Object.assign(new Error('timeout'), { code: 'ERR_SCRIPT_EXECUTION_TIMEOUT' })), plazo * 1000);
          }));
        }
        try { valor = await Promise.race(carreras); } finally { clearTimeout(temporizador); }
      }
      if (valor !== undefined) {
        evento(conn, { type: 'result', text: util.inspect(valor), results: resultados(valor), execution_count: n });
      }
    } catch (e) {
      ok = false;
      if (e && e.code === 'ERR_SCRIPT_EXECUTION_TIMEOUT') {
        evento(conn, { type: 'error', ename: 'TimeoutError', evalue: `execution timed out after ${plazo}s`, traceback: [] });
      } else if (e && e.code === 'ERR_SCRIPT_EXECUTION_INTERRUPTED') {
        evento(conn, { type: 'error', ename: 'KeyboardInterrupt', evalue: 'execution interrupted', traceback: [] });
      } else {
        // Un error lanzado dentro del vm es de otro reino: no es instanceof Error.
        const err = e && typeof e === 'object' && 'message' in e ? e : new Error(util.inspect(e));
        evento(conn, {
          type: 'error', ename: err.name || 'Error', evalue: String(err.message),
          traceback: String(err.stack || '').split('\n').filter((l) => !l.includes(__filename)),
        });
      }
    } finally {
      ejecutando = false;
      interrumpir = null;
      actual = null;
    }
    evento(conn, { type: 'done', ok, duration_ms: Date.now() - t0, execution_count: n });
    conn.end();
  }
  try { fs.unlinkSync(ruta); } catch (e) { /* no existía */ }
  process.umask(0o077);
  srv.listen(ruta, () => fs.chmodSync(ruta, 0o600));
}

function cliente(ruta, plazo) {
  const codigo = fs.readFileSync(0);
  const limite = Date.now() + 5000;
  const error = (msg) => process.stdout.write(JSON.stringify({ type: 'error', ename: 'ContextError', evalue: msg, traceback: [] }) + '\n');
  let visto = false;
  function conectar() {
    const s = net.createConnection(ruta);
    s.on('error', (e) => {
      if (!s.connected && Date.now() < limite) { s.reintento = true; s.destroy(); return setTimeout(conectar, 50); }
      error(s.connected ? String(e.message) : `the context is not answering: ${e.message}`);
      process.exit(s.connected ? 3 : 2);
    });
    s.on('connect', () => {
      s.connected = true;
      s.write(`${plazo}\n`);
      s.end(codigo);
    });
    let pendiente = '';
    s.on('data', (d) => {
      process.stdout.write(d);
      pendiente += d.toString('utf8');
      const lineas = pendiente.split('\n');
      pendiente = lineas.pop();
      for (const l of lineas) {
        if (l.includes('"done"')) {
          try { visto = visto || JSON.parse(l).type === 'done'; } catch (e) { /* línea rara */ }
        }
      }
    });
    s.on('close', () => {
      if (s.reintento) return; // se reintenta con otra conexión
      if (!visto) { error('the context ended the execution without finishing it'); process.exit(3); }
      process.exit(0);
    });
  }
  conectar();
}

const a = process.argv;
if (a.length === 4 && a[2] === 'serve') servir(a[3]);
else if (a.length === 5 && a[2] === 'run') cliente(a[3], a[4]);
else { process.stderr.write('usage: conductor.js serve SOCKET | run SOCKET TIMEOUT\n'); process.exit(64); }
