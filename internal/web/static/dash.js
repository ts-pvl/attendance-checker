// Analytics dashboard: theme switch, filters, table search and sorting.
// The page works without it except for search and sorting.
(function () {
  var root = document.documentElement;

  function currentTheme() {
    var t = root.getAttribute('data-theme');
    if (t) return t;
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  document.addEventListener('click', function (e) {
    if (e.target.closest('[data-theme-toggle]')) {
      var next = currentTheme() === 'dark' ? 'light' : 'dark';
      root.setAttribute('data-theme', next);
      try { localStorage.setItem('att-theme', next); } catch (err) { /* ignore */ }
      return;
    }
    var b = e.target.closest('th button[data-sort]');
    if (b) sortBy(b);
  });

  // Changing a date reloads the dashboard.
  document.addEventListener('change', function (e) {
    if (e.target.matches('form[data-autosubmit] input[type=date]') && e.target.value) e.target.form.submit();
  });

  document.addEventListener('input', function (e) {
    var id = e.target.getAttribute('data-table-search');
    if (!id) return;
    var q = e.target.value.trim().toLowerCase();
    document.querySelectorAll('#' + id + ' tbody tr').forEach(function (tr) {
      tr.hidden = q !== '' && tr.getAttribute('data-name').toLowerCase().indexOf(q) < 0;
    });
  });

  function sortBy(btn) {
    var table = btn.closest('table');
    var col = +btn.getAttribute('data-sort');
    var num = btn.getAttribute('data-type') === 'num';
    // First click on a numeric column sorts biggest first, text A→Z.
    var cur = btn.getAttribute('aria-sort');
    var dir = cur ? (cur === 'descending' ? 1 : -1) : (num ? -1 : 1);
    table.querySelectorAll('th button[aria-sort]').forEach(function (x) { x.removeAttribute('aria-sort'); });
    btn.setAttribute('aria-sort', dir < 0 ? 'descending' : 'ascending');
    var body = table.tBodies[0];
    var rows = Array.prototype.slice.call(body.rows);
    rows.sort(function (a, b) {
      var x = a.cells[col].getAttribute('data-v'), y = b.cells[col].getAttribute('data-v');
      if (num) return dir * (parseFloat(x) - parseFloat(y));
      return dir * x.localeCompare(y, root.lang);
    });
    rows.forEach(function (r) { body.appendChild(r); });
  }
})();
