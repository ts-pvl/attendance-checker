// Check-in screen: polls /qr/state, draws the current Telegram link as a QR
// code and shows a countdown until the code rotates.
(function () {
  var app = document.getElementById('qr-app');
  var qrView = document.getElementById('qr-view');
  var loginView = document.getElementById('login-view');
  var code = document.getElementById('code');
  var bar = document.getElementById('bar');
  var countdown = document.getElementById('countdown');
  var offline = document.getElementById('offline');
  var checked = document.getElementById('checked');
  var checkedList = document.getElementById('checked-list');
  var refreshText = app.dataset.refresh;
  offline.textContent = app.dataset.offline;

  var current = '';
  var rotateAt = 0;
  var period = 30000;
  var failures = 0;

  function show(state) {
    qrView.hidden = state !== 'qr';
    loginView.hidden = state === 'qr';
  }

  function render(s) {
    show(s.state);
    if (s.state !== 'qr') {
      current = '';
      code.textContent = '';
      return;
    }
    if (s.link !== current) {
      current = s.link;
      code.textContent = '';
      code.appendChild(window.qrSVG(s.link, 'M'));
    }
    rotateAt = Date.now() + s.rotate_ms;
    period = s.period_ms || period;
    checked.hidden = s.checked_in.length === 0;
    checkedList.textContent = '';
    s.checked_in.forEach(function (c) {
      var li = document.createElement('li');
      li.textContent = '✓ ' + c.name + ' · ' + c.since;
      checkedList.appendChild(li);
    });
  }

  function poll() {
    fetch('/qr/state', { cache: 'no-store' })
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (s) {
        failures = 0;
        offline.hidden = true;
        render(s);
      })
      .catch(function () {
        failures++;
        if (failures >= 2) {
          // Never leave a stale code on screen while the server is unreachable.
          offline.hidden = false;
          current = '';
          code.textContent = '';
          show('login');
        }
      })
      .finally(function () {
        setTimeout(poll, 2000);
      });
  }

  function tick() {
    var left = Math.max(0, rotateAt - Date.now());
    countdown.textContent = refreshText.replace('{s}', Math.ceil(left / 1000));
    bar.style.width = Math.min(100, (100 * left) / period) + '%';
  }

  setInterval(tick, 250);
  poll();
})();
