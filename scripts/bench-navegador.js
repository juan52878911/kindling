// Comparador de motores de navegador para servicios MCP de scraping.
//
//   EXE=/ruta/al/binario node scripts/bench-navegador.js
//
// Mide lo que decide la eleccion: cuanto tarda en ARRANCAR el motor —que es lo
// que se paga en cada despertar de una microVM— y cuanto en cargar y extraer
// texto de paginas reales.
//
// El numero de caracteres extraidos es la comprobacion de EQUIVALENCIA: si dos
// motores devuelven cuentas distintas, uno de los dos esta viendo otra pagina y
// la comparacion de tiempos no significa nada.
//
// Sitios elegidos por estables, publicos y de peso muy distinto: una pagina
// minima, una historica de HTML plano, y un texto largo.
const { chromium } = require("playwright-core");
const SITIOS = [
  "https://example.com/",
  "https://info.cern.ch/hypertext/WWW/TheProject.html",
  "https://www.rfc-editor.org/rfc/rfc2324.txt",
];
(async () => {
  const t0 = Date.now();
  const b = await chromium.launch({ executablePath: process.env.EXE, args: ["--no-sandbox"] });
  const tLaunch = Date.now() - t0;
  const res = [];
  for (const url of SITIOS) {
    const t = Date.now();
    const p = await b.newPage();
    await p.goto(url, { waitUntil: "domcontentloaded", timeout: 30000 });
    const titulo = await p.title();
    const texto = (await p.evaluate(() => document.body.innerText)).trim();
    res.push({ url, ms: Date.now() - t, titulo: titulo.slice(0,42), chars: texto.length });
    await p.close();
  }
  await b.close();
  console.log(JSON.stringify({ launch_ms: tLaunch, paginas: res }, null, 1));
})().catch(e => { console.error("ERROR:", e.message.split("\n")[0]); process.exit(1); });
