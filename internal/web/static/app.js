// costblame UI behavior: count-up animation for real server-rendered numbers
// and a settle flash after htmx swaps. No data originates here — every
// number animated is read from a data-countup attribute the server already
// filled in with a real query result.
(function () {
  "use strict";

  function runCountUps(root) {
    var scope = root && root.querySelectorAll ? root : document;
    var els = scope.querySelectorAll("[data-countup]");
    for (var i = 0; i < els.length; i++) {
      animateOne(els[i]);
    }
  }

  function animateOne(el) {
    var target = parseInt(el.getAttribute("data-countup"), 10);
    if (isNaN(target)) return;
    if (el.getAttribute("data-countup-done") === String(target)) return;
    el.setAttribute("data-countup-done", String(target));

    var duration = 650;
    var start = null;

    function tick(ts) {
      if (start === null) start = ts;
      var p = Math.min((ts - start) / duration, 1);
      var eased = 1 - Math.pow(1 - p, 3); // ease-out-cubic
      el.textContent = String(Math.round(target * eased));
      if (p < 1) {
        window.requestAnimationFrame(tick);
      } else {
        el.textContent = String(target);
      }
    }
    window.requestAnimationFrame(tick);
  }

  function flashSettled(target) {
    if (!target || !target.classList) return;
    target.classList.add("just-settled");
    window.setTimeout(function () {
      target.classList.remove("just-settled");
    }, 900);
  }

  function staggerRows(root) {
    var scope = root && root.querySelectorAll ? root : document;
    var rows = scope.querySelectorAll("table tbody tr:not([data-staggered])");
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      row.setAttribute("data-staggered", "1");
      row.style.animationDelay = Math.min(i * 35, 350) + "ms";
      row.classList.add("row-enter");
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    runCountUps(document);
    staggerRows(document);
  });

  document.body.addEventListener("htmx:afterSettle", function (evt) {
    runCountUps(evt.target);
    staggerRows(evt.target);
    flashSettled(evt.detail && evt.detail.target);
  });
})();
