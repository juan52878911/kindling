package report

// css se incrusta en el HTML. Sin fuentes ni hojas remotas: el informe describe
// la topología de un homelab y no tiene por qué pedirle nada a nadie al abrirse.
//
// Se adapta al tema claro/oscuro del sistema con prefers-color-scheme.
const css = `
:root{
  --bg:#fbfbfa; --fg:#1a1a19; --muted:#6b6b68; --line:#e3e3e0; --card:#fff;
  --running:#2e7d5b; --warm:#b0721e; --other:#8a8a86; --accent:#3b5bdb;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#16161a; --fg:#e8e8e6; --muted:#98989a; --line:#2b2b31; --card:#1d1d22;
    --running:#4ec08c; --warm:#e0a44e; --other:#7a7a80; --accent:#7c95f5;
  }
}
*{box-sizing:border-box}
body{
  margin:0;padding:2rem 1.5rem 4rem;background:var(--bg);color:var(--fg);
  font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
  max-width:1180px;margin-inline:auto;
}
h1{font-size:1.6rem;margin:0;letter-spacing:-.02em}
h2{font-size:1.05rem;margin:0 0 .9rem;display:flex;align-items:center;gap:.6rem}
header{margin-bottom:1.5rem}
.sub{color:var(--muted);margin:.3rem 0 0;font-size:.86rem}
code{font:.85em ui-monospace,SFMono-Regular,Menlo,monospace;background:var(--line);padding:.1em .35em;border-radius:4px}

.stats{display:flex;flex-wrap:wrap;gap:.75rem;margin-bottom:1.5rem}
.stat{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:.7rem 1.1rem;min-width:118px}
.stat .v{display:block;font-size:1.45rem;font-weight:600;letter-spacing:-.02em}
.stat .l{display:block;color:var(--muted);font-size:.76rem;margin-top:.1rem}
.stat.running .v{color:var(--running)} .stat.warm .v{color:var(--warm)} .stat.other .v{color:var(--other)}

.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:1.15rem 1.25rem;margin-bottom:1.1rem}
.tag{font-size:.74rem;font-weight:400;color:var(--muted);background:var(--bg);
     border:1px solid var(--line);padding:.15rem .5rem;border-radius:20px}
.empty{color:var(--muted);margin:0;font-size:.88rem}


table{width:100%;border-collapse:collapse;font-size:.845rem}
th{text-align:left;font-weight:600;color:var(--muted);font-size:.72rem;
   text-transform:uppercase;letter-spacing:.04em;padding:0 .7rem .5rem 0;border-bottom:1px solid var(--line)}
td{padding:.55rem .7rem .55rem 0;border-bottom:1px solid var(--line);vertical-align:top}
tr:last-child td{border-bottom:none}
.mono{font:.82rem ui-monospace,SFMono-Regular,Menlo,monospace}
.ctr{text-align:center}
.id{display:block;color:var(--muted);font:.72rem ui-monospace,Menlo,monospace}
.muted{color:var(--muted)}

.badge{display:inline-block;padding:.1rem .5rem;border-radius:20px;font-size:.74rem;font-weight:600}
.badge.running{background:color-mix(in srgb,var(--running) 16%,transparent);color:var(--running)}
.badge.warm{background:color-mix(in srgb,var(--warm) 18%,transparent);color:var(--warm)}
.badge.other{background:color-mix(in srgb,var(--other) 16%,transparent);color:var(--other)}
.chip{display:inline-block;background:var(--bg);border:1px solid var(--line);
      border-radius:5px;padding:.05rem .35rem;margin:0 .25rem .25rem 0;
      font:.74rem ui-monospace,Menlo,monospace}

.legend{color:var(--muted);font-size:.78rem;margin:.9rem 0 0;display:flex;align-items:center;gap:.4rem;flex-wrap:wrap}
.legend .k{display:inline-block;width:11px;height:11px;border-radius:3px;border:1.5px solid;margin-left:.5rem}
.legend .k.running{border-color:var(--running)} .legend .k.warm{border-color:var(--warm)}
.legend .k.other{border-color:var(--other);border-style:dashed}
footer{color:var(--muted);font-size:.78rem;text-align:center;margin-top:2rem}
`

// cssTree estiliza el árbol navegable. El color del borde codifica estado, y
// el gris de la derecha es siempre anotación: nunca algo en lo que se pueda
// pulsar.
const cssTree = `
.tabs{display:flex;gap:.3rem;flex-wrap:wrap;margin:-.2rem 0 .5rem}
.tab{font:600 12.5px inherit;padding:.34rem .8rem;border-radius:6px;cursor:pointer;
     background:transparent;border:1px solid var(--line);color:var(--muted)}
.tab:hover{color:var(--fg);border-color:var(--muted)}
.tab.on{background:var(--bg);color:var(--fg);border-color:var(--accent)}
.viewhint{color:var(--muted);font-size:.84rem;margin:.1rem 0 .7rem}

.crumbs{display:flex;align-items:center;gap:.3rem;flex-wrap:wrap;margin:0 0 .5rem;
        font-size:.8rem}
.crumb{background:none;border:none;padding:.1rem .2rem;cursor:pointer;color:var(--accent);
       font:inherit}
.crumb:hover{text-decoration:underline}
.crumbs .sep{color:var(--muted)}

.map{overflow-x:auto}
.map svg{width:100%;height:auto;display:block;min-width:620px}

.box{fill:var(--bg);stroke:var(--line);stroke-width:1.5}
.b-host{fill:color-mix(in srgb,var(--accent) 20%,var(--bg));stroke:var(--accent);stroke-width:2}
.b-running,.b-atendiendo{stroke:#2ea043;stroke-width:2}
.b-warm,.b-dormido{stroke:#d29922;stroke-width:2;stroke-dasharray:5 3}
.b-ready,.b-listo{stroke:var(--muted);stroke-dasharray:5 3}
.b-ext{stroke:#58a6ff;stroke-width:2}
.b-warn{stroke:#d29922}
.b-bloqueado,.b-failed{stroke:#f85149;stroke-width:2}

svg text{font:600 13px ui-sans-serif,-apple-system,system-ui,sans-serif;fill:var(--fg)}
text.s{font-size:11px;font-weight:400;fill:var(--muted)}
text.note{font-size:11.5px;font-weight:400;fill:var(--muted)}
text.caret{font-size:15px;font-weight:400;fill:var(--muted);text-anchor:middle}

.edge{fill:none;stroke:var(--line);stroke-width:1.6}
.e-running,.e-atendiendo{stroke:#2ea043}
.e-warm,.e-dormido{stroke:#d29922}
.e-ready,.e-listo{stroke:var(--line);stroke-dasharray:5 4}
.e-ext{stroke:#58a6ff}
.e-warn{stroke:#d29922;stroke-dasharray:5 4}

.node{cursor:pointer}
.node:focus{outline:none}
.node:hover .box{filter:brightness(1.3)}
.node.sel .box{stroke-width:2.8;filter:brightness(1.15)}

.k{display:inline-block;width:11px;height:11px;border-radius:3px;vertical-align:-1px;
   margin:0 .3rem 0 .9rem}
.k.running{background:#2ea043}.k.warm{background:#d29922}
.k.ready{background:transparent;border:1.5px dashed var(--muted)}
.k.ext{background:#58a6ff}.k.warn{background:#d29922}

.drill{font:600 12.5px inherit;padding:.36rem .8rem;border-radius:6px;cursor:pointer;
       background:transparent;border:1px solid var(--accent);color:var(--accent);
       margin:0 0 .9rem}
.drill:hover{background:color-mix(in srgb,var(--accent) 14%,transparent)}

#panel h3{font-size:.76rem;text-transform:uppercase;letter-spacing:.07em;color:var(--muted);
          margin:0 0 .45rem}
#panel .empty{color:var(--muted);margin:0}
dl.detail{display:grid;grid-template-columns:auto 1fr;gap:.3rem .9rem;margin:0 0 1rem}
dl.detail dt{color:var(--muted);font-size:.82rem}
dl.detail dd{margin:0;font-size:.86rem}
.steps ol{margin:0 0 1rem;padding-left:1.15rem}
.steps li{margin:.22rem 0}
.persist{margin:0 0 1rem;padding:.7rem .9rem;border-radius:7px;border-left:3px solid}
.persist.ok{border-color:#2ea043;background:color-mix(in srgb,#2ea043 8%,transparent)}
.persist.warn{border-color:#d29922;background:color-mix(in srgb,#d29922 9%,transparent)}
.persist p{margin:0}
.chips{display:flex;flex-wrap:wrap;gap:.32rem}
.chip{font:400 11.5px ui-monospace,monospace;padding:.16rem .45rem;border:1px solid var(--line);
      border-radius:4px;color:var(--muted)}
`
