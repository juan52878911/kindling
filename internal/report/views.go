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
  if (!s.writers.length) return {cls: 'ok', title: 'No persiste nada',
    text: 'Ninguna de sus herramientas escribe.'};
  return {
    cls: s.storeOk ? 'ok' : 'warn',
    title: s.storeOk ? 'Lo que escriba sobrevive' : 'Ojo con lo que escriba',
    text: 'Escribe con ' + s.writers.join(', ') + '. ' + s.store + '.',
  };
}

function svcDetail(s) {
  const rows = [['tipo', s.kind === 'externo' ? 'servidor externo' :
                         s.ephemeral ? 'microVM efímera' : 'microVM persistente'],
                ['próxima llamada', s.latency],
                ['herramientas', String(s.tools.length)]];
  if (s.snapshot)  rows.push(['snapshot', s.snapshot], ['imagen', s.image],
                             ['memoria compartida', s.ramShared],
                             ['recursos', s.vcpus + ' vCPU · ' + s.memMiB + ' MiB']);
  if (s.url)       rows.push(['url', s.url]);
  rows.push(['instancias vivas', String(s.machines.length)]);
  return {rows, steps: s.flow, note: storeNote(s)};
}

function machDetail(m) {
  const rows = [['id', m.id], ['estado', m.state], ['ip', m.ip],
                ['salida', m.egress === 'internet' ? 'internet' : 'aislada'],
                ['recursos', m.vcpus + ' vCPU · ' + m.memMiB + ' MiB'],
                ['disco propio', m.disk], ['última operación', m.lastOp], ['edad', m.age]];
  for (const [k, v] of Object.entries(m.labels || {})) rows.push(['etiqueta ' + k, v]);
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
    label: 'Topología',
    hint: 'El host, sus servicios y las instancias vivas de cada uno.',
    legend: [['running', 'atendiendo'], ['warm', 'dormido'],
             ['ready', 'listo, sin instancia'], ['ext', 'externo']],
    build() {
      const kids = M.services.map(s => n('s:' + s.name, s.name, {
        cls: s.kind === 'externo' ? 'ext' : s.state,
        sub: plural(s.tools.length, 'herramienta', 'herramientas'),
        note: s.machines.length ? '' :
              s.kind === 'externo' ? 'no corre aquí' : 'sin instancias · ' + coste(s.latency),
        kids: svcKids(s), open: s.machines.length > 0,
        detail: svcDetail(s),
      }));
      return n('host', 'host', {cls: 'host', sub: M.host.endpoint, kids, open: true,
        detail: {rows: [['endpoint', M.host.endpoint],
                        ['firecracker', M.host.firecracker || '—'],
                        ['kvm', M.host.kvm ? 'sí' : 'no'],
                        ['memoria compartida', M.host.ramShared],
                        ['disco propio de las instancias', M.host.diskOwn],
                        ['servicio de memoria', mem || 'ninguno']]}});
    },
  },

  capas: {
    label: 'Capas',
    hint: 'Qué atraviesa una llamada, de la petición del modelo al servidor MCP.',
    legend: [['ready', 'capa de software'], ['running', 'proceso en marcha']],
    build() {
      const vm = n('l:vm', 'microVM Firecracker', {
        sub: 'un proceso por instancia', cls: 'ready',
        detail: {rows: [['aislamiento', 'KVM: no comparte kernel con el host'],
                        ['arranque', '~250 ms desde snapshot dorado'],
                        ['descongelar', '~30 ms']],
                 steps: ['Firecracker restaura el snapshot dorado',
                         'La memoria se mapea con backend File: las instancias comparten páginas',
                         'El disco base va de solo lectura; cada una escribe en su overlay']},
        kids: [
          n('l:kernel', 'kernel vmlinux', {sub: 'compartido por todas', cls: 'ready',
            detail: {rows: [['modo', 'solo lectura'], ['coste', 'una copia en caché para todas']]}}),
          n('l:base', 'rootfs base', {sub: '/dev/vda · solo lectura', cls: 'ready',
            detail: {rows: [['modo', 'solo lectura, compartido'],
                            ['por qué', 'una imagen sirve a N instancias sin duplicarse']]}}),
          n('l:overlay', 'overlay propio', {sub: '/dev/vdb · disperso', cls: 'ready',
            note: 'aquí acaba lo que escriba el invitado',
            detail: {rows: [['modo', 'lectura y escritura, por instancia'],
                            ['tamaño', 'disperso: solo ocupa lo que se escribe']],
                     note: {cls: 'warn', title: 'Esto no sobrevive a la instancia',
                            text: 'El overlay muere con la máquina. Lo que deba durar va a ' +
                                  (mem || 'un servicio de memoria enlazado') + '.'}}}),
          n('l:bridge', 'puente', {sub: 'stdin/stdout ↔ HTTP', cls: 'running',
            detail: {rows: [['papel', 'traduce la llamada MCP al proceso del invitado']],
                     steps: ['Recibe la llamada del gateway',
                             'La escribe en el stdin del servidor MCP',
                             'Lee la respuesta de su stdout y la devuelve']},
            kids: [n('l:mcp', 'servidor MCP', {sub: 'el binario original, sin tocar', cls: 'running',
              detail: {rows: [['transporte', 'stdio'],
                              ['modificaciones', 'ninguna: se ejecuta tal cual']]}})],
            open: true}),
        ], open: true});

      return n('l:gw', 'kling gateway', {cls: 'host', sub: 'un solo endpoint HTTP', open: true,
        detail: {rows: [['endpoint', M.host.endpoint],
                        ['servicios', String(M.services.length)]]},
        kids: [
          n('l:agg', 'agregador /mcp/_all', {sub: '3 meta-herramientas', cls: 'running',
            note: 'evita volcar todos los catálogos en el contexto',
            detail: {rows: [['por qué', 'exponer N catálogos enteros llena el contexto'],
                            ['a cambio', 'el inventario va en initialize.instructions']],
                     chips: ['find_tools', 'describe_tool', 'call_tool']},
            kids: [
              n('l:find', 'find_tools', {sub: 'busca por intención', cls: 'ready',
                detail: {rows: [['entrada', 'una consulta en lenguaje natural'],
                                ['extra', 'tabla de sinónimos español↔inglés']]}}),
              n('l:desc', 'describe_tool', {sub: 'devuelve el esquema', cls: 'ready',
                detail: {rows: [['entrada', 'nombre de herramienta'],
                                ['salida', 'su esquema de parámetros, solo cuando hace falta']]}}),
              n('l:call', 'call_tool', {sub: 'ejecuta', cls: 'ready',
                detail: {rows: [['extra', 'repara tipos contra el esquema declarado']]}}),
            ]}),
          n('l:pool', 'fondo pre-calentado', {sub: 'instancias listas de antemano', cls: 'running',
            note: 'convierte 250 ms en ~20 ms',
            detail: {rows: [['qué guarda', 'instancias con la sesión MCP ya abierta'],
                            ['para qué', 'la primera llamada no paga el arranque']]}}),
          vm,
          n('l:links', 'enlaces externos', {
            sub: plural(M.services.filter(s => s.kind === 'externo').length, 'servidor', 'servidores'),
            cls: 'ext', note: 'no arrancan ninguna máquina aquí',
            kids: M.services.filter(s => s.kind === 'externo').map(s =>
              n('s:' + s.name, s.name, {cls: 'ext', sub: s.url, detail: svcDetail(s)})),
            detail: {rows: [['qué son', 'servidores MCP que ya corren en otra parte'],
                            ['coste', 'el gateway solo reenvía']]}}),
        ]});
    },
  },

  mcp: {
    label: 'MCP',
    hint: 'El catálogo: qué herramientas ofrece cada servicio y cuáles escriben.',
    legend: [['running', 'lee'], ['warn', 'escribe'], ['ext', 'servidor externo']],
    build() {
      const kids = M.services.map(s => n('s:' + s.name, s.name, {
        cls: s.kind === 'externo' ? 'ext' : s.state,
        sub: plural(s.tools.length, 'herramienta', 'herramientas'),
        note: s.writers.length ? plural(s.writers.length, 'escribe', 'escriben') : 'solo lectura',
        detail: svcDetail(s),
        kids: s.tools.map(t => n('t:' + s.name + '.' + t.name, t.name, {
          cls: t.writes ? 'warn' : 'running', sub: t.writes ? 'escribe' : 'lee',
          note: t.desc,
          detail: {rows: [['servicio', s.name], ['efecto', t.writes ? 'escribe' : 'solo lectura'],
                          ['descripción', t.desc || '—']],
                   steps: s.flow,
                   note: t.writes ? storeNote(s) : null},
        })),
      }));
      const total = M.services.reduce((a, s) => a + s.tools.length, 0);
      return n('cat', 'catálogo', {cls: 'host', sub: plural(total, 'herramienta', 'herramientas'),
        kids, open: true,
        detail: {rows: [['servicios', String(M.services.length)],
                        ['herramientas', String(total)],
                        ['cómo se expone', 'inventario en initialize.instructions, no el catálogo entero']]}});
    },
  },

  red: {
    label: 'Red',
    hint: 'Quién puede salir a internet y quién está aislado.',
    legend: [['running', 'con salida'], ['ready', 'aislada'], ['ext', 'fuera del host']],
    build() {
      const live = M.services.flatMap(s => s.machines.map(m => ({s, m})));
      const kids = [];

      const externos = M.services.filter(s => s.kind === 'externo');
      if (externos.length) kids.push(n('r:ext', 'internet', {cls: 'ext',
        sub: plural(externos.length, 'servidor externo', 'servidores externos'),
        note: 'el gateway sale a buscarlos',
        kids: externos.map(s => n('s:' + s.name, s.name, {cls: 'ext', sub: s.url,
          detail: svcDetail(s)})),
        detail: {rows: [['dirección', 'saliente, iniciada por el gateway']]}}));

      if (!live.length) {
        kids.push(n('r:none', 'sin instancias', {cls: 'ready',
          sub: 'nada tiene red ahora mismo',
          note: 'cada instancia recibe su propio namespace al arrancar',
          detail: {rows: [['por qué', 'las microVM solo existen mientras se las usa']],
                   steps: ['Al arrancar, cada instancia recibe un namespace propio',
                           'Dentro todas ven el mismo tap0 y la misma 172.16.0.2',
                           'En el host las distingue su veth',
                           'La salida a internet está cerrada salvo que se pida']}}));
      }

      for (const {s, m} of live) {
        const open = m.egress === 'internet';
        kids.push(n('r:' + m.id, 'netns ' + m.id.slice(0, 8), {
          cls: open ? 'running' : 'ready',
          sub: s.name + ' · ' + m.name,
          note: open ? 'salida a internet permitida' : 'aislada',
          kids: [n('r:tap' + m.id, 'tap0 → 172.16.0.2', {cls: open ? 'running' : 'ready',
            sub: 'idéntico dentro de cada namespace',
            note: 'en el host lo distingue su veth (' + m.ip + ')',
            detail: {rows: [['ip del invitado', '172.16.0.2 (siempre la misma)'],
                            ['ip vista desde el host', m.ip],
                            ['por qué', 'un namespace por máquina permite reusar el mismo plan de red']]}})],
          detail: {rows: [['máquina', m.name], ['servicio', s.name],
                          ['ip', m.ip], ['salida', open ? 'internet' : 'aislada']],
                   note: open ? {cls: 'warn', title: 'Esta instancia puede salir a internet',
                                 text: 'Solo debería tenerlo lo que de verdad lo necesite.'} : null}}));
      }

      return n('r:host', 'host', {cls: 'host', sub: M.host.endpoint, kids, open: true,
        detail: {rows: [['papel', 'enruta, filtra y aísla'],
                        ['política', 'sin salida a internet salvo que se pida explícitamente']]}});
    },
  },
};
`
