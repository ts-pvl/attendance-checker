// Small progressive enhancements for the admin pages. Everything works
// without JavaScript except the QR images on the slips page.
document.addEventListener('submit', function (e) {
  var msg = e.target.getAttribute('data-confirm');
  if (msg && !window.confirm(msg)) e.preventDefault();
});

document.addEventListener('click', function (e) {
  if (e.target.closest('[data-print]')) window.print();
});

document.addEventListener('DOMContentLoaded', function () {
  document.querySelectorAll('[data-qr]').forEach(function (el) {
    if (window.qrSVG) el.appendChild(window.qrSVG(el.getAttribute('data-qr'), 'M'));
  });
});
