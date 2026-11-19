'use strict';
// Consola mínima. Todo texto que viene del servidor o del invitado se
// inserta con textContent; nunca se construye HTML a partir de él.
var main = document.getElementById('main');

function token() { return sessionStorage.getItem('kling-token') || ''; }

function el(tag, props, kids) {
  var e = document.createElement(tag);
  Object.keys(props || {}).forEach(function (k) {
    if (k === 'text') e.textContent = props[k]; else e.setAttribute(k, props[k]);
  });
  (kids || []).forEach(function (c) { e.appendChild(c); });
  return e;
}
function limpia() { while (main.firstChild) main.removeChild(main.firstChild); }
function aviso(msg) { main.insertBefore(el('p', { 'class': 'error', text: msg }), main.firstChild); }

function api(method, path, body, raw) {
  var h = { Authorization: 'Bearer ' + token() };
  var init = { method: method, headers: h };
  if (body !== undefined) {
    if (raw) { init.body = body; } else { h['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
  }
  return fetch('/v1' + path, init).then(function (r) {
    if (!r.ok) return r.text().then(function (t) { throw new Error(r.status + ' ' + t); });
    return r;
  });
}
function json(method, path, body) {
  return api(method, path, body).then(function (r) { return r.status === 204 ? null : r.json(); });
}
function b64(s) { return btoa(String.fromCharCode.apply(null, new TextEncoder().encode(s))); }
function deb64(s) {
  var bin = atob(s), u = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
  return new TextDecoder().decode(u);
}

function login() {
  limpia();
  var t = el('input', { type: 'password', placeholder: 'token de inquilino', size: '50' });
  var b = el('button', { text: 'Entrar' });
  b.addEventListener('click', function () {
    sessionStorage.setItem('kling-token', t.value); ruta();
  });
  main.appendChild(el('p', { text: 'Pega tu token. Solo se guarda en esta pestaña (sessionStorage).' }));
  main.appendChild(t); main.appendChild(b);
}

function celda(txt) { return el('td', { text: txt == null ? '' : String(txt) }); }

function vistaLista() {
  limpia();
  main.appendChild(el('h2', { text: 'Sandboxes' }));
  var tabla = el('table');
  main.appendChild(tabla);
  json('GET', '/sandboxes').then(function (lista) {
    tabla.appendChild(el('tr', {}, ['id', 'estado', 'plantilla', 'expira', ''].map(function (h) { return el('th', { text: h }); })));
    lista.forEach(function (s) {
      var ver = el('a', { href: '#/s/' + s.id, text: s.id });
      var td = el('td'); td.appendChild(ver);
      var del = el('button', { text: 'Borrar' });
      del.addEventListener('click', function () {
        json('DELETE', '/sandboxes/' + s.id).then(vistaLista, function (e) { aviso(e.message); });
      });
      var tdb = el('td'); tdb.appendChild(del);
      tabla.appendChild(el('tr', {}, [td, celda(s.state), celda(s.template || s.image), celda(s.expires_at), tdb]));
    });
  }, function (e) { aviso(e.message); });
  var plant = el('input', { placeholder: 'plantilla', size: '20' });
  var ttl = el('input', { placeholder: 'ttl (s)', size: '6' });
  var crear = el('button', { text: 'Crear' });
  crear.addEventListener('click', function () {
    var b = { template: plant.value };
    if (ttl.value) b.ttl_seconds = parseInt(ttl.value, 10);
    json('POST', '/sandboxes', b).then(vistaLista, function (e) { aviso(e.message); });
  });
  main.appendChild(el('fieldset', {}, [el('legend', { text: 'Crear desde plantilla' }), plant, ttl, crear]));
}

function vistaPlantillas() {
  limpia();
  main.appendChild(el('h2', { text: 'Plantillas' }));
  json('GET', '/templates').then(function (d) {
    main.appendChild(el('pre', { text: JSON.stringify(d, null, 2) }));
  }, function (e) { aviso(e.message); });
}

function salida(pre, txt, err) {
  var s = el('span', { text: txt });
  if (err) s.setAttribute('class', 'err');
  pre.appendChild(s);
}

function vistaDetalle(id) {
  limpia();
  main.appendChild(el('h2', { text: 'Sandbox ' + id }));
  var info = el('pre', { text: 'cargando…' });
  main.appendChild(info);
  json('GET', '/sandboxes/' + id).then(function (s) {
    info.textContent = JSON.stringify(s, null, 2);
  }, function (e) { info.textContent = e.message; });

  var renovar = el('button', { text: 'Renovar' });
  renovar.addEventListener('click', function () {
    json('POST', '/sandboxes/' + id + '/renew', {}).then(function () { vistaDetalle(id); }, function (e) { aviso(e.message); });
  });
  main.appendChild(renovar);

  // exec en flujo NDJSON
  var cmd = el('input', { placeholder: 'comando (sh -c)', size: '60' });
  var run = el('button', { text: 'Ejecutar' });
  var out = el('pre');
  run.addEventListener('click', function () {
    out.textContent = '';
    api('POST', '/sandboxes/' + id + '/exec', { cmd: ['sh', '-c', cmd.value] }).then(function (r) {
      var rd = r.body.getReader(), dec = new TextDecoder(), buf = '';
      function linea(l) {
        if (!l) return;
        var ev = JSON.parse(l);
        if (ev.data) salida(out, deb64(ev.data), ev.stream === 'stderr');
        if (ev.error) salida(out, '\n[error] ' + ev.error + '\n', true);
        if (ev.exit != null) salida(out, '\n[exit ' + ev.exit + ']\n');
      }
      function paso() {
        return rd.read().then(function (x) {
          if (x.done) { linea(buf); return; }
          buf += dec.decode(x.value, { stream: true });
          var ls = buf.split('\n'); buf = ls.pop(); ls.forEach(linea);
          return paso();
        });
      }
      return paso();
    }).catch(function (e) { salida(out, e.message, true); });
  });
  main.appendChild(el('fieldset', {}, [el('legend', { text: 'Exec' }), cmd, run, out]));

  // procesos y logs
  var procs = el('pre');
  var verp = el('button', { text: 'Listar procesos' });
  verp.addEventListener('click', function () {
    json('GET', '/sandboxes/' + id + '/processes').then(function (d) {
      procs.textContent = JSON.stringify(d, null, 2);
    }, function (e) { procs.textContent = e.message; });
  });
  var pid = el('input', { placeholder: 'pid', size: '6' });
  var logs = el('button', { text: 'Ver logs' });
  logs.addEventListener('click', function () {
    api('GET', '/sandboxes/' + id + '/processes/' + encodeURIComponent(pid.value) + '/output')
      .then(function (r) { return r.text(); })
      .then(function (t) { procs.textContent = t; }, function (e) { procs.textContent = e.message; });
  });
  main.appendChild(el('fieldset', {}, [el('legend', { text: 'Procesos y logs' }), verp, pid, logs, procs]));

  // ficheros
  var ruta = el('input', { placeholder: '/ruta', size: '40', value: '/' });
  var ls = el('button', { text: 'Listar' });
  var bajar = el('button', { text: 'Ver/bajar' });
  var fich = el('input', { type: 'file' });
  var subir = el('button', { text: 'Subir' });
  var fo = el('pre');
  ls.addEventListener('click', function () {
    api('POST', '/sandboxes/' + id + '/exec', { cmd: ['ls', '-la', ruta.value] }).then(function (r) { return r.text(); })
      .then(function (t) {
        fo.textContent = '';
        t.split('\n').forEach(function (l) { if (l) { var ev = JSON.parse(l); if (ev.data) fo.appendChild(document.createTextNode(deb64(ev.data))); } });
      }, function (e) { fo.textContent = e.message; });
  });
  bajar.addEventListener('click', function () {
    api('GET', '/sandboxes/' + id + '/files?path=' + encodeURIComponent(ruta.value)).then(function (r) { return r.blob(); })
      .then(function (b) {
        var u = URL.createObjectURL(b);
        var a = el('a', { href: u, download: ruta.value.split('/').pop() || 'fichero' });
        document.body.appendChild(a); a.click(); document.body.removeChild(a);
        URL.revokeObjectURL(u);
      }, function (e) { fo.textContent = e.message; });
  });
  subir.addEventListener('click', function () {
    if (!fich.files[0]) return;
    api('PUT', '/sandboxes/' + id + '/files?mkdir=1&path=' + encodeURIComponent(ruta.value), fich.files[0], true)
      .then(function () { fo.textContent = 'subido'; }, function (e) { fo.textContent = e.message; });
  });
  main.appendChild(el('fieldset', {}, [el('legend', { text: 'Ficheros' }), ruta, ls, bajar, fich, subir, fo]));

  // endpoint de entrada
  var puerto = el('input', { placeholder: 'puerto', size: '6' });
  var ep = el('button', { text: 'Crear endpoint' });
  var epo = el('pre');
  ep.addEventListener('click', function () {
    json('POST', '/sandboxes/' + id + '/endpoints', { port: parseInt(puerto.value, 10) }).then(function (d) {
      epo.textContent = JSON.stringify(d, null, 2);
    }, function (e) { epo.textContent = e.message; });
  });
  main.appendChild(el('fieldset', {}, [el('legend', { text: 'Entrada' }), puerto, ep, epo]));
}

function ruta() {
  document.getElementById('sesion').textContent = '';
  if (!token()) return login();
  var salir = el('button', { text: 'Salir' });
  salir.addEventListener('click', function () { sessionStorage.removeItem('kling-token'); ruta(); });
  document.getElementById('sesion').appendChild(salir);
  var h = location.hash.replace(/^#\/?/, '');
  if (h.indexOf('s/') === 0) return vistaDetalle(h.slice(2));
  if (h === 'plantillas') return vistaPlantillas();
  vistaLista();
}
window.addEventListener('hashchange', ruta);
ruta();
