/* ==========================================================================
   CapScope — m-site shared behavior
   ========================================================================== */
(function () {
  "use strict";

  // ---- Theme toggle ----
  const themeToggle = document.querySelector(".theme-toggle");
  const htmlEl = document.documentElement;
  const savedTheme = localStorage.getItem("theme") || "light";

  htmlEl.setAttribute("data-theme", savedTheme);
  if (themeToggle) {
    themeToggle.innerHTML = savedTheme === "dark" ? '<i class="fa-solid fa-sun"></i>' : '<i class="fa-solid fa-moon"></i>';

    themeToggle.addEventListener("click", function () {
      const isDark = htmlEl.getAttribute("data-theme") === "dark";
      const newTheme = isDark ? "light" : "dark";
      htmlEl.setAttribute("data-theme", newTheme);
      localStorage.setItem("theme", newTheme);
      themeToggle.innerHTML = newTheme === "dark" ? '<i class="fa-solid fa-sun"></i>' : '<i class="fa-solid fa-moon"></i>';
    });
  }

  // ---- Mobile menu toggle ----
  const mobileBtn = document.querySelector(".mobile-menu-btn");
  const navMenu = document.querySelector(".nav-menu");

  if (mobileBtn && navMenu) {
    mobileBtn.addEventListener("click", function () {
      const open = navMenu.classList.contains("open");
      navMenu.classList.toggle("open");
      mobileBtn.setAttribute("aria-expanded", String(!open));
      mobileBtn.innerHTML = open ? "☰" : "✕";
    });

    // Close when clicking a link
    navMenu.querySelectorAll("a").forEach(function (link) {
      link.addEventListener("click", function () {
        navMenu.classList.remove("open");
        mobileBtn.setAttribute("aria-expanded", "false");
        mobileBtn.innerHTML = "☰";
      });
    });
  }

  // ---- Active nav highlight based on current path ----
  const currentPath = window.location.pathname;
  document.querySelectorAll(".nav-menu a").forEach(function (link) {
    const href = link.getAttribute("href");
    if (href && href !== "#" && currentPath.endsWith(href)) {
      link.classList.add("active");
    }
  });

  // ---- Copy code block button (optional enhancement) ----
  document.querySelectorAll("pre").forEach(function (pre) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy-btn";
    btn.title = "Copy to clipboard";
    btn.innerHTML = "Copy";
    btn.style.cssText =
      "position:absolute; top:8px; right:8px; background:var(--bg-panel); " +
      "border:1px solid var(--border); color:var(--text-muted); " +
      "padding:4px 10px; font-size:0.75rem; border-radius:4px; cursor:pointer;";
    pre.style.position = "relative";
    pre.appendChild(btn);

    btn.addEventListener("click", function () {
      const code = pre.querySelector("code") || pre;
      navigator.clipboard.writeText(code.innerText).then(function () {
        const original = btn.innerText;
        btn.innerText = "Copied!";
        btn.style.color = "var(--ok)";
        setTimeout(function () {
          btn.innerText = original;
          btn.style.color = "var(--text-muted)";
        }, 1500);
      });
    });
  });
})();
