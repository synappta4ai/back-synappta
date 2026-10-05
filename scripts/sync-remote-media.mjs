/**
 * Sincroniza medios del despliegue remoto (sslip) al back local.
 *
 * El binario nuevo corre local, pero los archivos subidos/generados mientras
 * la app apuntaba a sslip viven en el disco remoto. Este script:
 *   1. Lista los files en la API local (misma DB compartida).
 *   2. Baja por /files/{id}/serve los que falten en ./uploads.
 *   3. Baja los outputs de generación (/outputs/...) que falten en ./outputs.
 * Las miniaturas no se sincronizan: el back local las genera al vuelo cuando
 * existe el archivo fuente.
 *
 * Uso: node scripts/sync-remote-media.mjs [--dry-run]
 * Credenciales: lee ../front-synapta/.env.e2e (E2E_USER / E2E_PASSWORD).
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const DRY = process.argv.includes('--dry-run');
const LOCAL = 'http://localhost:8099/api/v1';
const REMOTE_ORIGIN = 'https://drako-synaptaback-i2at0w-5d9ff3-187-124-148-219.sslip.io';
const REMOTE = REMOTE_ORIGIN + '/api/v1';
const TENANT_SLUG = 'synapta';
const UPLOADS = path.join(ROOT, 'uploads');
const OUTPUTS = path.join(ROOT, 'outputs');

const env = Object.fromEntries(
  fs
    .readFileSync(path.join(ROOT, '../front-synapta/.env.e2e'), 'utf8')
    .split(/\r?\n/)
    .filter((l) => l.includes('=') && !l.startsWith('#'))
    .map((l) => [l.slice(0, l.indexOf('=')).trim(), l.slice(l.indexOf('=') + 1).trim()]),
);

async function login(base) {
  const res = await fetch(base + '/auth/login-tenant', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: env.E2E_USER, password: env.E2E_PASSWORD, tenant_id: 1 }),
  }).then((r) => r.json());
  const token = res?.data?.token || res?.token;
  if (!token) throw new Error('login falló contra ' + base);
  return token;
}

async function saveTo(destPath, url, token) {
  const res = await fetch(url, { headers: token ? { Authorization: 'Bearer ' + token } : {} });
  if (!res.ok) throw new Error('HTTP ' + res.status + ' para ' + url);
  const buf = Buffer.from(await res.arrayBuffer());
  if (DRY) return buf.length;
  fs.mkdirSync(path.dirname(destPath), { recursive: true });
  fs.writeFileSync(destPath, buf);
  return buf.length;
}

const localToken = await login(LOCAL);
const remoteToken = await login(REMOTE).catch((e) => {
  console.warn('[aviso] login remoto falló:', e.message);
  return null;
});

let synced = 0,
  bytes = 0,
  failed = 0,
  alreadyLocal = 0;

// ─── 1. Archivos (uploads) ─────────────────────────────────
for (let page = 1; ; page++) {
  const res = await fetch(`${LOCAL}/files/page?page=${page}&pageSize=200`, {
    headers: { Authorization: 'Bearer ' + localToken },
  }).then((r) => r.json());
  const items = res?.data?.items || res?.data?.Items || res?.data?.files || [];
  if (!items.length) break;
  for (const f of items) {
    if (!f.path) continue;
    const dest = path.join(UPLOADS, f.path);
    if (fs.existsSync(dest)) {
      alreadyLocal++;
      continue;
    }
    const url = `${REMOTE}/t/${TENANT_SLUG}/files/${f.id}/serve`;
    try {
      console.log('[file]', f.filename, '→', f.path);
      bytes += await saveTo(dest, url, remoteToken);
      synced++;
    } catch (e) {
      failed++;
      console.warn('  [fallo]', e.message);
    }
  }
  if (items.length < 200) break;
}

// ─── 2. Outputs de generación ──────────────────────────────
const hist = await fetch(`${LOCAL}/agency/tasks/history?limit=200`, {
  headers: { Authorization: 'Bearer ' + localToken },
}).then((r) => r.json());
const logs = hist?.data || [];
const outPaths = new Set();
for (const l of logs) {
  for (const o of l.outputs || []) {
    for (const key of ['localUrl', 'url']) {
      const u = o?.[key];
      if (typeof u === 'string' && u.startsWith('/outputs/')) outPaths.add(u);
    }
  }
}
for (const rel of outPaths) {
  const dest = path.join(ROOT, rel.replace(/^\//, ''));
  if (fs.existsSync(dest)) {
    alreadyLocal++;
    continue;
  }
  const url = REMOTE_ORIGIN + rel;
  try {
    console.log('[output]', rel);
    bytes += await saveTo(dest, url, remoteToken);
    synced++;
  } catch (e) {
    failed++;
    console.warn('  [fallo]', e.message);
  }
}

console.log(
  `\nResumen: ${synced} descargados (${(bytes / 1e6).toFixed(1)} MB), ` +
    `${alreadyLocal} ya estaban locales, ${failed} fallos${DRY ? ' [DRY-RUN]' : ''}`,
);
