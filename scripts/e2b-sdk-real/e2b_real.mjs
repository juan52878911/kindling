// Prueba el shim `kling sbx e2b` con el SDK real de E2B para JS/TS (paquete de
// npm `e2b`, la versión que fija E2B_VERSION). Uso:
//
//   node e2b_real.mjs <url del shim> <E2B_API_KEY> <directorio> [plantilla]
//
// El paquete se busca en el node_modules de $KLING_E2B_NODE (un directorio con
// `npm i e2b@<versión>`) o, si no está, en el del directorio actual. Apunta el
// SDK al shim con E2B_API_URL y E2B_SANDBOX_URL, como e2b_real.py. Cada paso
// imprime OK o FALLA; al final, "E2B OK n/n" o código 1 (sin el paquete, 3).
import { createRequire } from 'node:module'
import path from 'node:path'
import fs from 'node:fs'

const E2B_VERSION = '2.64.0'

const [url, clave, baseArg, plantillaArg] = process.argv.slice(2)
const base = baseArg.replace(/\/+$/, '')
const plantilla = plantillaArg || 'base'
Object.assign(process.env, { E2B_API_URL: url, E2B_SANDBOX_URL: url, E2B_API_KEY: clave })
delete process.env.E2B_DEBUG
delete process.env.E2B_DOMAIN

const raiz = process.env.KLING_E2B_NODE || process.cwd()
const requerir = createRequire(path.join(raiz, 'kling-e2b.cjs'))
let e2b, version
try {
  e2b = requerir('e2b')
  version = JSON.parse(fs.readFileSync(requerir.resolve('e2b/package.json'), 'utf8')).version
} catch (e) {
  console.log('SIN SDK: ' + e.message)
  process.exit(3)
}
if (version !== E2B_VERSION) {
  console.log(`FALLA versión del SDK: ${version}, se esperaba ${E2B_VERSION}`)
  process.exit(1)
}
const { Sandbox, CommandExitError } = e2b

const pasos = []
async function paso(nombre, f) {
  const t0 = performance.now()
  try {
    const r = await f()
    pasos.push([nombre, 'OK', `${Math.round(performance.now() - t0)} ms`, String(JSON.stringify(r)).slice(0, 100)])
  } catch (e) {
    pasos.push([nombre, 'FALLA', '', `${e?.name}: ${e?.message}`.slice(0, 300)])
    console.error(e)
  }
  return pasos.at(-1)[1] === 'OK'
}

function esperar(cond, msg) {
  if (!cond) throw new Error(msg)
}

let sb

if (await paso('create', async () => {
  sb = await Sandbox.create(plantilla, { timeoutMs: 300_000, metadata: { suite: 'e2b-real-js' }, envs: { DE_CREATE: 'si' } })
  esperar(sb.sandboxId && !sb.sandboxId.includes('/'), `id: ${sb.sandboxId}`)
  return sb.sandboxId
})) {
  await paso('commands.run + onStdout', async () => {
    const trozos = []
    const r = await sb.commands.run('echo hola-e2b', { onStdout: (d) => trozos.push(d) })
    esperar(r.exitCode === 0 && r.stdout === 'hola-e2b\n', `salida ${JSON.stringify(r)}`)
    esperar(trozos.join('') === 'hola-e2b\n', `onStdout ${JSON.stringify(trozos)}`)
    return r.stdout
  })
  await paso('commands.run exit 3', async () => {
    try {
      await sb.commands.run('echo a-stderr 1>&2; exit 3')
    } catch (e) {
      esperar(e instanceof CommandExitError && e.exitCode === 3 && e.stderr.includes('a-stderr'), `error ${e}`)
      return e.exitCode
    }
    throw new Error('exit 3 no lanzó CommandExitError')
  })
  await paso('commands.run envs + cwd', async () => {
    const r = await sb.commands.run('echo "$SALUDO"; pwd', { envs: { SALUDO: 'hola-env' }, cwd: base })
    const l = r.stdout.trim().split('\n')
    esperar(l[0] === 'hola-env' && (l[1] === base || l[1] === fs.realpathSync(base)), `salida ${r.stdout}`)
    return r.stdout
  })
  await paso('Sandbox.create(envs) + run', async () => {
    const r = await sb.commands.run('echo "${DE_CREATE:-no}"')
    esperar(r.stdout === 'si\n', `salida ${r.stdout}`)
    return r.stdout
  })
  if (await paso('files.write', async () => {
    const i = await sb.files.write(base + '/hola.txt', 'contenido e2b\n')
    esperar(i.path === base + '/hola.txt', `info ${JSON.stringify(i)}`)
    return i.name
  })) {
    await paso('files.read (text, bytes)', async () => {
      const t = await sb.files.read(base + '/hola.txt')
      esperar(t === 'contenido e2b\n', `leído ${t}`)
      const b = await sb.files.read(base + '/hola.txt', { format: 'bytes' })
      esperar(Buffer.from(b).toString() === 'contenido e2b\n', `bytes ${b}`)
      return t
    })
    await paso('files exists/getInfo/makeDir/rename/list/remove', async () => {
      esperar(await sb.files.exists(base + '/hola.txt'), 'exists')
      esperar(!(await sb.files.exists(base + '/no-existe.txt')), 'exists de uno que no existe')
      const i = await sb.files.getInfo(base + '/hola.txt')
      esperar(i.size === 14 && i.type === 'file', `getInfo ${JSON.stringify(i)}`)
      esperar(await sb.files.makeDir(base + '/sub'), 'makeDir')
      await sb.files.rename(base + '/hola.txt', base + '/sub/movido.txt')
      const nombres = (await sb.files.list(base, { depth: 2 })).map((e) => e.name).sort()
      esperar(JSON.stringify(nombres) === '["movido.txt","sub"]', `list ${nombres}`)
      await sb.files.remove(base + '/sub')
      esperar(!(await sb.files.exists(base + '/sub')), 'remove')
      return nombres
    })
  }
  await paso('files.write (varios)', async () => {
    await sb.files.write([{ path: base + '/a.txt', data: 'a' }, { path: base + '/b.txt', data: 'b' }])
    esperar((await sb.files.read(base + '/a.txt')) === 'a', 'a.txt')
    esperar((await sb.files.read(base + '/b.txt')) === 'b', 'b.txt')
    await sb.files.remove(base + '/a.txt')
    await sb.files.remove(base + '/b.txt')
  })
  await paso('commands.run background + wait', async () => {
    const h = await sb.commands.run('sleep 0.2; echo de-fondo', { background: true })
    esperar(h.pid > 0, `pid ${h.pid}`)
    const r = await h.wait()
    esperar(r.exitCode === 0 && r.stdout === 'de-fondo\n', `wait ${JSON.stringify(r)}`)
    return h.pid
  })
  await paso('commands.run background + list + kill', async () => {
    const h = await sb.commands.run('sleep 60', { background: true })
    let pids = (await sb.commands.list()).map((p) => p.pid)
    esperar(pids.includes(h.pid), `list ${pids} no tiene ${h.pid}`)
    esperar(await h.kill(), 'kill devolvió false')
    await h.wait().catch((e) => esperar(e instanceof CommandExitError && e.exitCode !== 0, `wait ${e}`))
    pids = (await sb.commands.list()).map((p) => p.pid)
    esperar(!pids.includes(h.pid), `sigue en list ${pids}`)
    return h.pid
  })
  await paso('commands.connect', async () => {
    const h = await sb.commands.run('sleep 0.5; echo reconectado', { background: true })
    await h.disconnect()
    const h2 = await sb.commands.connect(h.pid)
    const r = await h2.wait()
    esperar(r.stdout.includes('reconectado'), `connect ${JSON.stringify(r)}`)
    return h.pid
  })
  await paso('Sandbox.list(metadata)', async () => {
    const p = Sandbox.list({ query: { metadata: { suite: 'e2b-real-js' } } })
    const ids = (await p.nextItems()).map((i) => i.sandboxId)
    esperar(JSON.stringify(ids) === JSON.stringify([sb.sandboxId]), `list ${ids}`)
    return ids
  })
  await paso('getInfo + isRunning', async () => {
    const i = await sb.getInfo()
    esperar(i.sandboxId === sb.sandboxId && i.metadata.suite === 'e2b-real-js', `info ${JSON.stringify(i)}`)
    esperar(await sb.isRunning(), 'isRunning')
    return i.state
  })
  await paso('setTimeout', () => sb.setTimeout(600_000))
  await paso('Sandbox.connect + run', async () => {
    const otro = await Sandbox.connect(sb.sandboxId)
    const r = await otro.commands.run('echo por-connect')
    esperar(r.stdout === 'por-connect\n', `salida ${r.stdout}`)
    return r.stdout
  })
  await paso('updateNetwork', async () => {
    await sb.updateNetwork({ allowOut: ['pypi.org'] })
    await sb.updateNetwork({})
  })
  if (await paso('pause', async () => {
    await sb.pause()
    esperar((await sb.getInfo()).state === 'paused', 'no quedó paused')
  })) {
    await paso('Sandbox.connect (resume) + run', async () => {
      const otro = await Sandbox.connect(sb.sandboxId)
      esperar((await otro.getInfo()).state === 'running', 'no quedó running')
      return (await otro.commands.run('echo tras-pausa')).stdout
    })
  }
  await paso('sendStdin/watchDir/pty fallan', async () => {
    // Lo que el gateway no tiene (docs/e2b.md) debe fallar con un error, no colgarse.
    const fallos = []
    for (const [nombre, f] of [
      ['sendStdin', () => sb.commands.sendStdin(1, 'x')],
      ['watchDir', () => sb.files.watchDir(base, () => {})],
      ['pty.create', () => sb.pty.create({ rows: 24, cols: 80, onData: () => {} })],
    ]) {
      try {
        await f()
      } catch (e) {
        fallos.push(`${nombre}: ${e?.name}`)
        continue
      }
      throw new Error(`${nombre} no falló`)
    }
    return fallos
  })
  await paso('kill', async () => {
    await sb.kill()
    try {
      await sb.getInfo()
    } catch {
      return 'borrado'
    }
    throw new Error('el sandbox sigue existiendo tras kill')
  })
}
for (const p of pasos) console.log(p.join('\t'))
const ok = pasos.filter((p) => p[1] === 'OK').length
if (ok === pasos.length && ok > 0) {
  console.log(`E2B OK ${ok}/${pasos.length} (e2b js ${version})`)
  process.exit(0)
}
console.log(`E2B FALLA ${ok}/${pasos.length}`)
process.exit(1)
