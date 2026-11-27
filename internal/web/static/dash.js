// Admin UI: theme switch and filters. Sorting, the group filter,
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
    // After htmx changed a list the address bar has the current filters;
    // export exactly that (the server ignores the page).
    var csv = e.target.closest('a[data-csv]');
    if (csv) csv.href = csv.getAttribute('data-csv') + window.location.search;
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

  // The dashboard's dates (its period and the matrix's) are remembered for
  // this browser tab, so going to another tab of the new UI and back does
  // not lose them. They live in sessionStorage, which the browser clears
  // when the tab is closed, and they are dropped on a reload. The address of
  // the dashboard is what counts: whatever dates it shows are the ones kept,
  // and "Reset" (no dates) forgets them.
  var DATES = ['from', 'to', 'mfrom', 'mto'];
  var STORE = 'att-dash-dates';
  var onDashboard = window.location.pathname === '/admin/dashboard';

  function navType() {
    try {
      var nav = performance.getEntriesByType('navigation')[0];
      if (nav) return nav.type;
      if (performance.navigation && performance.navigation.type === 1) return 'reload';
    } catch (err) { /* old browser */ }
    return 'navigate';
  }
  function loadDates() {
    try { return new URLSearchParams(sessionStorage.getItem(STORE) || ''); } catch (err) { return new URLSearchParams(); }
  }
  function saveDates() {
    var q = new URLSearchParams(window.location.search);
    var keep = new URLSearchParams();
    DATES.forEach(function (k) { if (q.get(k)) keep.set(k, q.get(k)); });
    try {
      if (keep.toString()) sessionStorage.setItem(STORE, keep.toString());
      else sessionStorage.removeItem(STORE);
    } catch (err) { /* storage blocked: nothing is remembered */ }
  }
  // The Dashboard tab link carries the remembered dates.
  function updateTabLink() {
    var dates = loadDates().toString();
    document.querySelectorAll('.tabs a[data-dash-tab]').forEach(function (a) {
      a.setAttribute('href', '/admin/dashboard' + (dates ? '?' + dates : ''));
    });
  }

  if (navType() === 'reload') {
    try { sessionStorage.removeItem(STORE); } catch (err) { /* ignore */ }
  }
  if (onDashboard) saveDates();
  updateTabLink();
  // htmx changes the matrix dates without a page load (hx-push-url).
  ['htmx:pushedIntoHistory', 'htmx:historyRestore'].forEach(function (ev) {
    document.addEventListener(ev, function () {
      if (onDashboard) saveDates();
      updateTabLink();
    });
  });
  // Back/forward can show a page from the browser's cache without running
  // this script again; its tab link may be out of date.
  window.addEventListener('pageshow', function (e) { if (e.persisted) updateTabLink(); });

  // The dashboard's date/status filter reloads the page: keep the list
  // settings as they are now (htmx may have changed them since the page
  // loaded), but start both lists on their first page.
  var KEEP = ['group', 'q', 'sort', 'dir', 'per', 'mper', 'mfrom', 'mto'];
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
