// War Tiket: uji kontrak API.
//
// Tidak terikat bahasa. Implementasi apa pun (Go, Python, Rust) harus lulus ini
// dulu sebelum di-benchmark. Jalannya cuma satu VU dan satu iterasi, dan exit
// code-nya langsung bukan 0 kalau ada satu cek saja yang gagal.
//
// Env:
//   BASE_URL     (default http://localhost:8088)
//   STRATEGY     naive|forupdate|redis|queue (default redis)
//   EVENT_ID     (default 1)
//   ADMIN_TOKEN  opsional, dikirim sebagai header X-Admin-Token ke /admin
//
// Contoh:
//   docker compose --profile bench run --rm -e STRATEGY=queue k6 run /bench/k6/smoke.js
//   k6 run -e BASE_URL=http://localhost:8088 -e STRATEGY=forupdate bench/k6/smoke.js

import http from 'k6/http';
import { check, group, sleep, fail } from 'k6';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8088').replace(/\/+$/, '');
const STRATEGY = __ENV.STRATEGY || 'redis';
const EVENT_ID = parseInt(__ENV.EVENT_ID || '1', 10);
const STOCK = 3;
const UNKNOWN_EVENT = 999999;
const IS_QUEUE = STRATEGY === 'queue';

const QUEUE_POLL_INTERVAL_S = 0.2;
const QUEUE_MAX_WAIT_MS = 10000;
const REPORT_MAX_WAIT_MS = 10000;

export const options = {
  scenarios: {
    smoke: { executor: 'per-vu-iterations', vus: 1, iterations: 1, maxDuration: '60s' },
  },
  thresholds: {
    checks: ['rate==1.0'],
  },
};

// Semua status yang ada di kontrak ditandai expected supaya http_req_failed
// tidak bikin bingung. Benar atau salahnya respons dinilai lewat check di bawah.
http.setResponseCallback(http.expectedStatuses(200, 201, 202, 204, 400, 404, 409));

const JSON_HEADERS = { 'Content-Type': 'application/json' };
const ADMIN_HEADERS = Object.assign(
  {},
  JSON_HEADERS,
  __ENV.ADMIN_TOKEN ? { 'X-Admin-Token': __ENV.ADMIN_TOKEN } : {},
);

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function parseJSON(res) {
  try {
    return res.json();
  } catch (_) {
    return null;
  }
}

function absoluteUrl(pathOrUrl) {
  if (/^https?:\/\//.test(pathOrUrl)) return pathOrUrl;
  return BASE_URL + (pathOrUrl.startsWith('/') ? '' : '/') + pathOrUrl;
}

function getEvent(id) {
  const res = http.get(`${BASE_URL}/v1/events/${id}`, { tags: { name: 'get_event' } });
  return { res, body: parseJSON(res) };
}

function postPurchase(eventId, payload) {
  return http.post(`${BASE_URL}/v1/events/${eventId}/purchase`, JSON.stringify(payload), {
    headers: JSON_HEADERS,
    tags: { name: 'purchase' },
  });
}

// Polling antrean sampai confirmed atau sold_out, atau sampai waktunya habis.
function pollQueue(entry) {
  const url = absoluteUrl(entry.poll_url);
  const start = Date.now();
  let last = null;
  while (Date.now() - start < QUEUE_MAX_WAIT_MS) {
    const res = http.get(url, { tags: { name: 'queue_poll' } });
    last = parseJSON(res);
    if (res.status !== 200 || !last) return { status: `http_${res.status}`, body: last };
    if (last.status === 'confirmed' || last.status === 'sold_out') return { status: last.status, body: last };
    sleep(QUEUE_POLL_INTERVAL_S);
  }
  return { status: 'poll_timeout', body: last };
}

// Beli, lalu ringkas hasilnya jadi satu nilai kind. Kalau dapat 202, antreannya
// ditunggu dulu sampai selesai.
// kind: created | replayed | sold_out | queued_confirmed | queued_sold_out | lainnya
function buy(userId) {
  const res = postPurchase(EVENT_ID, { user_id: userId });
  const body = parseJSON(res);
  const out = { res, body, http: res.status, kind: `http_${res.status}`, ticketId: null };

  if (res.status === 201 || res.status === 200) {
    out.kind = res.status === 201 ? 'created' : 'replayed';
    out.ticketId = body && body.ticket_id;
  } else if (res.status === 409) {
    out.kind = body && body.error === 'sold_out' ? 'sold_out' : 'http_409_bad_body';
  } else if (res.status === 202) {
    const entryOk = check(body, {
      'antrean: 202 ada queue_id, status, poll_url': (b) =>
        !!b && typeof b.queue_id === 'string' && typeof b.status === 'string' && typeof b.poll_url === 'string',
    });
    if (!entryOk) {
      out.kind = 'queued_bad_body';
      return out;
    }
    const q = pollQueue(body);
    out.queue = q;
    if (q.status === 'confirmed') {
      out.kind = 'queued_confirmed';
      out.ticketId = q.body && q.body.ticket_id;
    } else if (q.status === 'sold_out') {
      out.kind = 'queued_sold_out';
    } else {
      out.kind = `queued_${q.status}`;
    }
  }
  return out;
}

function isValidTicket(body, userId) {
  return (
    !!body &&
    typeof body.ticket_id === 'string' &&
    UUID_RE.test(body.ticket_id) &&
    body.event_id === EVENT_ID &&
    body.user_id === userId &&
    body.strategy === STRATEGY
  );
}

export default function () {
  group('healthz', () => {
    const res = http.get(`${BASE_URL}/healthz`, { tags: { name: 'healthz' } });
    const ok = check(res, {
      'healthz: 200': (r) => r.status === 200,
      'healthz: status=ok': (r) => (parseJSON(r) || {}).status === 'ok',
    });
    if (!ok) fail(`App di ${BASE_URL} belum siap. Smoke test berhenti.`);
  });

  group('reset', () => {
    const res = http.post(
      `${BASE_URL}/admin/reset`,
      JSON.stringify({ event_id: EVENT_ID, stock: STOCK, strategy: STRATEGY }),
      { headers: ADMIN_HEADERS, tags: { name: 'reset' } },
    );
    const ok = check(res, { 'reset: 204': (r) => r.status === 204 });
    if (!ok) fail(`Reset gagal dengan status ${res.status}. Smoke test berhenti.`);
  });

  group('event awal', () => {
    const { res, body } = getEvent(EVENT_ID);
    check(res, { 'event: 200': (r) => r.status === 200 });
    check(body, {
      'event: stock = 3': (b) => !!b && b.stock === STOCK,
      'event: capacity = 3': (b) => !!b && b.capacity === STOCK,
      'event: strategy sama dengan reset': (b) => !!b && b.strategy === STRATEGY,
    });
  });

  const buyers = ['smoke-u1', 'smoke-u2', 'smoke-u3'];
  const tickets = {};

  group('3 pembeli dapat tiket', () => {
    for (const u of buyers) {
      const r = buy(u);
      if (IS_QUEUE) {
        check(r, {
          [`beli ${u}: 202 lalu confirmed`]: (x) => x.http === 202 && x.kind === 'queued_confirmed',
          [`beli ${u}: ticket_id UUID`]: (x) => typeof x.ticketId === 'string' && UUID_RE.test(x.ticketId),
        });
      } else {
        check(r, {
          [`beli ${u}: 201`]: (x) => x.http === 201,
          [`beli ${u}: body tiket valid`]: (x) => isValidTicket(x.body, u),
        });
      }
      tickets[u] = r.ticketId;
    }
    const ids = buyers.map((u) => tickets[u]).filter(Boolean);
    check(ids, { 'beli: 3 ticket_id unik': (x) => x.length === 3 && new Set(x).size === 3 });
  });

  group('beli ulang idempoten', () => {
    const u = buyers[0];
    const r = buy(u);
    if (IS_QUEUE) {
      check(r, {
        'beli ulang: 200, atau 202 lalu confirmed': (x) => x.kind === 'replayed' || x.kind === 'queued_confirmed',
      });
    } else {
      check(r, {
        'beli ulang: 200': (x) => x.http === 200,
        'beli ulang: body tiket valid': (x) => isValidTicket(x.body, u),
      });
    }
    check(r, { 'beli ulang: ticket_id sama': (x) => !!tickets[u] && x.ticketId === tickets[u] });
  });

  group('stok habis', () => {
    const r = buy('smoke-u4');
    if (IS_QUEUE) {
      check(r, { 'pembeli ke-4: 409, atau 202 lalu sold_out': (x) => x.kind === 'sold_out' || x.kind === 'queued_sold_out' });
    } else {
      check(r, {
        'pembeli ke-4: 409': (x) => x.http === 409,
        'pembeli ke-4: error=sold_out': (x) => x.kind === 'sold_out',
      });
    }
    const { body } = getEvent(EVENT_ID);
    check(body, { 'event: stock = 0 setelah habis': (b) => !!b && b.stock === 0 });
  });

  group('validasi input', () => {
    const res = postPurchase(EVENT_ID, {});
    check(res, {
      'body {}: 400': (r) => r.status === 400,
      'body {}: error=bad_request': (r) => (parseJSON(r) || {}).error === 'bad_request',
    });
  });

  group('event tidak dikenal', () => {
    const { res, body } = getEvent(UNKNOWN_EVENT);
    check(res, { 'GET event 999999: 404': (r) => r.status === 404 });
    check(body, { 'GET event 999999: error=not_found': (b) => !!b && b.error === 'not_found' });

    const pres = postPurchase(UNKNOWN_EVENT, { user_id: 'smoke-u1' });
    check(pres, {
      'beli event 999999: 404': (r) => r.status === 404,
      'beli event 999999: error=not_found': (r) => (parseJSON(r) || {}).error === 'not_found',
    });
  });

  group('report konsisten', () => {
    // Di strategi berbasis Redis, pesanan baru masuk Postgres belakangan lewat
    // stream. Tunggu persist_lag jadi 0 dulu supaya angka di report sudah final.
    const start = Date.now();
    let res;
    let report = null;
    while (Date.now() - start < REPORT_MAX_WAIT_MS) {
      res = http.get(`${BASE_URL}/admin/report?event_id=${EVENT_ID}`, {
        headers: ADMIN_HEADERS,
        tags: { name: 'report' },
      });
      report = parseJSON(res);
      const lag = report && report.redis ? report.redis.persist_lag : undefined;
      if (res.status === 200 && report && (lag === undefined || lag === null || lag === 0)) break;
      sleep(0.25);
    }
    const lag = report && report.redis ? report.redis.persist_lag : undefined;
    check(res, { 'report: 200': (r) => !!r && r.status === 200 });
    check(report, {
      'report: persist_lag = 0 dalam 10 detik': () => lag === undefined || lag === null || lag === 0,
      'report: strategy sama dengan reset': (r) => !!r && r.strategy === STRATEGY,
      'report: oversold = 0': (r) => !!r && r.oversold === 0,
      'report: db.tickets = 3': (r) => !!r && !!r.db && r.db.tickets === STOCK,
      'report: consistent = true': (r) => !!r && r.consistent === true,
    });
  });
}
