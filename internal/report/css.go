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

.diagram{overflow-x:auto}
svg{width:100%;min-width:820px;height:auto;display:block}
.node{fill:var(--card);stroke:var(--line);stroke-width:1.5}
.node.host{fill:var(--accent);stroke:none}
.node.group{fill:var(--bg);stroke:var(--accent);stroke-width:1.5}
.node.running{stroke:var(--running)} .node.warm{stroke:var(--warm)} .node.other{stroke:var(--other);stroke-dasharray:3 3}
.link{fill:none;stroke:var(--line);stroke-width:1.5}
.lbl{fill:var(--fg);font:13px ui-sans-serif,system-ui,sans-serif}
.lbl.host{fill:#fff;font-weight:600}
.lbl.group{font-weight:600}
.lbl.meta,.lbl.shared,.lbl.muted{fill:var(--muted);font-size:11.5px}

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

// cssDetail se añade cuando el informe incluye fichas por servicio.
const cssDetail = `
.detail{display:grid;grid-template-columns:auto 1fr;gap:.35rem 1rem;margin:0 0 1rem;
        padding:.85rem 1rem;background:var(--bg);border:1px solid var(--line);border-radius:9px;font-size:.85rem}
.detail dt{color:var(--muted);font-weight:600;white-space:nowrap}
.detail dd{margin:0}
.tools{font:.78rem ui-monospace,Menlo,monospace;color:var(--muted)}
.run{font-weight:700}
.run.yes{color:var(--running)} .run.no{color:#c0392b}
.tag.ext{border-color:var(--accent);color:var(--accent)}
`
