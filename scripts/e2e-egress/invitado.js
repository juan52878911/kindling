// Corre dentro de la microVM: node invitado.js get URL | rafaga IP PUERTO SEGUNDOS
const i = process.argv.findIndex(x => x === "get" || x === "rafaga");
const [modo, a, b, c] = process.argv.slice(i);
if (modo === "get") {
  fetch(a, { signal: AbortSignal.timeout(3000) })
    .then(r => r.text()).then(t => console.log("OK " + t))
    .catch(e => console.log("FAIL " + (e.cause?.code || e.name)));
} else if (modo === "rafaga") {
  const s = require("dgram").createSocket("udp4");
  const fin = Date.now() + Number(c) * 1000, buf = Buffer.alloc(64);
  let enviados = 0;
  const lote = () => {
    if (Date.now() > fin) { setTimeout(() => { console.log("ENVIADOS " + enviados); s.close(); }, 300); return; }
    for (let i = 0; i < 200; i++) s.send(buf, Number(b), a, e => { if (!e) enviados++; });
    setImmediate(lote);
  };
  lote();
}
