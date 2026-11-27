## War Tiket: perbandingan strategi (go)

5000 pembeli, stok 500, 2026-11-26 18:22

| Strategi | Terjual | Oversold | Konsisten | 503 | 504 | p50 | p99 | Antre p99 |
|---|---:|---:|:---:|---:|---:|---:|---:|---:|
| naive | 1000 | 500 | **tidak** | 0 | 0 | 152 ms | 652.6 ms | - |
| forupdate | 500 | 0 | ya | 0 | 0 | 331 ms | 635.2 ms | - |
| redis | 500 | 0 | ya | 0 | 0 | 42.6 ms | 146.4 ms | - |
| queue | 500 | 0 | ya | 0 | 0 | 31.5 ms | 77.5 ms | 229 ms |

p50/p99 = latency POST beli (purchase_latency). 503/504 dihitung dari sisi k6.

Run:
- `results/20261126-182212-go-naive`
- `results/20261126-182223-go-forupdate`
- `results/20261126-182232-go-redis`
- `results/20261126-182240-go-queue`
