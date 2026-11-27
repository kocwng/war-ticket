#!/usr/bin/env bash
# Ringkas satu folder seri jadi tabel median.
#
# Cara pakai: ./bench/summarize.sh results/series-<waktu>-<label>
#
# Angka di tabel adalah median dari semua putaran. Kalau angkanya beda-beda
# antarputaran, rentang min–maks ditulis di dalam kurung.
set -euo pipefail

DIR="${1:?Kasih folder seri, misalnya results/series-20261127-101500-baseline}"

rows=$(for d in "$DIR"/r*-*/; do
  [ -f "$d/report.json" ] && [ -f "$d/k6-summary.json" ] || continue
  jq -n \
    --slurpfile rep "$d/report.json" \
    --slurpfile k6 "$d/k6-summary.json" \
    --slurpfile meta "$d/meta.json" '
    ($k6[0].metrics) as $m | {
      strategy: $rep[0].strategy,
      buyers: $meta[0].buyers, stock: $meta[0].stock,
      commit: $meta[0].git_commit, cpu: $meta[0].host.cpu_model,
      chaos: ($meta[0].chaos.enabled // false),
      tickets: $rep[0].db.tickets,
      oversold: $rep[0].oversold,
      consistent: $rep[0].consistent,
      e503: ($m.outcome_overloaded.values.count // 0),
      e504: ($m.outcome_timeout.values.count // 0),
      p50: ($m.purchase_latency.values.med // 0),
      p99: ($m.purchase_latency.values["p(99)"] // 0),
      qp99: ($m.queue_wait.values["p(99)"] // null)
    }'
done)

if [ -z "$rows" ]; then
  echo "Tidak ada hasil run di $DIR" >&2
  exit 1
fi

echo "$rows" | jq -rs '
  def median: sort | if length == 0 then null
    elif length % 2 == 1 then .[length / 2 | floor]
    else (.[length / 2 - 1] + .[length / 2]) / 2 end;
  def num: if . == null then "-" elif . >= 1000 then (. | round | tostring | if length > 3 then .[:-3] + "." + .[-3:] else . end) else (. | round | tostring) end;
  def ms: if . == null then "-" elif . >= 1000 then ((. / 100 | round) / 10 | tostring) + " dtk" else (. | round | tostring) + " ms" end;
  def cell(f; fmt): [.[] | f] as $v | ($v | median) as $md | ($v | min) as $lo | ($v | max) as $hi
    | if ($lo | fmt) == ($hi | fmt) then ($md | fmt) else "\($md | fmt) (\($lo | fmt)–\($hi | fmt))" end;

  . as $all
  | ($all[0]) as $h
  | "\($all | map(.strategy) | unique | length) strategi, \(($all | length) / ($all | map(.strategy) | unique | length) | floor) putaran masing-masing. \($h.buyers | num) pembeli, stok \($h.stock | num), \($h.cpu), commit \($h.commit)\(if $h.chaos then ", DB dilambatkan lewat Toxiproxy" else "" end).",
    "",
    "| Strategi | Tiket terbit | Oversold | Konsisten | 503 | 504 | p50 | p99 | Antre p99 |",
    "|---|---:|---:|:---:|---:|---:|---:|---:|---:|",
    ( ["naive", "forupdate", "redis", "queue"][] as $s
      | [$all[] | select(.strategy == $s)] | select(length > 0)
      | "| `\($s)` | \(cell(.tickets; num)) | \(cell(.oversold; num)) | \(map(select(.consistent)) | length)/\(length) | \(cell(.e503; num)) | \(cell(.e504; num)) | \(cell(.p50; ms)) | \(cell(.p99; ms)) | \(if $s == "queue" then cell(.qp99; ms) else "-" end) |" )
'
