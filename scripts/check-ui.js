#!/usr/bin/env node
// Syntax gate for the embedded UI. CapScope deliberately has no build step —
// the UI is a single HTML file with inline script — so this stands in where a
// bundler's parse step would otherwise catch breakage before deploy.
// Usage: node scripts/check-ui.js   (exits 1 on the first malformed block)
"use strict";
const fs = require("fs");
const os = require("os");
const path = require("path");
const { spawnSync } = require("child_process");

const files = ["internal/web/static/index.html", ...process.argv.slice(2).filter((a) => a !== "")];

let blocks = 0;
let failed = false;

// Isolated temp dir so concurrent check-ui runs (e.g. parallel CI jobs) don't
// collide on the same inline-${i}.js path in the shared os.tmpdir().
const workdir = fs.mkdtempSync(path.join(os.tmpdir(), "check-ui-"));
let cleaned = false;
const cleanup = () => {
  if (!cleaned) {
    cleaned = true;
    try { fs.rmSync(workdir, { recursive: true, force: true }); } catch (e) { /* ignore */ }
  }
};

for (const file of files) {
  const src = fs.readFileSync(file, "utf8");
  const re = /<script>([\s\S]*?)<\/script>/g;
  let i = 0;
  for (const m of src.matchAll(re)) {
    i++;
    blocks++;
    const tmp = path.join(workdir, `${path.basename(file)}.inline-${i}.js`);
    fs.writeFileSync(tmp, m[1], "utf8");
    const r = spawnSync(process.execPath, ["--check", tmp], { encoding: "utf8" });
    if (r.status !== 0) {
      failed = true;
      console.error(`✗ ${file} <script> block #${i}:\n${r.stderr.trim()}`);
    }
    fs.unlinkSync(tmp);
  }
}
cleanup();

if (failed) process.exit(1);
console.log(`ui check ok: ${blocks} inline script block(s) parsed`);
