// runner.mjs
//
// End-to-end test runner for the my-travels Go server.
//
//  1. Start the prebuilt server binary (`my-travels.exe` in the repo root)
//     on a free localhost port with a throwaway sqlite database.
//  2. Seed two sessions directly into the DB: an admin and a regular user.
//  3. Run the Playwright spec(s), passing the admin's session token to
//     the test code so it can authenticate.
//  4. On exit, stop the server and clean up the temp data.
//
// Usage:
//   node runner.mjs [--spec name.spec.mjs] [--port N] [--keep-server]
//
// Screenshots are written to tests/playwright/screenshots/.

import { spawn, execFile } from 'node:child_process';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, resolve, basename } from 'node:path';
import { fileURLToPath, URL } from 'node:url';
import { promisify } from 'node:util';

const execFileP = promisify(execFile);

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, '..', '..');
const SCREENSHOT_DIR = join(__dirname, 'screenshots');
const PORT = Number(process.env.TEST_PORT ?? 18765);
const ADMIN_EMAIL = 'admin@test.com';

// ---------------------------------------------------------------------------
// CLI arg parsing (minimal)
// ---------------------------------------------------------------------------
const args = process.argv.slice(2);
const opts = { spec: 'impersonation.spec.mjs', keepServer: false };
for (let i = 0; i < args.length; i++) {
  const a = args[i];
  if (a === '--spec') { opts.spec = args[++i]; }
  else if (a === '--keep-server') { opts.keepServer = true; }
  else if (a === '--help' || a === '-h') {
    console.log('Usage: node runner.mjs [--spec FILE] [--keep-server]');
    process.exit(0);
  }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
async function fileExists(p) {
  try { await import('node:fs/promises').then(m => m.access(p)); return true; }
  catch { return false; }
}

function log(msg) {
  // Use a single line prefix so the output is easy to grep when CI is full
  // of Playwright's verbose logs.
  console.log(`[runner] ${msg}`);
}

async function runExe(exe, args, opts2 = {}) {
  return new Promise((resolve2, reject) => {
    const child = spawn(exe, args, { stdio: ['ignore', 'pipe', 'pipe'], ...opts2 });
    const out = [];
    const err = [];
    child.stdout.on('data', (b) => { out.push(b); process.stdout.write(`[${basename(exe)}] ${b}`); });
    child.stderr.on('data', (b) => { err.push(b); process.stderr.write(`[${basename(exe)}] ${b}`); });
    child.on('error', reject);
    child.on('exit', (code, signal) => {
      resolve2({ code, signal, stdout: Buffer.concat(out).toString('utf8'), stderr: Buffer.concat(err).toString('utf8') });
    });
  });
}

// Start the prebuilt server. Returns { proc, dbPath, tempDir, baseURL }.
async function startServer() {
  const serverExe = join(REPO_ROOT, 'my-travels.exe');
  if (!existsSync(serverExe)) {
    throw new Error(`Server binary not found at ${serverExe} - run 'go build -o my-travels.exe ./cmd/srv' first.`);
  }
  const seedExe = join(REPO_ROOT, 'seedtest.exe');
  if (!existsSync(seedExe)) {
    throw new Error(`seedtest binary not found at ${seedExe} - run 'go build -o seedtest.exe ./cmd/seedtest' first.`);
  }

  // Per-run temp dir holds db.sqlite3 and uploads/ so we never touch the
  // user's real data.
  const tempDir = await mkdtemp(join(tmpdir(), 'mt-e2e-'));
  const dbPath = join(tempDir, 'db.sqlite3');
  const baseDir = tempDir;
  // BASE_DIR is the dir above srv/templates and srv/static. The server
  // looks for these under baseDir/srv/, so we symlink the repo's srv/
  // directory into the temp dir.
  // On Windows, symlinks require elevation - use a junction instead.
  const { symlink } = await import('node:fs/promises');
  try {
    await symlink(join(REPO_ROOT, 'srv'), join(tempDir, 'srv'), 'junction');
  } catch (e) {
    throw new Error(`Failed to link srv directory into ${tempDir}: ${e.message}`);
  }

  const env = {
    ...process.env,
    LISTEN: `:${PORT}`,
    BASE_DIR: baseDir,
    ADMIN_EMAIL,
    // OAuth env vars empty: the test does not exercise OAuth.
    GOOGLE_CLIENT_ID: '',
    GOOGLE_CLIENT_SECRET: '',
  };

  log(`Starting server on port ${PORT} (db=${dbPath})`);
  const proc = spawn(serverExe, [], {
    stdio: ['ignore', 'pipe', 'pipe'],
    env,
    cwd: tempDir,
    // detached so we can kill the whole process tree
    detached: true,
  });
  proc.stdout.on('data', (b) => process.stdout.write(`[srv] ${b}`));
  proc.stderr.on('data', (b) => process.stderr.write(`[srv] ${b}`));

  // Wait for the server to come up by polling /api/me.
  const baseURL = `http://127.0.0.1:${PORT}`;
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`${baseURL}/api/me`);
      if (r.status < 500) break;
    } catch { /* not up yet */ }
    if (proc.exitCode != null) {
      throw new Error(`Server exited with code ${proc.exitCode} before becoming ready`);
    }
    await new Promise((r2) => setTimeout(r2, 200));
  }
  // Final check.
  try {
    const r = await fetch(`${baseURL}/api/me`);
    if (!r.ok) throw new Error(`/api/me returned ${r.status}`);
  } catch (e) {
    proc.kill('SIGKILL');
    throw new Error(`Server never became ready: ${e.message}`);
  }
  log(`Server up at ${baseURL}`);

  return { proc, dbPath, tempDir, baseURL };
}

async function stopServer(proc) {
  if (proc.exitCode != null) return;
  log('Stopping server');
  try {
    // Kill the whole process group on POSIX. On Windows, just kill the proc.
    if (process.platform !== 'win32') {
      try { process.kill(-proc.pid, 'SIGTERM'); } catch { /* ignore */ }
    } else {
      proc.kill('SIGTERM');
    }
    // Give it a moment to exit cleanly.
    await new Promise((r) => setTimeout(r, 500));
    if (proc.exitCode == null) proc.kill('SIGKILL');
  } catch (e) {
    log(`Warning: failed to stop server: ${e.message}`);
  }
}

// Seed a session for the given user and return the session token.
async function seedSession(seatExe, dbPath, userID, email) {
  const { stdout } = await execFileP(seatExe, ['-db', dbPath, '-user', userID, '-email', email]);
  return stdout.trim();
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------
async function main() {
  await mkdir(SCREENSHOT_DIR, { recursive: true });

  const { proc, dbPath, tempDir, baseURL } = await startServer();

  let exitCode = 0;
  try {
    // Seed admin + a normal user
    const adminToken = await seedSession(join(REPO_ROOT, 'seedtest.exe'), dbPath, 'admin-1', ADMIN_EMAIL);
    const userToken  = await seedSession(join(REPO_ROOT, 'seedtest.exe'), dbPath, 'user-2', 'bob@example.com');
    log(`Seeded admin token: ${adminToken.slice(0, 8)}...`);
    log(`Seeded user  token: ${userToken.slice(0, 8)}...`);

    // Hand off to the Playwright spec.
    const specPath = resolve(__dirname, opts.spec);
    if (!existsSync(specPath)) {
      throw new Error(`Spec file not found: ${specPath}`);
    }
    // On Windows, dynamic import() requires a file:// URL.
    const specURL = new URL('file:///' + specPath.replace(/\\/g, '/'));
    // Pass the test context via env so the spec can pick it up.
    process.env.E2E_BASE_URL    = baseURL;
    process.env.E2E_ADMIN_TOKEN = adminToken;
    process.env.E2E_USER_TOKEN  = userToken;
    process.env.E2E_ADMIN_EMAIL = ADMIN_EMAIL;
    process.env.E2E_USER_EMAIL  = 'bob@example.com';
    process.env.E2E_SCREENSHOT_DIR = SCREENSHOT_DIR;

    log(`Running spec: ${specPath}`);
    const spec = await import(specURL.href);
    if (typeof spec.run !== 'function') {
      throw new Error(`Spec ${opts.spec} must export a run({ playwright, baseURL, tokens }) function`);
    }
    const { chromium } = await import('playwright');
    exitCode = await spec.run({ playwright: { chromium }, baseURL, tokens: { adminToken, userToken, adminEmail: ADMIN_EMAIL, userEmail: 'bob@example.com' } });
  } catch (e) {
    log(`FATAL: ${e.stack || e.message}`);
    exitCode = 1;
  } finally {
    if (!opts.keepServer) {
      await stopServer(proc);
      try { await rm(tempDir, { recursive: true, force: true }); } catch (e) { log(`Warning: rm tempDir: ${e.message}`); }
    } else {
      log(`--keep-server set: server left running, tempDir=${tempDir}`);
    }
  }
  process.exit(exitCode);
}

main();
