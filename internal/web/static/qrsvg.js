// qrSVG renders text as a QR code <svg> element using qrcode-generator.
// Built with DOM calls (no innerHTML, no inline styles) so it works under
// the strict Content-Security-Policy.
window.qrSVG = function (text, level) {
  var qr = qrcode(0, level || 'M');
  qr.addData(text);
  qr.make();
  var n = qr.getModuleCount();
  var quiet = 4;
  var size = n + 2 * quiet;
  var ns = 'http://www.w3.org/2000/svg';
  var svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 ' + size + ' ' + size);
  svg.setAttribute('shape-rendering', 'crispEdges');
  var bg = document.createElementNS(ns, 'rect');
  bg.setAttribute('width', size);
  bg.setAttribute('height', size);
  bg.setAttribute('fill', '#fff');
  svg.appendChild(bg);
  var d = '';
  for (var r = 0; r < n; r++) {
    for (var c = 0; c < n; c++) {
      if (qr.isDark(r, c)) d += 'M' + (c + quiet) + ' ' + (r + quiet) + 'h1v1h-1z';
    }
  }
  var path = document.createElementNS(ns, 'path');
  path.setAttribute('d', d);
  path.setAttribute('fill', '#000');
  svg.appendChild(path);
  return svg;
};
