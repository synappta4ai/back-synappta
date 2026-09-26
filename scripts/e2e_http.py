#!/usr/bin/env py -3
"""E2E HTTP completo de back-synapta + inference worker (mock local).

Uso:  py -3 scripts/e2e_http.py
Flujo: login → modelos por tipo → estado del worker →
       evento/programa/pieza → generación descargada (gRPC) → artifact.
"""
import json
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

# Windows consola: asegurar salida UTF-8.
if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

BASE = "http://127.0.0.1:8099"
TENANT = "synapta"


def env_value(key: str) -> str:
    for line in (Path(__file__).resolve().parent.parent / ".env").read_text(encoding="utf-8").splitlines():
        if line.startswith(key + "="):
            return line.split("=", 1)[1].strip()
    return ""


def call(method: str, path: str, token: str = "", body: dict | None = None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    req.add_header("X-Tenant-Slug", TENANT)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data=data, timeout=20) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        raise SystemExit(f"{method} {path} → HTTP {e.status}: {e.read().decode(errors='replace')[:300]}")


def main() -> None:
    print("== 1. Login ==")
    r = call("POST", "/api/v1/auth/login",
             body={"username": "superadmin", "password": env_value("SUPER_ADMIN_PASSWORD") or "superadmin_pass_123"})
    token = r["data"]["token"]
    print(f"  ok, token {token[:25]}...")

    print("\n== 2. Modelos por tipo ==")
    api = call("GET", "/api/v1/models?type=api", token)["data"]
    dl = call("GET", "/api/v1/models?type=downloaded", token)["data"]
    print(f"  type=api: {len(api)} modelos — todos 'api': {all(m['type'] == 'api' for m in api)}")
    print(f"  type=downloaded: {len(dl)} modelos (en vivo del worker)")
    vids = [m["name"] for m in dl if (m.get("downloaded") or {}).get("mode") == "video"]
    print("  videos descargados:", vids[:5], "..." if len(vids) > 5 else "")
    if not any(m["name"] == "Wan2.1-T2V-1.3B" for m in dl):
        raise SystemExit("Wan2.1-T2V-1.3B no está en el catálogo del worker")

    print("\n== 3. Estado del worker (live) ==")
    st = call("GET", "/api/v1/worker/status", token)["data"]
    print(f"  configured={st['configured']} connected={st['connected']} "
          f"device={st.get('device_name')!r} cuda={st['cuda_available']} modelos={st['model_count']}")

    print("\n== 4. Evento → programa → pieza ==")
    event = call("POST", "/api/v1/events", token,
                 {"name": "E2E worker", "description": "smoke test automatizado"})["data"]
    program = call("POST", f"/api/v1/events/{event['id']}/programs", token,
                   {"number": 1, "name": "P1"})["data"]
    piece = call("POST", f"/api/v1/events/{event['id']}/pieces", token,
                 {"program_id": program["id"], "number": 1, "piece_code": "E2E-W1",
                  "name": "Pieza worker", "type": "video"})["data"]
    print(f"  event={event['id'][:8]}.. program={program['id'][:8]}.. piece={piece['id'][:8]}.. ({piece['piece_code']})")

    print("\n== 5. Generación con modelo descargado (gRPC al worker) ==")
    gen = call("POST", "/api/v1/agency/video/generate", token, {
        "model": "Wan2.1-T2V-1.3B",
        "content": [{"type": "text", "text": "a golden retriever astronaut floating in space, cinematic"}],
        "ratio": "16:9",
        "duration": 2,
        "event_id": event["id"],
        "program_id": program["id"],
        "piece_id": piece["id"],
        "piece_code": piece["piece_code"],
        "generation_number": 1,
    })["data"]
    task_id = gen["taskId"]
    print(f"  taskId={task_id} status={gen['status']} model={gen['model']}")

    print("\n== 6. Polling ==")
    final = None
    for i in range(1, 31):
        s = call("GET", f"/api/v1/agency/video/status/{task_id}", token)["data"]
        print(f"  [{i}] status={s['status']} progress={s['progress_percent']}%")
        if s["status"] in ("succeeded", "failed"):
            final = s
            break
        time.sleep(1)
    if final is None:
        raise SystemExit("TIMEOUT esperando el job")
    if final["status"] != "succeeded":
        raise SystemExit(f"job terminó en {final['status']}: {final.get('error')}")

    print("\n== 7. Artifact ==")
    outs = final.get("outputs") or []
    for o in outs:
        print(f"  output: {o['type']} url={o['url'][:90]}")
    if not outs:
        raise SystemExit("sin outputs")
    url = outs[0]["url"]
    req = urllib.request.Request(url)
    req.add_header("Authorization", "Bearer " + token)
    req.add_header("X-Tenant-Slug", TENANT)
    data = urllib.request.urlopen(req, timeout=20).read()
    print(f"  descargado: {len(data)} bytes — contenido: {data[:50]!r}")
    if not data:
        raise SystemExit("artifact vacío")

    print("\nE2E HTTP COMPLETO ✅")


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception as e:  # noqa: BLE001
        print(f"FALLO: {e}", file=sys.stderr)
        sys.exit(1)
