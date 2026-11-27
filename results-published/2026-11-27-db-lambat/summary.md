4 strategi, 5 run masing-masing. 5.000 pembeli, stok 500, Apple M2, commit 4fdb181-dirty, DB dilambatkan lewat Toxiproxy.

| Strategi | Tiket terbit | Oversold | Consistent | 503 | 504 | p50 | p99 | Queue p99 |
|---|---:|---:|:---:|---:|---:|---:|---:|---:|
| `naive` | 0 | 0 | 5/5 | 0 | 5.000 | 2 dtk | 2.1 dtk (2.1 dtk–2.2 dtk) | - |
| `forupdate` | 1 (0–4) | 0 | 5/5 | 0 | 4.999 (4.996–5.000) | 2 dtk | 2.2 dtk (2.1 dtk–2.5 dtk) | - |
| `redis` | 500 | 0 | 5/5 | 0 | 0 | 36 ms (30 ms–47 ms) | 110 ms (66 ms–252 ms) | - |
| `queue` | 500 | 0 | 5/5 | 0 | 0 | 43 ms (9 ms–49 ms) | 172 ms (82 ms–268 ms) | 248 ms (226 ms–297 ms) |
