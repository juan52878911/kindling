# Community posts: drafts

Every number in here comes from docs/benchmarks.md (c8i.2xlarge with nested virtualization,
n=15 serial). If a post quotes a number, it also says where it was measured. Re-read each
community's current rules the day you post. The norms noted here are a starting point, not a
substitute for the sidebar.

---

## r/selfhosted

**Norms.** Read the current rules and flair before posting: the sub has tightened its rules
for project announcements more than once. Say plainly that you're the developer, use the
release/project flair it asks for, and check whether new projects have to go in a specific
thread or on a specific day, and whether AI-assisted code has to be disclosed. Selfhosters
care about: what it runs on, what it phones home to, the license, the install footprint, and
how to remove it. Post a text post with the repo link, not just a link.

**Title:** I built a self-hosted way to run throwaway microVMs that freeze to disk when idle
(Linux + Mac, Apache-2.0)

> I'm the developer. kindling is one daemon plus a CLI (`kling`) that runs microVMs
> (Firecracker on Linux with KVM, Apple's Virtualization.framework on Apple Silicon Macs).
> Each template boots once and is frozen. Everything after that is a restore from that file,
> and an idle machine can be frozen back to disk, where it uses no CPU or RAM until the next
> request.
>
> Why I wanted it at home: running things I don't fully trust (MCP servers for a local
> agent, code an agent writes, random tools) behind a hypervisor boundary instead of a shared
> kernel, without leaving 20 VMs eating RAM all day.
>
> What it looks like:
>
> ```
> kling try -- uname -a                  # throwaway VM: create, run, print, delete
> kling image import postgres:17-alpine -e POSTGRES_PASSWORD
> kling run -image postgres-17-alpine -mem 512M -wait-ready
> kling freeze <name>  /  kling thaw <name>
> ```
>
> Selfhosting details:
>
> - **The daemon has no network port.** It listens on a Unix socket only; remote control goes
>   over SSH.
> - **No egress by default.** VMs start with egress `none`, and `internet` mode blocks private
>   ranges, including your LAN and router. One known gap: ports your router forwards are
>   still reachable through your own public IP (hairpin NAT).
> - **Install and removal.** `install.sh` verifies a signed `SHA256SUMS`. `kling up -install`
>   on Linux sets up the systemd unit, and `kling up -uninstall` removes the Mac agent.
>   `kling doctor` tells you what's missing.
> - **Density.** On my old lab box (i7-8700T, nested), 10 copies of one template added 68 MiB
>   of RAM in total.
>
> Limits, honestly: the macOS backend has no jailer (it uses a sandbox profile), egress
> allowlists filter by IP, and it's maintained by one person. The threat model and the
> "not solved" list are in SECURITY.md (in Spanish for now; an English summary is
> <PLACEHOLDER: link>).
>
> Repo: https://github.com/juan52878911/kindling. Happy to answer anything, and I'd like to
> hear what breaks on your hardware.

---

## r/LocalLLaMA

**Norms.** Posts have to be about local models and tooling. Generic self-promotion gets
removed, and the community is sensitive to anything that looks like an ad. Lead with the
local-agent use case, don't name or rank models, and don't claim anything about inference
performance (this post is about where an agent's code runs, not about the model). Disclose
that you're the author. Answer questions in the comments rather than with edits that pile on
links.

**Title:** Running your local agent's code in throwaway microVMs on your own Mac/Linux box,
not on the host

> If your local agent can run shell commands or `pip install` things, that code runs as you,
> on your machine. I wanted a cheap way to give it a real VM instead, so I built kindling (I'm
> the author; Apache-2.0).
>
> - **What it is.** One daemon and CLI that run microVMs: Firecracker on Linux with KVM, and
>   on Apple Silicon Macs the native Virtualization.framework, no root.
> - **Why it's cheap.** Machines aren't booted per request. A template boots once, is frozen
>   to disk, and each sandbox is a restore of that file. Idle sandboxes freeze back to disk
>   and use 0 CPU and 0 RAM until the next exec.
> - **Hooking up your agent.** `kling sbx` is a sandbox gateway with a token per tenant, plus
>   an MCP server (`kling sbx mcp`) that gives an agent tools to create a sandbox, run code,
>   read and write files, and fork or pause it. There are Python and TypeScript SDKs, an
>   OpenSandbox-compatible API and an E2B shim (status in docs/e2b.md).
> - **Network.** Sandboxes start with no network. You can allow specific domains, and API
>   keys can stay on the host through a credential proxy: the VM only sees a placeholder.
> - **Try it.** `kling try -- uname -a` creates a throwaway VM, runs the command and deletes
>   it.
>
> Numbers, with their context: on a nested EC2 c8i.2xlarge, claiming a sandbox from a warm
> pool took 0.8–1.1 ms and a create without a pool took 76–91 ms (n=15 serial). Those are
> Linux/Firecracker numbers, and I haven't published equivalents for the Mac backend.
>
> Limits: on macOS there's no jailer (a sandbox profile instead), allowlist egress is
> IP-based, and the code interpreter endpoint is experimental. Repo and docs:
> https://github.com/juan52878911/kindling
>
> What I'd like to know: what agent setups you'd plug this into, and what's missing for them.

---

## Lobsters

**Norms.** You need an invite from an existing member. Self-promotion is accepted in
moderation from people who also take part in the site: if most of your activity is your own
links, expect flags. Tick **"I am the author of the story at this URL"**. Submit the
**article**, not the repo: Lobsters prefers technical write-ups to product pages. Pick tags
from the site's existing list, such as `virtualization`, `linux`, `security` and `go`
(check they exist and that they fit). Don't post a promotional comment. If you comment,
add technical context or answer questions.

**Title:** MicroVM sandboxes in ~1 ms warm and under 100 ms cold: golden snapshots, warm
pools, and freezing to disk

(Use the article's title unchanged. Lobsters frowns on editorialised titles.)

**Optional first comment (only if it adds context):**

> Author here. The part I'd most like scrutinised is the comparison with OpenSandbox: it's a
> microVM against a runc container on one nested EC2 instance with a small n, and OSB's exec
> took ~1 s against its documented ~203 ms for a reason I haven't found. All nine caveats are
> in the post. Second-most: the threat model, especially the IP-based egress allowlist.

---

## X / Bluesky thread (7 posts, each under 280 characters)

1/ I've been building kindling: one daemon + CLI that runs microVMs (Firecracker on Linux,
Virtualization.framework on macOS) without booting them on the request path. Each template
boots once, is frozen, and every instance is a restore. Apache-2.0. (thread)

2/ A golden snapshot = VM state + guest memory + a copy of the disk taken while paused, all
signed. Memory is a mapped file, so N copies share pages through the page cache. 10 copies on
my lab box: +68 MiB of RAM in total.

3/ Disks are reflink clones (XFS/Btrfs, or a loop-mounted store on ext4). The network is the
same tap0/IP/MAC in a namespace per VM, because Firecracker can't rename the TAP on restore.
After each restore the guest gets fresh entropy and the host clock.

4/ Freeze = dump memory, kill the VMM: just files, 0 CPU, 0 RAM. Pause = VMM alive, vCPUs
stopped. On nested EC2, `kling thaw` → first exec: 92–106 ms after a freeze, 6.1–6.6 ms after
a pause. RAM vs latency, your call.

5/ For agent sandboxes there's a warm pool, claimed with one atomic call: 0.8–1.1 ms to hand
out a running VM. Without a pool, a restore takes 76–91 ms (c8i.2xlarge, nested, n=15).
That's not a kernel boot; a boot takes seconds.

6/ Against OpenSandbox 1.2.1 on the same rig it's microVM vs runc container, small n, and
OSB's exec took ~1 s for a reason I couldn't explain. OSB is ahead on a credential vault, k8s
CRDs and SDKs. All nine caveats: <PLACEHOLDER: link to docs/benchmarks.md>

7/ The guest is assumed hostile, and SECURITY.md lists what's not solved (macOS has no
jailer, IP-based egress allowlist, authz is an MVP). Try it with `kling try -- uname -a`.
Write-up: <PLACEHOLDER: article link> · repo: <PLACEHOLDER: repo link>

(Bluesky allows 300 characters, so the same text fits. Put the links in the last post, not
the first: both platforms tend to bury posts that start with an external link.)

---

## LinkedIn (español)

**Normas.** En LinkedIn se espera un tono profesional y la primera persona. Pon el enlace en
el primer comentario, no en el cuerpo: el algoritmo castiga los enlaces externos en el post.
Sin hashtags de relleno: 3 como mucho. No etiquetes a gente para pedir difusión.

> Llevo meses construyendo kindling, un proyecto open source (Apache-2.0) para ejecutar
> código no confiable en microVMs: Firecracker en Linux y Virtualization.framework en Mac.
>
> La idea central: no arrancar nunca una máquina cuando llega una petición. Cada plantilla
> arranca una vez, se congela en disco (un "snapshot dorado") y cada instancia nueva es una
> restauración de ese fichero. Una máquina congelada no gasta ni CPU ni RAM: son ficheros.
>
> ¿Para qué? Los agentes de IA ejecutan código que nadie ha revisado. Un contenedor comparte
> el kernel con el host; una microVM pone un hipervisor en medio.
>
> Lo medido, con su contexto (una EC2 c8i.2xlarge con virtualización anidada, muestras
> pequeñas):
>
> - entregar un sandbox ya caliente de un pool: 0,8–1,1 ms;
> - crear uno sin pool (restaurar del snapshot): 76–91 ms;
> - despertar uno congelado hasta su primer comando: 92–106 ms.
>
> Lo he comparado con OpenSandbox 1.2.1 en la misma máquina y he publicado las nueve
> salvedades de esa comparación, incluido en qué va por delante OpenSandbox. También publico
> lo que todavía no está resuelto en seguridad. Una lista de garantías sin sus límites es
> propaganda.
>
> Es un proyecto personal y me vendría muy bien que gente de infraestructura, seguridad o
> plataformas lo pruebe y me diga dónde falla. Enlace en el primer comentario.
>
> #opensource #seguridad #infraestructura

**Primer comentario:** Repo: https://github.com/juan52878911/kindling · Artículo técnico (en
inglés): <PLACEHOLDER> · Benchmarks y salvedades: docs/benchmarks.md

---

## Comunidades de desarrollo en español (Reddit, Discord, Telegram)

**Normas.** Cada comunidad tiene las suyas. Lee las reglas fijadas antes de publicar: muchas
limitan la autopromoción a un hilo semanal o piden participar antes de enlazar lo propio. Di
que eres el autor. Si el grupo es de un lenguaje concreto (Go, por ejemplo), cuenta la parte
técnica que interesa ahí (está escrito en Go, y el módulo raíz no usa cgo) en vez del
anuncio general.

**Versión corta (Reddit / foro):**

> **kindling: microVMs que despiertan de un snapshot en lugar de arrancar (Linux y Mac,
> Apache-2.0)**
>
> Soy el autor. Es un daemon y un CLI (`kling`) que ejecuta microVMs: Firecracker en Linux y
> Virtualization.framework en Mac. Cada plantilla arranca una vez y se congela. Cada
> instancia es una restauración: memoria mapeada y compartida entre copias, disco clonado por
> reflink. Una máquina congelada son ficheros, sin CPU ni RAM.
>
> Lo hice para ejecutar el código de agentes de IA detrás de un hipervisor y no de un kernel
> compartido. Tiene un gateway de sandboxes multiinquilino con pool precalentado y una API
> compatible con OpenSandbox.
>
> En una EC2 anidada (c8i.2xlarge, n=15): 0,8–1,1 ms para entregar uno del pool, 76–91 ms
> para crear uno sin pool. Las salvedades están en docs/benchmarks.md. Lo no resuelto (macOS
> sin jailer, allowlist por IP, authz en fase MVP) está en SECURITY.md, que de momento está en
> español.
>
> Para probarlo: `kling try -- uname -a`. Repo: https://github.com/juan52878911/kindling
>
> Busco a gente que lo pruebe en su máquina y me diga qué se rompe.

**Versión mínima (Discord / Telegram, un mensaje):**

> Hola, soy el autor de kindling (Apache-2.0): microVMs con Firecracker o
> Virtualization.framework que se restauran de un snapshot en vez de arrancar, y se congelan
> a disco cuando no se usan (0 CPU, 0 RAM). Pensado para ejecutar código de agentes de IA
> aislado. Si alguien lo prueba con `kling try -- uname -a`, me interesa mucho saber qué
> falla. https://github.com/juan52878911/kindling
