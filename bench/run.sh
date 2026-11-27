#!/usr/bin/env bash
# Menjalankan satu benchmark flash sale untuk satu strategi.
#
#   ./bench/run.sh <naive|forupdate|redis|queue> [buyers=5000] [stock=500]
#
# Env opsional:
#   APP_PORT       port app di host (default 8088)
#   EVENT_ID       (default 1)
#   VUS            jumlah VU k6 yang jalan paralel (default 1000)
#   ADMIN_TOKEN    isi kalau server mewajibkan header X-Admin-Token
#   TOXIPROXY_URL  (default http://localhost:8474)
#   RUN_DIR        nama folder hasil, biasanya diisi compare.sh
#
# Hasilnya ada di results/<RUN_DIR>/{meta,report,k6-summary}.json
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.."

IMPL="go" # di docker-compose, container k6 menembak app-go

usage() {
  echo "Cara pakai: $0 <naive|forupdate|redis|queue> [buyers=5000] [stock=500]" >&2
  exit 2
}
die() { echo "GAGAL: $*" >&2; exit 1; }
info() { echo "==> $*"; }

[ $# -ge 1 ] || usage
STRATEGY="$1"
BUYERS="${2:-5000}"
STOCK="${3:-500}"
case "$STRATEGY" in naive|forupdate|redis|queue) ;; *) echo "Strategi tidak dikenal: $STRATEGY" >&2; usage ;; esac
case "$BUYERS" in ''|*[!0-9]*) die "buyers harus berupa angka, bukan '$BUYERS'" ;; esac
case "$STOCK" in ''|*[!0-9]*) die "stock harus berupa angka, bukan '$STOCK'" ;; esac

for bin in curl jq docker; do
  command -v "$bin" >/dev/null 2>&1 || die "'$bin' belum terpasang atau tidak ada di PATH"
done

APP_URL="http://localhost:${APP_PORT:-8088}"
EVENT_ID="${EVENT_ID:-1}"
VUS="${VUS:-1000}"
TOXIPROXY_URL="${TOXIPROXY_URL:-http://localhost:8474}"
RUN_DIR="${RUN_DIR:-$(date +%Y%m%d-%H%M%S)-${IMPL}-${STRATEGY}}"
OUT="results/${RUN_DIR}"

# Header admin opsional. Ekspansi ${ADMIN_HDR[@]+...} di admin_curl dipakai
# supaya array kosong tidak bikin error di bash 3.2 dengan set -u.
ADMIN_HDR=()
if [ -n "${ADMIN_TOKEN:-}" ]; then ADMIN_HDR=(-H "X-Admin-Token: ${ADMIN_TOKEN}"); fi
admin_curl() { curl ${ADMIN_HDR[@]+"${ADMIN_HDR[@]}"} "$@"; }

# 1. Cek app sudah hidup ----------------------------------------------------
if ! curl -fsS --max-time 3 "${APP_URL}/healthz" >/dev/null 2>&1; then
  die "app belum hidup di ${APP_URL}. Jalankan 'make up' dulu."
fi

[ -e "$OUT" ] && die "folder ${OUT} sudah ada. Hasil lama tidak akan ditimpa."
mkdir -p "$OUT"
# Container k6 jalan sebagai user non-root. Di Linux, folder ini perlu dibuka
# aksesnya supaya k6 bisa menulis ringkasan.
chmod a+rwx "$OUT"

# 2. Reset stok dan pilih strategi ------------------------------------------
info "Reset event ${EVENT_ID}: stok ${STOCK}, strategi ${STRATEGY}"
reset_body=$(jq -cn --argjson e "$EVENT_ID" --argjson s "$STOCK" --arg st "$STRATEGY" \
  '{event_id: $e, stock: $s, strategy: $st}')
code=$(admin_curl -sS -o "${OUT}/.reset-response" -w '%{http_code}' --max-time 10 \
  -X POST -H 'Content-Type: application/json' -d "$reset_body" "${APP_URL}/admin/reset") || code="000"
if [ "$code" != "204" ]; then
  echo "Respons reset:" >&2; cat "${OUT}/.reset-response" >&2 2>/dev/null || true; echo >&2
  die "reset dapat HTTP ${code}, harusnya 204"
fi
rm -f "${OUT}/.reset-response"

# 3. Jalankan k6 --------------------------------------------------------------
info "k6: ${BUYERS} pembeli, ${VUS} VU, strategi ${STRATEGY} -> ${OUT}"
export STRATEGY BUYERS RUN_DIR EVENT_ID VUS
k6_status=0
docker compose --profile bench run --rm \
  -e STRATEGY -e BUYERS -e RUN_DIR -e EVENT_ID -e VUS \
  k6 run /bench/k6/spike.js || k6_status=$?
[ "$k6_status" -eq 0 ] || echo "PERINGATAN: k6 selesai dengan exit code ${k6_status}. Report tetap diambil." >&2

# 4. Tunggu pesanan dari Redis masuk semua ke Postgres, lalu simpan report ----
info "Menunggu persist_lag jadi 0, paling lama 30 detik..."
report=""
lag="?"
for _ in $(seq 1 60); do
  if r=$(admin_curl -fsS --max-time 5 "${APP_URL}/admin/report?event_id=${EVENT_ID}"); then
    report="$r"
    lag=$(printf '%s' "$report" | jq -r '.redis.persist_lag // 0' 2>/dev/null) || lag="?"
    [ "$lag" = "0" ] && break
  fi
  sleep 0.5
done
[ -n "$report" ] || die "tidak bisa mengambil /admin/report"
[ "$lag" = "0" ] || echo "PERINGATAN: setelah 30 detik persist_lag masih ${lag}. Report disimpan apa adanya." >&2
printf '%s\n' "$report" | jq . >"${OUT}/report.json"

# 5. meta.json ----------------------------------------------------------------
git_commit="uncommitted"
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if c=$(git rev-parse --short HEAD 2>/dev/null); then
    git_commit="$c"
    [ -z "$(git status --porcelain 2>/dev/null)" ] || git_commit="${c}-dirty"
  fi
fi

cpus=$(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)
cpu_model=$(sysctl -n machdep.cpu.brand_string 2>/dev/null \
  || awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null || true)

toxics=$(curl -fsS --max-time 2 "${TOXIPROXY_URL}/proxies/postgres/toxics" 2>/dev/null || true)
printf '%s' "$toxics" | jq -e 'type == "array"' >/dev/null 2>&1 || toxics='[]'

jq -n \
  --arg strategy "$STRATEGY" \
  --argjson buyers "$BUYERS" \
  --argjson stock "$STOCK" \
  --argjson vus "$VUS" \
  --argjson event_id "$EVENT_ID" \
  --arg implementation "$IMPL" \
  --arg run_dir "$RUN_DIR" \
  --arg git_commit "$git_commit" \
  --arg os "$(uname -sm)" \
  --argjson cpus "${cpus:-0}" \
  --arg cpu_model "${cpu_model:-}" \
  --argjson toxics "$toxics" \
  --argjson k6_exit_code "$k6_status" \
  --argjson persist_lag_final "$( [ "$lag" = "?" ] && echo null || echo "$lag" )" \
  --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{
    run_dir: $run_dir,
    implementation: $implementation,
    strategy: $strategy,
    buyers: $buyers,
    stock: $stock,
    vus: $vus,
    event_id: $event_id,
    git_commit: $git_commit,
    host: { os: $os, cpus: $cpus, cpu_model: $cpu_model },
    chaos: { postgres_toxics: $toxics, enabled: ($toxics | length > 0) },
    k6_exit_code: $k6_exit_code,
    persist_lag_final: $persist_lag_final,
    timestamp: $timestamp
  }' >"${OUT}/meta.json"

# 6. Ringkasan ----------------------------------------------------------------
summary="${OUT}/k6-summary.json"
k6json='{}'
if [ -f "$summary" ]; then
  k6json=$(cat "$summary")
else
  echo "PERINGATAN: ${summary} tidak ada. Sepertinya k6 gagal menulis ringkasan." >&2
fi

echo
jq -rn \
  --slurpfile r "${OUT}/report.json" \
  --argjson k "$k6json" \
  --arg strategy "$STRATEGY" --arg dir "$OUT" '
  def ms: if . == null then "-" else ((. * 10 | round) / 10 | tostring) + "ms" end;
  ($r[0]) as $r | ($k.metrics // {}) as $m |
  "[\($strategy)] terjual=\($r.db.tickets // "-") oversold=\($r.oversold // "-") konsisten=\(if $r.consistent == null then "-" else $r.consistent end)"
  + " outcomes={\(($r.app.outcomes // {}) | to_entries | map("\(.key):\(.value)") | join(" "))}"
  + " p50=\($m.purchase_latency.values.med | ms) p99=\($m.purchase_latency.values["p(99)"] | ms)"
  + (if $m.queue_wait then " antre_p99=\($m.queue_wait.values["p(99)"] | ms)" else "" end)
  + "\nHasil tersimpan di \($dir)/"'

exit "$k6_status"
