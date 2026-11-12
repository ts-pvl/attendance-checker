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

  // Changing a date reloads the dashboard.
  document.addEventListener('change', function (e) {
    if (e.target.matches('form[data-autosubmit] input[type=date]') && e.target.value) e.target.form.submit();
  });
})();
