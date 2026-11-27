#!/usr/bin/env bash
: <<'CATATAN'
Jalankan keempat strategi beberapa run, lalu hitung mediannya.

Cara pakai:
  ./bench/series.sh <label> [runs=5] [buyers=5000] [stock=500]
  ./bench/series.sh baseline
  ./bench/chaos.sh on 40 10 && ./bench/series.sh db-lambat; ./bench/chaos.sh off

Strateginya dijalankan interleaved per run (naive, forupdate, redis,
queue, lalu ulang), bukan lima kali naive dulu baru pindah. Dengan begitu
kalau laptop makin panas atau ada proses lain yang ikut jalan, dampaknya
kebagian rata ke semua strategi.

Hasilnya ada di results/series-<waktu>-<label>/, satu folder per run,
plus summary.md berisi median dan range-nya.
CATATAN
set -euo pipefail
cd "$(dirname "$0")/.."

LABEL="${1:?Kasih label dulu, misalnya: ./bench/series.sh baseline}"
ROUNDS="${2:-5}"
BUYERS="${3:-5000}"
STOCK="${4:-500}"
STRATEGIES="${STRATEGIES:-naive forupdate redis queue}"
COOLDOWN="${COOLDOWN:-5}"

BATCH="series-$(date +%Y%m%d-%H%M%S)-${LABEL}"
mkdir -p "results/${BATCH}"

echo "Warm-up dulu, hasilnya tidak dihitung."
RUN_DIR="${BATCH}/_warmup" ./bench/run.sh redis 1000 100 >/dev/null || true
rm -rf "results/${BATCH}/_warmup"

failed=""
for r in $(seq 1 "$ROUNDS"); do
  for s in $STRATEGIES; do
    echo "Run ${r}/${ROUNDS}, ${s}"
    if ! RUN_DIR="${BATCH}/r${r}-${s}" ./bench/run.sh "$s" "$BUYERS" "$STOCK" | grep -E '^\[' ; then
      failed="${failed} r${r}-${s}"
    fi
    sleep "$COOLDOWN"
  done
done

echo
./bench/summarize.sh "results/${BATCH}" | tee "results/${BATCH}/summary.md"

if [ -n "$failed" ]; then
  echo "Ada run yang gagal:${failed}" >&2
  exit 1
fi
