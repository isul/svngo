// SvnGo UI helpers (no inline scripts: CSP script-src 'self').
(function () {
  "use strict";

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-copy-btn]");
    if (btn) {
      var input = btn.parentElement.querySelector("[data-copy]");
      if (!input) return;
      input.select();
      var done = function () {
        var old = btn.textContent;
        btn.textContent = btn.getAttribute("data-copied") || "Copied";
        setTimeout(function () { btn.textContent = old; }, 1500);
      };
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(input.value).then(done, function () { document.execCommand("copy"); done(); });
      } else {
        document.execCommand("copy");
        done();
      }
      return;
    }
    // close the mobile menu after navigating
    if (ev.target.closest(".sidebar a")) {
      var t = document.getElementById("nav-toggle");
      if (t) t.checked = false;
    }
  });

  document.addEventListener("focusin", function (ev) {
    if (ev.target.matches && ev.target.matches("[data-copy]")) ev.target.select();
  });

  // highlight the line referenced by #L123
  function markLine() {
    document.querySelectorAll("tr.hl").forEach(function (r) { r.classList.remove("hl"); });
    if (/^#L\d+$/.test(location.hash)) {
      var row = document.getElementById(location.hash.slice(1));
      if (row) row.classList.add("hl");
    }
  }
  window.addEventListener("hashchange", markLine);
  document.addEventListener("DOMContentLoaded", markLine);
  document.addEventListener("htmx:afterSettle", markLine);
})();
