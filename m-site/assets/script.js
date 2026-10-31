/* CapScope marketing site — behaviour */
(function () {
  "use strict";

  /* ---------- Theme ---------- */
  var STORAGE_KEY = "capscope-theme";

  function systemTheme() {
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches
      ? "light"
      : "dark";
  }

  function applyTheme(theme) {
    document.documentElement.setAttribute("data-theme", theme);
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute("content", theme === "light" ? "#ffffff" : "#08090c");
    document.querySelectorAll("[data-theme-toggle]").forEach(function (btn) {
      btn.setAttribute(
        "aria-label",
        theme === "light" ? "Switch to dark mode" : "Switch to light mode"
      );
      btn.setAttribute("title", theme === "light" ? "Dark mode" : "Light mode");
    });
  }

  var stored = null;
  try {
    stored = localStorage.getItem(STORAGE_KEY);
  } catch (e) {}
  applyTheme(stored === "light" || stored === "dark" ? stored : systemTheme());

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-theme-toggle]");
    if (!btn) return;
    var next =
      document.documentElement.getAttribute("data-theme") === "light" ? "dark" : "light";
    applyTheme(next);
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch (e) {}
  });

  /* ---------- Mobile navigation ---------- */
  document.addEventListener("click", function (ev) {
    var toggle = ev.target.closest("[data-nav-toggle]");
    if (toggle) {
      var menu = document.getElementById("nav-links");
      if (!menu) return;
      var open = menu.classList.toggle("open");
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
      return;
    }
    var link = ev.target.closest("#nav-links a");
    if (link) {
      var m = document.getElementById("nav-links");
      if (m) m.classList.remove("open");
    }
  });

  /* ---------- Copy buttons on code blocks ---------- */
  document.querySelectorAll("[data-copy]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var block = btn.closest(".code-block");
      var code = block && block.querySelector("pre code, pre");
      if (!code) return;
      var text = code.innerText.replace(/\n$/, "");
      var done = function () {
        var label = btn.querySelector("[data-copy-label]");
        if (label) {
          var prev = label.textContent;
          label.textContent = "Copied";
          setTimeout(function () {
            label.textContent = prev;
          }, 1600);
        }
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () {});
      } else {
        var ta = document.createElement("textarea");
        ta.value = text;
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        try {
          document.execCommand("copy");
          done();
        } catch (e) {}
        document.body.removeChild(ta);
      }
    });
  });

  /* ---------- GitHub star count ---------- */
  document.querySelectorAll("[data-gh-count]").forEach(function (el) {
    fetch("https://api.github.com/repos/hacrex/CapScope", {
      headers: { Accept: "application/vnd.github+json" },
    })
      .then(function (r) {
        return r.ok ? r.json() : null;
      })
      .then(function (data) {
        if (data && typeof data.stargazers_count === "number") {
          var n = data.stargazers_count;
          el.textContent = n >= 1000 ? (n / 1000).toFixed(1).replace(/\.0$/, "") + "k" : String(n);
          el.setAttribute("title", n.toLocaleString() + " stars on GitHub");
        }
      })
      .catch(function () {
        /* offline or rate-limited — keep the static label */
      });
  });

  /* ---------- Reveal on scroll ---------- */
  var revealables = document.querySelectorAll(".reveal");
  if ("IntersectionObserver" in window && revealables.length) {
    var io = new IntersectionObserver(
      function (entries) {
        entries.forEach(function (entry) {
          if (entry.isIntersecting) {
            entry.target.classList.add("in");
            io.unobserve(entry.target);
          }
        });
      },
      { rootMargin: "0px 0px -8% 0px", threshold: 0.06 }
    );
    revealables.forEach(function (el) {
      io.observe(el);
    });
  } else {
    revealables.forEach(function (el) {
      el.classList.add("in");
    });
  }

  /* ---------- Sidebar scrollspy (docs "on this page" lists) ---------- */
  (function () {
    var links = Array.prototype.slice.call(
      document.querySelectorAll('.sidebar a[href^="#"]')
    );
    if (!links.length || !("IntersectionObserver" in window)) return;

    var map = {};
    var targets = [];
    links.forEach(function (a) {
      var id = decodeURIComponent(a.getAttribute("href").slice(1));
      var el = document.getElementById(id);
      if (el) {
        map[id] = a;
        targets.push(el);
      }
    });
    if (!targets.length) return;

    var visible = {};
    var io = new IntersectionObserver(
      function (entries) {
        entries.forEach(function (e) {
          visible[e.target.id] = e.isIntersecting;
        });
        var current = null;
        targets.forEach(function (t) {
          if (visible[t.id] && !current) current = t.id;
        });
        if (!current) {
          // fall back to the last heading above the fold
          for (var i = targets.length - 1; i >= 0; i--) {
            if (targets[i].getBoundingClientRect().top < 140) {
              current = targets[i].id;
              break;
            }
          }
        }
        links.forEach(function (a) {
          a.classList.remove("active");
          a.removeAttribute("aria-current");
        });
        if (current && map[current]) {
          map[current].classList.add("active");
          map[current].setAttribute("aria-current", "location");
        }
      },
      { rootMargin: "-96px 0px -70% 0px", threshold: 0 }
    );
    targets.forEach(function (t) {
      io.observe(t);
    });
  })();

  /* ---------- Icons ---------- */
  function drawIcons() {
    if (window.lucide && typeof window.lucide.createIcons === "function") {
      window.lucide.createIcons({ attrs: { "aria-hidden": "true" } });
    }
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", drawIcons);
  } else {
    drawIcons();
  }
  window.addEventListener("load", drawIcons);

  /* ---------- Header shadow after scroll ---------- */
  var header = document.querySelector(".site-header");
  if (header) {
    var onScroll = function () {
      header.style.boxShadow = window.scrollY > 6 ? "0 10px 30px -22px rgba(0,0,0,.9)" : "none";
    };
    onScroll();
    window.addEventListener("scroll", onScroll, { passive: true });
  }
})();
