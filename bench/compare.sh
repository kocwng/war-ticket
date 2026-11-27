#!/usr/bin/env bash
: <<'CATATAN'
Menjalankan keempat strategi satu per satu dengan load yang sama, lalu
mencetak tabel perbandingan dalam Markdown.

  ./bench/compare.sh [buyers=5000] [stock=500]

Env opsional:
  STRATEGIES  daftar strategi (default "naive forupdate redis queue")
  COOLDOWN    jeda antar-run dalam detik (default 5)
  + semua env run.sh (APP_PORT, VUS, EVENT_ID, ADMIN_TOKEN, ...)

Tabelnya juga disimpan ke results/compare-<timestamp>.md
CATATAN
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.."

BUYERS="${1:-5000}"
STOCK="${2:-500}"
STRATEGIES="${STRATEGIES:-naive forupdate redis queue}"
COOLDOWN="${COOLDOWN:-5}"
TS=$(date +%Y%m%d-%H%M%S)
MD="results/compare-${TS}.md"

dirs=()
failed=""
first=1
for s in $STRATEGIES; do
  [ "$first" -eq 1 ] || { echo "==> Istirahat ${COOLDOWN} detik dulu sebelum run berikutnya..."; sleep "$COOLDOWN"; }
  first=0
  run_dir="$(date +%Y%m%d-%H%M%S)-go-${s}"
  echo
  echo "################ ${s} ################"
  # Kalau satu strategi gagal, sisanya tetap jalan. Yang gagal dilaporkan di akhir.
  if ! RUN_DIR="$run_dir" ./bench/run.sh "$s" "$BUYERS" "$STOCK"; then
    failed="${failed} ${s}"
  fi
  dirs+=("results/${run_dir}")
done

: <<'CATATAN'
Bikin satu baris tabel dari satu folder run. Kolom "Queue p99" cuma terisi
untuk strategi queue karena strategi lain tidak punya queue.
CATATAN
row() {
  local dir="$1"
  local k6json='{}'
  [ -f "${dir}/k6-summary.json" ] && k6json=$(cat "${dir}/k6-summary.json")
  local report='{}'
  [ -f "${dir}/report.json" ] && report=$(cat "${dir}/report.json")
  local strategy="${dir##*-go-}"
  jq -rn --argjson r "$report" --argjson k "$k6json" --arg s "$strategy" '
    def ms: if . == null then "-" else ((. * 10 | round) / 10 | tostring) + " ms" end;
    def n: if . == null then "-" else tostring end;
    ($k.metrics // {}) as $m |
    [ $s,
      ($r.db.tickets | n),
      ($r.oversold | n),
      (if $r.consistent == true then "ya" elif $r.consistent == false then "**tidak**" else "-" end),
      ($m.outcome_overloaded.values.count // 0 | tostring),
      ($m.outcome_timeout.values.count // 0 | tostring),
      ($m.purchase_latency.values.med | ms),
      ($m.purchase_latency.values["p(99)"] | ms),
      (if $s == "queue" then ($m.queue_wait.values["p(99)"] | ms) else "-" end)
    ] | "| " + join(" | ") + " |"'
}

{
  echo "## War Tiket: perbandingan strategi (go)"
  echo
  echo "${BUYERS} pembeli, stok ${STOCK}, $(date '+%Y-%m-%d %H:%M')"
  echo
  echo "| Strategi | Terjual | Oversold | Consistent | 503 | 504 | p50 | p99 | Queue p99 |"
  echo "|---|---:|---:|:---:|---:|---:|---:|---:|---:|"
  for d in "${dirs[@]}"; do row "$d"; done
  echo
  echo "p50/p99 = latency POST beli (purchase_latency). 503/504 dihitung dari sisi k6."
  echo
  echo "Run:"
  for d in "${dirs[@]}"; do echo "- \`${d}\`"; done
} >"$MD"

echo
cat "$MD"
echo
echo "Tabel tersimpan di ${MD}"

if [ -n "$failed" ]; then
  echo "PERINGATAN: ada run yang gagal:${failed}" >&2
  exit 1
fi
