#!/usr/bin/env bash
: <<'CATATAN'
Ubah hasil di results-published/ jadi docs/data.js untuk halaman replay.

Cara pakai: ./bench/export-replay.sh

Untuk tiap strategi, yang diputar adalah run dengan p50 di tengah dari
lima run, bukan run terbaik. Tabel perbandingan tetap memakai median dari
semua run. Datanya ditulis sebagai file JS (bukan JSON) supaya halaman bisa
dibuka langsung dari disk tanpa server.
CATATAN
set -euo pipefail
cd "$(dirname "$0")/.."

PUB=results-published
BASE="$PUB/2026-11-27-baseline"
SLOW="$PUB/2026-11-27-db-lambat"
LOW="$PUB/2026-11-27-db-lambat-beban-rendah"
OUT=docs/data.js

# Satu baris JSON per run.
run_json() {
  local d="$1" scenario="$2" key="$3"
  jq -nc \
    --slurpfile rep "$d/report.json" \
    --slurpfile k6 "$d/k6-summary.json" \
    --slurpfile meta "$d/meta.json" \
    --arg scenario "$scenario" --arg key "$key" --arg dir "${d#"$PUB"/}" '
    ($k6[0].metrics) as $m
    | ($rep[0].timeline.points) as $p
    # Buang ekor yang isinya diam (sampler terus merekam 2 detik setelah sepi).
    | ([range(1; $p | length) | select($p[.][1:] != $p[. - 1][1:])] | last // 0) as $lastChange
    | {
        scenario: $scenario, key: $key, dir: $dir,
        strategy: $rep[0].strategy,
        vus: $meta[0].vus, buyers: $meta[0].buyers, capacity: $rep[0].capacity,
        commit: $meta[0].git_commit, cpu: $meta[0].host.cpu_model,
        tickets: $rep[0].db.tickets, oversold: $rep[0].oversold, consistent: $rep[0].consistent,
        created: $rep[0].app.outcomes.created, soldOut: $rep[0].app.outcomes.sold_out,
        e503: ($m.outcome_overloaded.values.count // 0),
        e504: ($m.outcome_timeout.values.count // 0),
        p50: ($m.purchase_latency.values.med // 0),
        p99: ($m.purchase_latency.values["p(99)"] // 0),
        qp99: ($m.queue_wait.values["p(99)"] // null),
        interval: $rep[0].timeline.interval_ms,
        points: $p[0:([$lastChange + 15, ($p | length)] | min)]
      }'
}

{
  for d in "$BASE"/r*-*/; do d=${d%/}; run_json "$d" normal "$(basename "$d" | cut -d- -f2)"; done
  for d in "$SLOW"/r*-*/; do d=${d%/}; run_json "$d" slow "$(basename "$d" | cut -d- -f2)"; done
  run_json "$LOW/naive-50vu" load 50
  run_json "$LOW/naive-200vu" load 200
} | jq -sc '
  def median: sort | if length % 2 == 1 then .[length / 2 | floor] else (.[length / 2 - 1] + .[length / 2]) / 2 end;
  def stat(f): [.[] | f] | {med: median, min: min, max: max};
  def pick: sort_by(.p50) | .[(length - 1) / 2 | floor];
  def series($sc):
    [.[] | select(.scenario == $sc)] as $rs
    | ["naive", "forupdate", "redis", "queue"] | map(. as $s | [$rs[] | select(.strategy == $s)]) | map(select(length > 0))
    | {
        runs: (map(pick | del(.scenario)) | map({key: .strategy, value: .}) | from_entries),
        summary: map({
          strategy: .[0].strategy, n: length,
          tickets: stat(.tickets), oversold: stat(.oversold),
          consistent: (map(select(.consistent)) | length),
          e503: stat(.e503), e504: stat(.e504),
          p50: stat(.p50), p99: stat(.p99),
          qp99: (if .[0].qp99 == null then null else stat(.qp99) end)
        })
      };
  . as $all
  | (series("slow").runs.naive) as $slowNaive
  | {
      repo: "https://github.com/kocwng/war-ticket",
      commit: ($all[0].commit | sub("-dirty$"; "")),
      cpu: $all[0].cpu,
      normal: series("normal"),
      slow: series("slow"),
      load: {
        runs: {
          "50": ([$all[] | select(.scenario == "load" and .key == "50")][0] | del(.scenario)),
          "200": ([$all[] | select(.scenario == "load" and .key == "200")][0] | del(.scenario)),
          "1000": $slowNaive
        }
      }
    }
' > "$OUT.json.tmp"

{
  echo "// Dibuat oleh bench/export-replay.sh dari results-published/. Jangan diedit manual."
  printf 'window.WAR_TICKET_DATA = '
  cat "$OUT.json.tmp"
  echo ';'
} > "$OUT"
rm "$OUT.json.tmp"
echo "Data replay ditulis ke $OUT ($(wc -c < "$OUT" | tr -d ' ') byte)."
