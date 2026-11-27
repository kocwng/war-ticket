4 strategi, 5 run masing-masing. 5.000 pembeli, stok 500, Apple M2, commit 4fdb181-dirty.

| Strategi | Tiket terbit | Oversold | Consistent | 503 | 504 | p50 | p99 | Queue p99 |
|---|---:|---:|:---:|---:|---:|---:|---:|---:|
| `naive` | 1.000 | 500 | 0/5 | 0 | 0 | 75 ms (72 ms–94 ms) | 368 ms (321 ms–484 ms) | - |
| `forupdate` | 500 | 0 | 5/5 | 0 | 0 | 207 ms (194 ms–212 ms) | 426 ms (400 ms–953 ms) | - |
| `redis` | 500 | 0 | 5/5 | 0 | 0 | 37 ms (27 ms–38 ms) | 87 ms (50 ms–203 ms) | - |
| `queue` | 500 | 0 | 5/5 | 0 | 0 | 33 ms (6 ms–36 ms) | 95 ms (71 ms–259 ms) | 277 ms (223 ms–305 ms) |
