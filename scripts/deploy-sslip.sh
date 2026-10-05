#!/usr/bin/env bash
# Deploy del binario nuevo de synapta al host sslip (drako-synaptaback).
#
# Uso: bash scripts/deploy-sslip.sh [usuario@host]
#   Default: root@187.124.148.219 (requiere llave SSH autorizada en el host).
#
# Pasos: sube bin/synapta-linux, descubre cómo corre el servicio en el host
# (systemd o proceso directo), respalda el binario actual, lo reemplaza,
# reinicia y verifica /readyz por HTTPS.
set -euo pipefail

TARGET="${1:-root@187.124.148.219}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$DIR/bin/synapta-linux"
REMOTE_TMP="/tmp/synapta-deploy-$$"

[ -f "$BIN" ] || { echo "falta $BIN (compilar: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/synapta-linux .)"; exit 1; }

echo "==> Subiendo binario a $TARGET"
ssh "$TARGET" "mkdir -p /tmp" >/dev/null
scp -q "$BIN" "$TARGET:$REMOTE_TMP"

echo "==> Descubriendo instalación remota"
ssh "$TARGET" "
set -e
RUNNING=\$(pgrep -a synapta | head -1 || true)
echo \"proceso actual: \${RUNNING:-ninguno}\"
if [ -n \"\$RUNNING\" ]; then
  BIN_PATH=\$(echo \"\$RUNNING\" | awk '{print \$NF}')
  # el binario es el primer token que termina en synapta*
  BIN_PATH=\$(echo \"\$RUNNING\" | tr ' ' '\n' | grep -E 'synapta' | head -1)
fi
if command -v systemctl >/dev/null 2>&1 && systemctl list-units --type=service | grep -qi synapta; then
  SVC=\$(systemctl list-units --type=service | grep -i synapta | awk '{print \$1}' | head -1)
  echo \"servicio systemd: \$SVC\"
  [ -n \"\${BIN_PATH:-}\" ] || BIN_PATH=\$(systemctl show \"\$SVC\" -p ExecStart --value | sed 's/.*ExecStart=//;s/;.*//' | tr ' ' '\n' | grep synapta | head -1)
  [ -n \"\${BIN_PATH:-}\" ] || BIN_PATH=/usr/local/bin/synapta
  cp \"\$BIN_PATH\" \"\$BIN_PATH.bak.\$(date +%s)\"
  install -m 0755 \"\$REMOTE_TMP\" \"\$BIN_PATH\"
  systemctl restart \"\$SVC\"
else
  BIN_PATH=\${BIN_PATH:-/root/synapta}
  echo \"sin systemd: reemplazo \$BIN_PATH y relanzo\"
  [ -f \"\$BIN_PATH\" ] && cp \"\$BIN_PATH\" \"\$BIN_PATH.bak.\$(date +%s)\"
  install -m 0755 \"\$REMOTE_TMP\" \"\$BIN_PATH\"
  pkill -f synapta || true
  sleep 1
  cd \"\$(dirname \"\$BIN_PATH\")\"
  nohup \"\$BIN_PATH\" >> synapta.log 2>&1 &
fi
rm -f \"\$REMOTE_TMP\"
echo 'binario reemplazado'
"

echo "==> Health check (esperando readyz)…"
for i in $(seq 1 20); do
  CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 https://drako-synaptaback-i2at0w-5d9ff3-187-124-148-219.sslip.io/readyz || true)
  if [ "$CODE" = "200" ]; then echo "readyz=200 ✓"; exit 0; fi
  sleep 3
done
echo "readyz no respondió 200 a los 60s — revisar el servicio en el host" >&2
exit 1
