/*
War Tiket: simulasi flash sale.

Sebanyak BUYERS pembeli unik menyerbu bersamaan. Satu pembeli punya satu
user_id dan mengirim satu request beli. Script ini tidak memanggil
/admin/reset karena stok dan strategi sudah disiapkan bench/run.sh.

Env:
  BASE_URL  (default http://localhost:8088)
  STRATEGY  naive|forupdate|redis|queue, dipakai untuk tag dan flow queue
  BUYERS    jumlah pembeli unik (default 5000)
  VUS       jumlah VU yang jalan paralel (default 1000)
  EVENT_ID  (default 1)
  RUN_DIR   nama folder di /results untuk menyimpan k6-summary.json

Sengaja tidak ada threshold yang bisa menggagalkan run. Hasil strategi naive
memang diharapkan jelek, tapi tetap perlu direkam.
*/

import http from 'k6/http';
import exec from 'k6/execution';
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.1.0/index.js';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8088').replace(/\/+$/, '');
const STRATEGY = __ENV.STRATEGY || 'unknown';
const BUYERS = parseInt(__ENV.BUYERS || '5000', 10);
const VUS = Math.max(1, Math.min(parseInt(__ENV.VUS || '1000', 10), BUYERS));
const EVENT_ID = parseInt(__ENV.EVENT_ID || '1', 10);
const RUN_DIR = __ENV.RUN_DIR || '';

const PURCHASE_TIMEOUT = '15s'; // lebih lama dari REQUEST_TIMEOUT server (2s), supaya 504 dari server keluar duluan
const QUEUE_POLL_INTERVAL_S = 0.2;
const QUEUE_MAX_WAIT_MS = 30000;

// Satu counter per outcome akhir. Pembeli di queue dihitung setelah status final-nya keluar.
const outcomes = {
  created: new Counter('outcome_created'),
  replayed: new Counter('outcome_replayed'),
  sold_out: new Counter('outcome_sold_out'),
  overloaded: new Counter('outcome_overloaded'),
  timeout: new Counter('outcome_timeout'),
  error: new Counter('outcome_error'),
};
const queueEntered = new Counter('queue_entered'); // berapa kali dapat 202

// Durasi POST purchase saja, belum termasuk waktu di queue.
const purchaseLatency = new Trend('purchase_latency', true);
// Dari dapat 202 sampai queue memberi status final (confirmed atau sold_out).
const queueWait = new Trend('queue_wait', true);
/*
Dari klik beli sampai dapat hasil final. Di luar strategi queue, nilainya sama
dengan purchase_latency.
*/
const purchaseE2E = new Trend('purchase_e2e', true);

export const options = {
  scenarios: {
    flash_sale: {
      executor: 'shared-iterations',
      vus: VUS,
      iterations: BUYERS,
      maxDuration: '60s',
      gracefulStop: '35s', // kasih waktu pembeli yang masih di queue untuk menyelesaikan polling
    },
  },
  tags: { strategy: STRATEGY, event_id: String(EVENT_ID) },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  /*
  Threshold kosong tidak menilai apa-apa. Gunanya cuma supaya submetric per
  outcome ikut muncul di summary.
  */
  thresholds: {
    'purchase_latency{outcome:created}': [],
    'purchase_latency{outcome:replayed}': [],
    'purchase_latency{outcome:queued}': [],
    'purchase_latency{outcome:sold_out}': [],
    'purchase_latency{outcome:overloaded}': [],
    'purchase_latency{outcome:timeout}': [],
    'http_req_duration{name:purchase}': [],
    'http_req_duration{name:queue_poll}': [],
  },
};

// 409 dan 202 itu jawaban normal, bukan kegagalan HTTP.
http.setResponseCallback(http.expectedStatuses(200, 201, 202, 409));

const JSON_HEADERS = { 'Content-Type': 'application/json' };

function parseJSON(res) {
  try {
    return res.json();
  } catch (_) {
    return null;
  }
}

// Terjemahkan status respons POST beli jadi nama outcome.
function classify(res) {
  switch (res.status) {
    case 201: return 'created';
    case 200: return 'replayed';
    case 202: return 'queued';
    case 409: return 'sold_out';
    case 503: return 'overloaded';
    case 504: return 'timeout';
    case 0:
      // 1050 artinya request timeout di sisi k6
      return res.error_code === 1050 ? 'timeout' : 'error';
    default: return 'error';
  }
}

function absoluteUrl(pathOrUrl) {
  if (/^https?:\/\//.test(pathOrUrl)) return pathOrUrl;
  return BASE_URL + (pathOrUrl.startsWith('/') ? '' : '/') + pathOrUrl;
}

// Polling queue sampai confirmed atau sold_out, lalu kembalikan outcome finalnya.
function waitInQueue(entry) {
  const pollUrl = absoluteUrl(entry.poll_url || `/v1/queue/${entry.queue_id}`);
  const start = Date.now();
  let final = 'timeout';

  while (Date.now() - start < QUEUE_MAX_WAIT_MS) {
    sleep(QUEUE_POLL_INTERVAL_S);
    const res = http.get(pollUrl, { tags: { name: 'queue_poll' }, timeout: '5s' });
    if (res.status === 503 || res.status === 504 || res.status === 0) continue; // gangguan sementara, coba lagi
    if (res.status !== 200) { final = 'error'; break; }

    const body = parseJSON(res);
    const status = body && body.status;
    if (status === 'confirmed') { final = 'created'; break; }
    if (status === 'sold_out') { final = 'sold_out'; break; }
    if (status !== 'waiting' && status !== 'admitted') { final = 'error'; break; }
  }

  const waited = Date.now() - start;
  queueWait.add(waited, { outcome: final });
  check(null, { 'queue: selesai sebelum timeout 30 detik': () => final !== 'timeout' }, { phase: 'queue' });
  return { outcome: final, waited };
}

export default function () {
  // iterationInTest unik di semua VU, jadi tiap pembeli dapat user_id sendiri.
  const userId = `u-${exec.scenario.iterationInTest + 1}`;

  const res = http.post(
    `${BASE_URL}/v1/events/${EVENT_ID}/purchase`,
    JSON.stringify({ user_id: userId }),
    { headers: JSON_HEADERS, tags: { name: 'purchase' }, timeout: PURCHASE_TIMEOUT },
  );

  let outcome = classify(res);
  const postMs = res.timings.duration;
  purchaseLatency.add(postMs, { outcome });

  check(res, {
    'beli: status code sesuai API contract (200/201/202/409/503/504)': (r) =>
      [200, 201, 202, 409, 503, 504].includes(r.status),
    'beli: tidak ada 5xx': (r) => r.status < 500,
  });

  let e2e = postMs;
  if (outcome === 'queued') {
    queueEntered.add(1);
    const entry = parseJSON(res);
    if (entry && (entry.poll_url || entry.queue_id)) {
      const q = waitInQueue(entry);
      outcome = q.outcome;
      e2e += q.waited;
    } else {
      outcome = 'error';
    }
  } else if (outcome === 'sold_out') {
    const body = parseJSON(res);
    check(body, { 'beli: 409 berisi error=sold_out': (b) => !!b && b.error === 'sold_out' });
  }

  outcomes[outcome].add(1);
  purchaseE2E.add(e2e, { outcome });
}

export function handleSummary(data) {
  // Sisipkan parameter run supaya file JSON ini tetap bisa dibaca tanpa meta.json.
  const enriched = Object.assign({}, data, {
    war_tiket: {
      strategy: STRATEGY,
      buyers: BUYERS,
      vus: VUS,
      event_id: EVENT_ID,
      base_url: BASE_URL,
    },
  });

  const out = {
    stdout: textSummary(data, { indent: ' ', enableColors: true }),
  };
  // Folder /results/<RUN_DIR> harus sudah dibuat run.sh karena k6 tidak bisa mkdir.
  const path = __ENV.SUMMARY_PATH || (RUN_DIR ? `/results/${RUN_DIR}/k6-summary.json` : 'k6-summary.json');
  out[path] = JSON.stringify(enriched, null, 2);
  return out;
}
