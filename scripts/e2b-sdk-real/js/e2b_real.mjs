// SDK JS real de E2B contra el shim (el shim y el gateway falso los arranca e2b_js.py)
import { Sandbox } from 'e2b'
const pasos = []
async function paso(n, f) { try { const r = await f(); pasos.push([n, 'OK', JSON.stringify(r)?.slice(0, 120)]); return true } catch (e) { pasos.push([n, 'FALLA', String(e?.message ?? e).slice(0, 300)]); return false } }
let sb
if (await paso('create', async () => { sb = await Sandbox.create({ timeoutMs: 120000 }); return sb.sandboxId })) {
  await paso('commands.run', async () => { const r = await sb.commands.run('echo hola'); return [r.exitCode, r.stdout, r.stderr] })
  await paso('files.write', () => sb.files.write('/tmp/hola.txt', 'contenido'))
  await paso('files.read', () => sb.files.read('/tmp/hola.txt'))
  await paso('kill', () => sb.kill())
}
for (const p of pasos) console.log(p.join('\t'))
