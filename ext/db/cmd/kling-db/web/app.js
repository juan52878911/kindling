'use strict';
// Todo lo que viene del servidor (modelo o base de datos) entra con textContent:
// nunca como HTML.
(function () {
  var meta = document.querySelector('meta[name="csrf"]');
  var token = meta ? meta.content : '';
  var $ = function (id) { return document.getElementById(id); };
  var pending = null;

  function post(path, body) {
    return fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF': token },
      body: JSON.stringify(body)
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) { throw new Error(j.error || ('HTTP ' + r.status)); }
        return j;
      });
    });
  }
  function busy(on, msg) {
    $('go').disabled = on; $('run').disabled = on; $('discard').disabled = on;
    $('status').textContent = on ? msg : '';
  }
  function fail(e) { var p = $('error'); p.textContent = e.message; p.hidden = false; }
  function clearError() { $('error').hidden = true; }
  function reset() { pending = null; $('review').hidden = true; $('result').hidden = true; }

  fetch('/api/info', { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (i) {
    $('info').textContent = 'Copy ' + i.copy + ', read-only role ' + i.role + '. The model (' + i.provider + ') gets the schema and your question, no data.';
    if (i.explain) {
      var n = $('notice');
      n.textContent = 'Summaries are on: up to ' + i.explain_rows + ' rows of each result are sent to ' + i.provider + '.';
      n.hidden = false;
    }
  }).catch(fail);

  $('ask').addEventListener('submit', function (ev) {
    ev.preventDefault(); clearError(); reset();
    busy(true, 'Asking the model...');
    post('/api/propose', { question: $('q').value }).then(function (j) {
      pending = j.id;
      $('sql').textContent = j.sql;
      $('review').hidden = false;
    }).catch(fail).then(function () { busy(false); });
  });
  $('discard').addEventListener('click', function () { reset(); clearError(); });
  $('run').addEventListener('click', function () {
    if (!pending) { return; }
    var id = pending; clearError();
    busy(true, 'Running...');
    post('/api/run', { id: id, confirm: true }).then(function (j) {
      pending = null; $('review').hidden = true;
      var head = document.querySelector('#result thead'), body = document.querySelector('#result tbody');
      head.textContent = ''; body.textContent = '';
      var tr = document.createElement('tr');
      j.columns.forEach(function (c) { var th = document.createElement('th'); th.textContent = c; tr.appendChild(th); });
      head.appendChild(tr);
      j.rows.forEach(function (row) {
        var r = document.createElement('tr');
        row.forEach(function (c) { var td = document.createElement('td'); td.textContent = c; r.appendChild(td); });
        body.appendChild(r);
      });
      $('count').textContent = j.rows.length + (j.rows.length === 1 ? ' row' : ' rows') + (j.truncated ? ' (the limit: there may be more)' : '');
      var s = $('summary'); s.textContent = j.summary || ''; s.hidden = !j.summary;
      $('result').hidden = false;
    }).catch(function (e) { pending = null; $('review').hidden = true; fail(e); }).then(function () { busy(false); });
  });
})();
