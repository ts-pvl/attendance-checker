// Analytics dashboard: theme switch and filters. Sorting, the group filter,
// search and paging of the students table happen on the server; htmx (see
// the hx-* attributes) swaps in just the table. Without JavaScript the same
// links and forms reload the page.
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
    // After htmx changed the table the address bar has the current filters;
    // export exactly that (the server ignores the page).
    var csv = e.target.closest('a[data-csv]');
    if (csv) csv.href = '/admin/dashboard.csv' + window.location.search;
  });

  // Filters apply as soon as a date, menu or checkbox changes. requestSubmit
  // goes through htmx where the form is boosted.
  document.addEventListener('change', function (e) {
    var f = e.target.form;
    if (!f || !f.hasAttribute('data-autosubmit')) return;
    if (e.target.type === 'date' && !e.target.value) return;
    if (e.target.matches('input[type=date], select, input[type=checkbox]')) {
      if (f.requestSubmit) f.requestSubmit(); else f.submit();
    }
  });

  // The dashboard's date/status filter reloads the page: keep the list
  // settings as they are now (htmx may have changed them since the page
  // loaded), but start both lists on their first page.
  var KEEP = ['group', 'q', 'sort', 'dir', 'per', 'mper'];
  document.addEventListener('submit', function (e) {
    var f = e.target;
    if (!f.querySelector('[data-keep]') && !f.hasAttribute('data-keep-url')) return;
    f.querySelectorAll('[data-keep]').forEach(function (x) { x.remove(); });
    var q = new URLSearchParams(window.location.search);
    KEEP.forEach(function (k) {
      q.getAll(k).forEach(function (v) {
        var i = document.createElement('input');
        i.type = 'hidden'; i.name = k; i.value = v; i.setAttribute('data-keep', '');
        f.appendChild(i);
      });
    });
  }, true);
})();
