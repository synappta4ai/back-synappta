/**
 * Valida si un back resuelve modelos "downloaded" (SD-1.5) mediante el worker.
 *
 * Uso:
 *   node scripts/validate-worker-models.mjs
 *   node scripts/validate-worker-models.mjs --remote https://back-dev.synappta.cloud --local http://localhost:8099 --token <jwt>
 *
 * Salida: código 0 si ambos backs responden y resuelven SD-1.5; código 1 si el
 * remoto falla (binario viejo sin BM_WORKER_ADDR o worker inalcanzable).
 */

const REMOTE_DEFAULT = "https://back-dev.synappta.cloud";
const LOCAL_DEFAULT = "http://localhost:8099";

function parseArgs(argv) {
  const args = { remote: REMOTE_DEFAULT, local: LOCAL_DEFAULT, token: undefined, generate: false };
  for (let i = 2; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--remote") args.remote = argv[++i].replace(/\/$/, "");
    else if (a === "--local") args.local = argv[++i].replace(/\/$/, "");
    else if (a === "--token") args.token = argv[++i];
    else if (a === "--generate") args.generate = true;
    else if (a === "--help" || a === "-h") {
      console.log("Uso: node validate-worker-models.mjs [--remote URL] [--local URL] [--token JWT] [--generate]");
      process.exit(0);
    }
  }
  return args;
}

async function fetchJson(url, opts = {}) {
  const headers = { "Content-Type": "application/json", ...opts.headers };
  if (opts.token) headers.Authorization = `Bearer ${opts.token}`;
  const res = await fetch(url, { ...opts, headers });
  let body;
  try { body = await res.json(); } catch (_) { body = await res.text(); }
  return { ok: res.ok, status: res.status, body, headers: res.headers };
}

function pad(s, n) {
  s = String(s);
  return s.length >= n ? s : s + " ".repeat(n - s.length);
}

async function checkReadyz(base, label, token) {
  const { status, body } = await fetchJson(`${base}/readyz`, { token });
  return { label, endpoint: "/readyz", status, body };
}

async function checkDiagnostic(base, label, token) {
  const { status, body } = await fetchJson(`${base}/api/v1/worker/diagnostic`, { token });
  return { label, endpoint: "/api/v1/worker/diagnostic", status, body };
}

async function checkModelsDownloaded(base, label, token) {
  const { status, body } = await fetchJson(`${base}/api/v1/models?type=downloaded`, { token });
  return { label, endpoint: "/api/v1/models?type=downloaded", status, body };
}

async function tryGenerateSD15(base, label, token) {
  const payload = {
    model: "SD-1.5",
    content: [{ type: "text", text: "crea un perro ranchero" }],
    ratio: "1:1",
    resolution: "480p",
    seed: "20",
    event_id: "validate-" + crypto.randomUUID(),
    piece_id: crypto.randomUUID(),
    piece_code: "VAL-20261022",
    generation_number: 1,
  };
  const { status, body } = await fetchJson(`${base}/api/v1/agency/image/generate`, {
    method: "POST",
    body: JSON.stringify(payload),
    token,
  });
  return { label, endpoint: "POST /api/v1/agency/image/generate (SD-1.5)", status, body };
}

function summarize(result) {
  const lines = [];
  lines.push(`  ${result.endpoint} → HTTP ${result.status}`);
  const b = result.body;
  if (b && typeof b === "object") {
    if (b.success !== undefined) lines.push(`    success=${b.success} message=${b.message ?? "-"}`);
    if (b.configured !== undefined) lines.push(`    worker.configured=${b.configured} connected=${b.connected} ping_ms=${b.ping_ms ?? "-"}`);
    if (b.models && Array.isArray(b.models)) {
      const sd = b.models.find((m) => m.key === "SD-1.5" || m.name === "SD-1.5");
      lines.push(`    modelos downloaded=${b.models.length}`);
      if (sd) lines.push(`    SD-1.5: available=${sd.available} downloaded=${sd.downloaded} size_bytes=${sd.size_bytes} path=${sd.download_path ?? "-"}`);
      else lines.push(`    SD-1.5: NO APARECE en el catálogo`);
    }
    if (b.error) lines.push(`    error=${b.error}`);
    if (b.state) lines.push(`    state=${b.state}`);
  } else if (typeof b === "string") {
    lines.push(`    body=${b.slice(0, 200)}`);
  }
  return lines.join("\n");
}

async function main() {
  const args = parseArgs(process.argv);
  console.log("=== Validación de resolución de modelos downloaded (worker) ===\n");
  console.log(`Remoto : ${args.remote}`);
  console.log(`Local  : ${args.local}\n`);

  let failures = 0;

  for (const [base, label] of [[args.remote, "REMOTO"], [args.local, "LOCAL"]]) {
    console.log(`───── ${label} (${base}) ─────`);

    // 1. readyz
    let r;
    try { r = await checkReadyz(base, label, args.token); } catch (e) {
      console.log(`  /readyz → ERROR: ${e.message}`);
      failures++;
      continue;
    }
    console.log(summarize(r));

    // 2. diagnostic (sólo si hay token)
    if (args.token) {
      let d;
      try {
        d = await checkDiagnostic(base, label, args.token);
        console.log(summarize(d));
      } catch (e) {
        console.log(`  /api/v1/worker/diagnostic → ERROR: ${e.message}`);
        failures++;
      }

      // 3. modelos downloaded
      let m;
      try {
        m = await checkModelsDownloaded(base, label, args.token);
        console.log(summarize(m));
      } catch (e) {
        console.log(`  /api/v1/models?type=downloaded → ERROR: ${e.message}`);
        failures++;
      }

      // 4. intento de generación con SD-1.5 (opcional)
      if (args.generate) {
        let g;
        try {
          g = await tryGenerateSD15(base, label, args.token);
          console.log(summarize(g));
        } catch (e) {
          console.log(`  POST /agency/image/generate (SD-1.5) → ERROR: ${e.message}`);
          failures++;
        }
      }
    }
    console.log("");
  }

  console.log("=== Veredicto ===");
  if (failures === 0) {
    console.log("Ambos backs responden y resuelven modelos downloaded.");
    process.exit(0);
  } else {
    console.log(`Se detectaron ${failures} fallo(s). El remoto probablemente corre un binario viejo`);
    console.log("sin BM_WORKER_ADDR o sin alcanzar al worker. Redeployá con deploy-sslip.sh.");
    process.exit(1);
  }
}

main().catch((e) => {
  console.error("Error inesperado:", e);
  process.exit(2);
});
