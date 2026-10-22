// scripts/validate-worker-models.mjs
// Diagnóstico irrefutable: back → worker para modelos downloaded (SD-1.5).
// Solo requiere HTTP al back; no entra al servidor.
//
// Uso:
//   node scripts/validate-worker-models.mjs --remote https://back-dev.synappta.cloud --token <jwt>
//   node scripts/validate-worker-models.mjs --remote https://back-dev.synappta.cloud --local http://localhost:8099 --token <jwt> --generate
//
// Códigos de salida:
//   0 = todos los checks pasaron
//   1 = hay fallos (el back no alcanza al worker o SD-1.5 no está disponible)
//   2 = error inesperado

const REMOTE_DEFAULT = "https://back-dev.synappta.cloud";
const LOCAL_DEFAULT = "http://localhost:8099";

let passed = 0;
let failed = 0;

function check(name, ok, detail = "") {
  const status = ok ? "PASS" : "FAIL";
  console.log(`[${status}] ${name} ${detail ? "- " + detail : ""}`);
  if (ok) passed++; else failed++;
}

async function fetchJson(url, opts = {}) {
  const headers = { "Content-Type": "application/json", ...opts.headers };
  if (opts.token) {
    const tok = opts.token.startsWith("Bearer ")
      ? opts.token
      : `Bearer ${opts.token}`;
    headers["Authorization"] = tok;
  }
  const res = await fetch(url, { ...opts, headers });
  let body;
  try { body = await res.json(); } catch { body = await res.text(); }
  return { ok: res.ok, status: res.status, body };
}

function findSD15(models) {
  if (!Array.isArray(models)) return null;
  return models.find((m) => m && (m.key === "SD-1.5" || m.name === "SD-1.5"));
}

async function checkReadiness(base, token) {
  try {
    const { status, body } = await fetchJson(`${base}/readyz`, { token });
    const ok = status === 200;
    check("1. /readyz", ok, `HTTP ${status} ${ok ? "" : JSON.stringify(body)}`);
    return ok;
  } catch (e) {
    check("1. /readyz", false, `ERROR: ${e.message}`);
    return false;
  }
}

async function checkDiagnostic(base, token) {
  try {
    const { status, body } = await fetchJson(`${base}/api/v1/worker/diagnostic`, { token });
    if (status !== 200) {
      check("2. /api/v1/worker/diagnostic", false, `HTTP ${status}`);
      return false;
    }
    const configured = body.configured === true;
    const connected = body.connected === true;
    const ping = body.ping_ms ?? "?";
    const modelCount = body.model_count ?? 0;
    const sd = findSD15(body.models);
    const sdAvailable = sd ? sd.available === true : false;
    const sdDownloaded = sd ? sd.downloaded === true : false;
    const sdSize = sd ? sd.size_bytes ?? 0 : 0;
    const detail = `HTTP ${status} configured=${configured} connected=${connected} ping=${ping}ms models=${modelCount} SD-1.5 available=${sdAvailable} downloaded=${sdDownloaded} size=${sdSize}B`;
    const ok = configured && connected && modelCount > 0 && sdAvailable;
    check("2. /api/v1/worker/diagnostic", ok, detail);
    return ok;
  } catch (e) {
    check("2. /api/v1/worker/diagnostic", false, `ERROR: ${e.message}`);
    return false;
  }
}

async function checkModelsDownloaded(base, token) {
  try {
    const { status, body } = await fetchJson(`${base}/api/v1/models?type=downloaded`, { token });
    if (status !== 200) {
      check("3. /api/v1/models?type=downloaded", false, `HTTP ${status}`);
      return false;
    }
    const data = body.data ?? body.models ?? body ?? [];
    const models = Array.isArray(data) ? data : [];
    const sd = findSD15(models);
    const ok = models.length > 0 && !!sd;
    check("3. /api/v1/models?type=downloaded", ok, `HTTP ${status} modelos=${models.length} SD-1.5=${!!sd}`);
    return ok;
  } catch (e) {
    check("3. /api/v1/models?type=downloaded", false, `ERROR: ${e.message}`);
    return false;
  }
}

async function checkGenerateSD15(base, token) {
  const payload = {
    model: "SD-1.5",
    content: [{ type: "text", text: "perro ranchero" }],
    ratio: "1:1",
    resolution: "480p",
    seed: "20",
    event_id: "validate-" + crypto.randomUUID(),
    piece_id: crypto.randomUUID(),
    piece_code: "VAL-20261022",
    generation_number: 1,
  };
  try {
    const { status, body } = await fetchJson(`${base}/api/v1/agency/image/generate`, {
      method: "POST",
      body: JSON.stringify(payload),
      token,
    });
    const ok = status === 200 && body.success === true;
    const taskId = (body.data && (body.data.taskId ?? body.data.id)) ?? "-";
    check("4. POST /agency/image/generate (SD-1.5)", ok, `HTTP ${status} success=${body.success} taskId=${taskId} message=${body.message ?? "-"}`);
    return ok;
  } catch (e) {
    check("4. POST /agency/image/generate (SD-1.5)", false, `ERROR: ${e.message}`);
    return false;
  }
}

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

async function main() {
  const args = parseArgs(process.argv);
  console.log("=== Diagnóstico irrefutable: back → worker (modelos downloaded) ===\n");
  console.log(`Remoto : ${args.remote}`);
  if (args.local) console.log(`Local  : ${args.local}`);
  if (!args.token) console.log("Token : (no proporcionado - solo /readyz se probará)");
  console.log("");

  let allOk = true;

  const targets = args.local ? [[args.remote, "REMOTO"], [args.local, "LOCAL"]] : [[args.remote, "REMOTO"]];

  for (const [base, label] of targets) {
    console.log(`───── ${label} (${base}) ─────`);
    const r1 = await checkReadiness(base, args.token);
    if (args.token) {
      const r2 = await checkDiagnostic(base, args.token);
      const r3 = await checkModelsDownloaded(base, args.token);
      if (args.generate) await checkGenerateSD15(base, args.token);
      if (!r1 || !r2 || !r3) allOk = false;
    } else {
      if (!r1) allOk = false;
    }
    console.log("");
  }

  console.log("=== RESUMEN ===");
  console.log(`Checks pasados: ${passed} | Checks fallidos: ${failed}`);
  if (allOk && failed === 0) {
    console.log("RESULTADO: TODOS LOS CHECKS PASARON. El back resuelve modelos downloaded correctamente.");
    process.exit(0);
  } else {
    console.log("RESULTADO: HAY FALLOS. El back no alcanza al worker o SD-1.5 no está disponible.");
    console.log("Acción: revisá BM_WORKER_ADDR en el back, que el worker responda en :50051, y redeploy si es necesario.");
    process.exit(1);
  }
}

main().catch((e) => {
  console.error("Error inesperado:", e);
  process.exit(2);
});
