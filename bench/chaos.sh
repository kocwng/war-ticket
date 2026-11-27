#!/usr/bin/env bash
# Bikin koneksi app ke Postgres sengaja lambat lewat Toxiproxy.
#
#   ./bench/chaos.sh on [latency_ms=40] [jitter_ms=10]   # pasang toxic atau ganti nilainya
#   ./bench/chaos.sh off                                 # lepas toxic
#   ./bench/chaos.sh status                              # lihat toxic yang aktif
#
# Env opsional: TOXIPROXY_URL (default http://localhost:8474)
set -euo pipefail

TOXIPROXY_URL="${TOXIPROXY_URL:-http://localhost:8474}"
PROXY="postgres"
TOXIC="db_latency"
BASE="${TOXIPROXY_URL}/proxies/${PROXY}"

usage() {
  echo "Cara pakai: $0 on [latency_ms=40] [jitter_ms=10] | off | status" >&2
  exit 2
}
die() { echo "GAGAL: $*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "'curl' belum terpasang"
command -v jq >/dev/null 2>&1 || die "'jq' belum terpasang"

# Cek Toxiproxy hidup dan proxy postgres-nya sudah terdaftar.
check_proxy() {
  curl -fsS --max-time 3 "${BASE}" >/dev/null 2>&1 \
    || die "proxy '${PROXY}' tidak ada di ${TOXIPROXY_URL}. Pastikan Toxiproxy sudah jalan."
}

# Hapus toxic. Return 0 kalau berhasil dihapus, 1 kalau memang belum ada (404).
# Status HTTP lain langsung dianggap error.
remove_toxic() {
  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X DELETE "${BASE}/toxics/${TOXIC}") || code="000"
  case "$code" in
    204|200) return 0 ;;
    404) return 1 ;;
    *) die "gagal menghapus toxic ${TOXIC}, dapat HTTP ${code}" ;;
  esac
}

show_status() {
  local toxics
  toxics=$(curl -fsS --max-time 3 "${BASE}/toxics") || die "gagal membaca toxic dari ${BASE}/toxics"
  if [ "$(printf '%s' "$toxics" | jq 'length')" = "0" ]; then
    echo "Chaos mati. Tidak ada toxic di proxy '${PROXY}'."
  else
    echo "Toxic aktif di proxy '${PROXY}':"
    printf '%s' "$toxics" | jq -r '.[] | "- \(.name) (\(.type), \(.stream), toxicity \(.toxicity)): \(.attributes | tojson)"'
  fi
}

[ $# -ge 1 ] || usage
cmd="$1"
case "$cmd" in
  on)
    latency="${2:-40}"
    jitter="${3:-10}"
    case "$latency" in ''|*[!0-9]*) die "latency_ms harus berupa angka, bukan '$latency'" ;; esac
    case "$jitter" in ''|*[!0-9]*) die "jitter_ms harus berupa angka, bukan '$jitter'" ;; esac
    check_proxy
    # Kalau toxic-nya sudah ada, hapus dulu supaya bisa dipasang ulang dengan nilai baru.
    remove_toxic || true
    body=$(jq -cn --arg name "$TOXIC" --argjson l "$latency" --argjson j "$jitter" \
      '{name: $name, type: "latency", stream: "downstream", toxicity: 1.0, attributes: {latency: $l, jitter: $j}}')
    code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
      -H 'Content-Type: application/json' -d "$body" "${BASE}/toxics") || code="000"
    case "$code" in 200|201) ;; *) die "gagal memasang toxic, dapat HTTP ${code}" ;; esac
    echo "Chaos nyala. Latency Postgres ditambah ${latency}ms, jitter ${jitter}ms."
    ;;
  off)
    check_proxy
    if remove_toxic; then
      echo "Chaos mati. Toxic ${TOXIC} sudah dilepas."
    else
      echo "Chaos memang sudah mati, toxic ${TOXIC} tidak ada."
    fi
    ;;
  status)
    check_proxy
    show_status
    ;;
  *)
    usage
    ;;
esac
