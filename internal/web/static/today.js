// Today tab: drag a PC onto another cell of the grid to swap the two (or to
// move it to an empty cell). The drop fills in the hidden #pc-swap form and
// sends it; the server saves the new layout and htmx swaps in the grid.
// Only admins get draggable tiles and the form.
(function () {
  var dragged = null; // the tile being dragged

  function element(t) { return t && t.nodeType === 1 ? t : t && t.parentElement; }
  function cellOf(t) { var el = element(t); return el ? el.closest('[data-cell]') : null; }
  function clearTarget() {
    document.querySelectorAll('.pccell.over').forEach(function (c) { c.classList.remove('over'); });
  }
  function finish() {
    if (dragged) dragged.classList.remove('dragging');
    dragged = null;
    clearTarget();
  }

  document.addEventListener('dragstart', function (e) {
    var el = element(e.target);
    var pc = el && el.closest('.pc[draggable="true"]');
    if (!pc || !document.getElementById('pc-swap')) return;
    dragged = pc;
    pc.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', pc.getAttribute('data-host'));
  });

  document.addEventListener('dragover', function (e) {
    var cell = dragged && cellOf(e.target);
    if (!cell) return;
    e.preventDefault(); // allow the drop
    e.dataTransfer.dropEffect = 'move';
    if (!cell.classList.contains('over')) {
      clearTarget();
      if (cell !== cellOf(dragged)) cell.classList.add('over');
    }
  });

  document.addEventListener('drop', function (e) {
    var cell = dragged && cellOf(e.target);
    if (!cell) return;
    e.preventDefault();
    var host = dragged.getAttribute('data-host');
    var same = cell === cellOf(dragged);
    finish();
    if (same) return;
    var form = document.getElementById('pc-swap');
    form.elements.host.value = host;
    form.elements.to.value = cell.getAttribute('data-cell');
    if (form.requestSubmit) form.requestSubmit(); else form.submit();
  });

  document.addEventListener('dragend', finish);

  // The page reloads the grid every 30 s: not while a tile is in the air.
  document.addEventListener('htmx:beforeSwap', function (e) {
    if (dragged) e.detail.shouldSwap = false;
  });
})();
