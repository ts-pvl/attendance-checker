// Applies the saved light/dark choice before the page paints (loaded
// synchronously in <head>). Without a saved choice the system setting wins.
(function () {
  try {
    var t = localStorage.getItem('att-theme');
    if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  } catch (e) { /* storage blocked: follow the system */ }
})();
