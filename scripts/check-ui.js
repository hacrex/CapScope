#!/usr/bin/env node
// Syntax gate for the embedded UI. The UI is split into index.html +
// style.css + app.js (all embedded via //go:embed static), so this checks
// both inline <script> blocks (if any remain) and external .js files.
// Usage: node scripts/check-ui.js   (exits 1 on the first malformed block)
"use strict";
const fs = require("fs");
const os = require("os");
const path = require("path");
const { spawnSync } = require("child_process");

const htmlFile = "internal/web/static/index.html";
const jsDir = "internal/web/static";

let blocks = 0;
let failed = false;

const workdir = fs.mkdtempSync(path.join(os.tmpdir(), "check-ui-"));
let cleaned = false;
const cleanup = () => {
  if (!cleaned) {
    cleaned = true;
    try { fs.rmSync(workdir, { recursive: true, force: true }); } catch (e) { /* ignore */ }
  }
};

// Check inline <script> blocks in index.html (if any remain).
const src = fs.readFileSync(htmlFile, "utf8");
const re = /<script>([\s\S]*?)<\/script>/g;
let i = 0;
for (const m of src.matchAll(re)) {
  i++;
  blocks++;
  const tmp = path.join(workdir, `${path.basename(htmlFile)}.inline-${i}.js`);
  fs.writeFileSync(tmp, m[1], "utf8");
  const r = spawnSync(process.execPath, ["--check", tmp], { encoding: "utf8" });
  if (r.status !== 0) {
    failed = true;
    console.error(`✗ ${htmlFile} <script> block #${i}:\n${r.stderr.trim()}`);
  }
  fs.unlinkSync(tmp);
}

// Check external .js files in the static directory.
for (const file of fs.readdirSync(jsDir)) {
  if (!file.endsWith(".js")) continue;
  const full = path.join(jsDir, file);
  blocks++;
  const r = spawnSync(process.execPath, ["--check", full], { encoding: "utf8" });
  if (r.status !== 0) {
    failed = true;
    console.error(`✗ ${full}:\n${r.stderr.trim()}`);
  }
}

cleanup();

if (failed) process.exit(1);
console.log(`ui check ok: ${blocks} script block/file(s) parsed`);
