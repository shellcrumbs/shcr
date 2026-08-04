// Applies a stored theme preference before the page is painted.
//
// This is a separate file loaded from <head> without defer, rather than a few
// lines inside app.js at the end of <body>, for one reason: app.js runs after
// the document is parsed, so a stored preference that disagrees with the system
// would be applied *after* the first paint — a flash of the wrong theme on
// every load. The usual fix is an inline <script>, which the page's CSP
// deliberately forbids.
(function () {
  try {
    var pref = localStorage.getItem("shcr-theme");
    if (pref === "light" || pref === "dark") {
      document.documentElement.setAttribute("data-theme", pref);
    }
  } catch (e) {
    // Private mode, storage disabled, anything else: the system setting is a
    // perfectly good answer and needs nothing from us.
  }
})();
