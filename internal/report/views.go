package report

// Las vistas son el mismo sistema mirado desde cuatro alturas distintas. Cada
// una construye un árbol a partir del modelo; el renderizador no sabe de qué
// vista viene y las dibuja todas igual.
//
// Un nodo:  {id, label, sub, note, cls, kids, open, detail}
//
//	cls    colorea el borde por estado
//	note   texto gris a la derecha, para lo que no cabe dentro de la caja
//	detail lo que aparece en el panel al seleccionarlo
const jsViews = `
const M = JSON.parse(document.getElementById('model').textContent);
const mem = M.host.memory;

const n = (id, label, o = {}) => ({id, label, kids: [], open: false, ...o});
const plural = (k, one, many) => k + ' ' + (k === 1 ? one : many);

// La latencia trae su explicación detrás de dos puntos; en la anotación solo
// cabe la cifra, y el resto ya está en el panel.
const coste = l => (l.split(':')[0] || l).trim();

// Detalle de persistencia: se repite en varias vistas, así que vive aparte.
function storeNote(s) {
  if (!s.writers.length) return {cls: 'ok', title: "Doesn't persist anything",
    text: 'None of its tools write.'};
  return {
    cls: s.storeOk ? 'ok' : 'warn',
    title: s.storeOk ? 'What it writes survives' : 'Watch what it writes',
    text: 'Writes via ' + s.writers.join(', ') + '. ' + s.store + '.',
  };
}

function svcDetail(s) {
  const rows = [['type', s.kind === 'externo' ? 'external server' :
                         s.ephemeral ? 'ephemeral microVM' : 'persistent microVM'],
                ['next call', s.latency],
                ['tools', String(s.tools.length)]];
  if (s.snapshot)  rows.push(['snapshot', s.snapshot], ['image', s.image],
                             ['shared memory', s.ramShared],
                             ['resources', s.vcpus + ' vCPU · ' + s.memMiB + ' MiB']);
  if (s.url)       rows.push(['url', s.url]);
  rows.push(['live instances', String(s.machines.length)]);
  return {rows, steps: s.flow, note: storeNote(s)};
}

function machDetail(m) {
  const rows = [['id', m.id], ['state', m.state], ['ip', m.ip],
                ['egress', m.egress === 'internet' ? 'internet' : 'isolated'],
                ['resources', m.vcpus + ' vCPU · ' + m.memMiB + ' MiB'],
                ['own disk', m.disk], ['last operation', m.lastOp], ['age', m.age]];
  for (const [k, v] of Object.entries(m.labels || {})) rows.push(['label ' + k, v]);
  return {rows};
}

const machNode = (m) => n('m:' + m.id, m.name, {
  sub: m.id.slice(0, 8), cls: m.state, note: m.ip + ' · ' + m.lastOp,
  detail: machDetail(m),
});

// Un servicio sin instancias no debe verse como un error: está listo, solo que
// nadie lo ha llamado todavía.
function svcKids(s) {
  if (!s.machines.length) return [];
  return s.machines.map(machNode);
}

const VIEWS = {

  topologia: {
    label: 'Topology',
    hint: "The host, its services, and each one's live instances.",
    legend: [['running', 'running'], ['warm', 'asleep'],
             ['ready', 'ready, no instance'], ['ext', 'external']],
    build() {
      const kids = M.services.map(s => n('s:' + s.name, s.name, {
        cls: s.kind === 'externo' ? 'ext' : s.state,
        sub: plural(s.tools.length, 'tool', 'tools'),
        note: s.machines.length ? '' :
              s.kind === 'externo' ? "doesn't run here" : 'no instances · ' + coste(s.latency),
        kids: svcKids(s), open: s.machines.length > 0,
        detail: svcDetail(s),
      }));
      return n('host', 'host', {cls: 'host', sub: M.host.endpoint, kids, open: true,
        detail: {rows: [['endpoint', M.host.endpoint],
                        ['firecracker', M.host.firecracker || '—'],
                        ['kvm', M.host.kvm ? 'yes' : 'no'],
                        ['shared memory', M.host.ramShared],
                        ["instances' own disk", M.host.diskOwn],
                        ['memory service', mem || 'none']]}});
    },
  },

  capas: {
    label: 'Layers',
    hint: "What a call passes through, from the model's request to the MCP server.",
    legend: [['ready', 'software layer'], ['running', 'running process']],
    build() {
      const vm = n('l:vm', 'Firecracker microVM', {
        sub: 'one process per instance', cls: 'ready',
        detail: {rows: [['isolation', "KVM: doesn't share a kernel with the host"],
                        ['boot', '~250 ms from the golden snapshot'],
                        ['thaw', '~30 ms']],
                 steps: ['Firecracker restores the golden snapshot',
                         'Memory is mapped with the File backend: instances share pages',
                         'The base disk is read-only; each instance writes to its own overlay']},
        kids: [
          n('l:kernel', 'vmlinux kernel', {sub: 'shared by all', cls: 'ready',
            detail: {rows: [['mode', 'read-only'], ['cost', 'one cached copy for all']]}}),
          n('l:base', 'base rootfs', {sub: '/dev/vda · read-only', cls: 'ready',
            detail: {rows: [['mode', 'read-only, shared'],
                            ['why', 'one image serves N instances without duplication']]}}),
          n('l:overlay', 'own overlay', {sub: '/dev/vdb · sparse', cls: 'ready',
            note: 'this is where whatever the guest writes ends up',
            detail: {rows: [['mode', 'read-write, per instance'],
                            ['size', 'sparse: only takes up what gets written']],
                     note: {cls: 'warn', title: "This doesn't survive the instance",
                            text: 'The overlay dies with the machine. Whatever needs to last goes to ' +
                                  (mem || 'a linked memory service') + '.'}}}),
          n('l:bridge', 'bridge', {sub: 'stdin/stdout ↔ HTTP', cls: 'running',
            detail: {rows: [['role', 'translates the MCP call to the guest process']],
                     steps: ['Receives the call from the gateway',
                             "Writes it to the MCP server's stdin",
                             'Reads the response from its stdout and returns it']},
            kids: [n('l:mcp', 'MCP server', {sub: 'the original binary, untouched', cls: 'running',
              detail: {rows: [['transport', 'stdio'],
                              ['modifications', 'none: it runs as-is']]}})],
            open: true}),
        ], open: true});

      return n('l:gw', 'kling gateway', {cls: 'host', sub: 'a single HTTP endpoint', open: true,
        detail: {rows: [['endpoint', M.host.endpoint],
                        ['services', String(M.services.length)]]},
        kids: [
          n('l:agg', '/mcp/_all aggregator', {sub: '3 meta-tools', cls: 'running',
            note: 'avoids dumping every catalog into the context',
            detail: {rows: [['why', 'exposing N full catalogs fills up the context'],
                            ['in exchange', 'the inventory goes in initialize.instructions']],
                     chips: ['find_tools', 'describe_tool', 'call_tool']},
            kids: [
              n('l:find', 'find_tools', {sub: 'searches by intent', cls: 'ready',
                detail: {rows: [['input', 'a natural-language query'],
                                ['extra', 'Spanish↔English synonym table']]}}),
              n('l:desc', 'describe_tool', {sub: 'returns the schema', cls: 'ready',
                detail: {rows: [['input', 'tool name'],
                                ['output', 'its parameter schema, only when needed']]}}),
              n('l:call', 'call_tool', {sub: 'executes', cls: 'ready',
                detail: {rows: [['extra', 'coerces types against the declared schema']]}}),
            ]}),
          n('l:pool', 'pre-warmed pool', {sub: 'instances ready ahead of time', cls: 'running',
            note: 'turns 250 ms into ~20 ms',
            detail: {rows: [['what it holds', 'instances with the MCP session already open'],
                            ['what for', "the first call doesn't pay for the boot"]]}}),
          vm,
          n('l:links', 'external links', {
            sub: plural(M.services.filter(s => s.kind === 'externo').length, 'server', 'servers'),
            cls: 'ext', note: "they don't start any machine here",
            kids: M.services.filter(s => s.kind === 'externo').map(s =>
              n('s:' + s.name, s.name, {cls: 'ext', sub: s.url, detail: svcDetail(s)})),
            detail: {rows: [['what they are', 'MCP servers already running elsewhere'],
                            ['cost', 'the gateway just forwards']]}}),
        ]});
    },
  },

  mcp: {
    label: 'MCP',
    hint: 'The catalog: which tools each service offers and which ones write.',
    legend: [['running', 'reads'], ['warn', 'writes'], ['ext', 'external server']],
    build() {
      const kids = M.services.map(s => n('s:' + s.name, s.name, {
        cls: s.kind === 'externo' ? 'ext' : s.state,
        sub: plural(s.tools.length, 'tool', 'tools'),
        note: s.writers.length ? plural(s.writers.length, 'writes', 'write') : 'read-only',
        detail: svcDetail(s),
        kids: s.tools.map(t => n('t:' + s.name + '.' + t.name, t.name, {
          cls: t.writes ? 'warn' : 'running', sub: t.writes ? 'writes' : 'reads',
          note: t.desc,
          detail: {rows: [['service', s.name], ['effect', t.writes ? 'writes' : 'read-only'],
                          ['description', t.desc || '—']],
                   steps: s.flow,
                   note: t.writes ? storeNote(s) : null},
        })),
      }));
      const total = M.services.reduce((a, s) => a + s.tools.length, 0);
      return n('cat', 'catalog', {cls: 'host', sub: plural(total, 'tool', 'tools'),
        kids, open: true,
        detail: {rows: [['services', String(M.services.length)],
                        ['tools', String(total)],
                        ['how it is exposed', 'inventory in initialize.instructions, not the entire catalog']]}});
    },
  },

  red: {
    label: 'Network',
    hint: 'Who can reach the internet and who is isolated.',
    legend: [['running', 'has egress'], ['ready', 'isolated'], ['ext', 'outside the host']],
    build() {
      const live = M.services.flatMap(s => s.machines.map(m => ({s, m})));
      const kids = [];

      const externos = M.services.filter(s => s.kind === 'externo');
      if (externos.length) kids.push(n('r:ext', 'internet', {cls: 'ext',
        sub: plural(externos.length, 'external server', 'external servers'),
        note: 'the gateway reaches out to them',
        kids: externos.map(s => n('s:' + s.name, s.name, {cls: 'ext', sub: s.url,
          detail: svcDetail(s)})),
        detail: {rows: [['direction', 'outbound, initiated by the gateway']]}}));

      if (!live.length) {
        kids.push(n('r:none', 'no instances', {cls: 'ready',
          sub: 'nothing has networking right now',
          note: 'each instance gets its own namespace when it boots',
          detail: {rows: [['why', 'microVMs only exist while they are being used']],
                   steps: ['On boot, each instance gets its own namespace',
                           'Inside, they all see the same tap0 and the same 172.16.0.2',
                           'On the host, their veth tells them apart',
                           'Internet egress is closed unless requested']}}));
      }

      for (const {s, m} of live) {
        const open = m.egress === 'internet';
        kids.push(n('r:' + m.id, 'netns ' + m.id.slice(0, 8), {
          cls: open ? 'running' : 'ready',
          sub: s.name + ' · ' + m.name,
          note: open ? 'internet egress allowed' : 'isolated',
          kids: [n('r:tap' + m.id, 'tap0 → 172.16.0.2', {cls: open ? 'running' : 'ready',
            sub: 'identical inside every namespace',
            note: 'on the host, its veth tells it apart (' + m.ip + ')',
            detail: {rows: [['guest ip', '172.16.0.2 (always the same)'],
                            ['ip as seen from the host', m.ip],
                            ['why', 'one namespace per machine lets every instance reuse the same network plan']]}})],
          detail: {rows: [['machine', m.name], ['service', s.name],
                          ['ip', m.ip], ['egress', open ? 'internet' : 'isolated']],
                   note: open ? {cls: 'warn', title: 'This instance can reach the internet',
                                 text: 'Only what truly needs it should have this.'} : null}}));
      }

      return n('r:host', 'host', {cls: 'host', sub: M.host.endpoint, kids, open: true,
        detail: {rows: [['role', 'routes, filters, and isolates'],
                        ['policy', 'no internet egress unless explicitly requested']]}});
    },
  },
};
`
