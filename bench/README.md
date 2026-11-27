# bench/

Di folder ini ada semua tooling untuk menguji War Tiket: contract test, load test, dan script untuk inject latency ke database. k6-nya jalan di container lewat `docker compose --profile bench`, jadi di laptop cukup ada `docker`, `curl`, dan `jq`.

## Isinya

`k6/smoke.js` adalah contract test. Setiap implementasi wajib lulus ini dulu sebelum boleh di-benchmark. Script ini me-reset stok jadi 3, mengecek semua status code (201, 200, 409, 400, 404), mencoba flow queue kalau strateginya `queue`, lalu memastikan di akhir tidak ada oversold dan datanya consistent. Satu cek saja gagal, exit code-nya langsung bukan 0.

`k6/spike.js` mensimulasikan flash sale dengan traffic spike. Defaultnya 5.000 pembeli unik dengan 1.000 VU, paling lama 60 detik. Untuk strategi `queue`, tiap pembeli mengecek `poll_url` setiap 200 ms sampai statusnya `confirmed` atau `sold_out`, maksimal 30 detik. Script ini tidak me-reset apa pun karena itu tugas `run.sh`.

`run.sh` menjalankan satu run dari awal sampai akhir. Dia mengecek app sudah hidup, me-reset event, menjalankan `spike.js`, menunggu persist lag ke Postgres jadi nol, menyimpan hasilnya ke `results/<run>/`, lalu mencetak summary satu baris.

`compare.sh` menjalankan `run.sh` untuk keempat strategi berturut-turut dengan load yang sama, lalu mencetak tabel perbandingan dalam Markdown. Tabelnya juga disimpan ke `results/compare-<timestamp>.md`.

`series.sh` dipakai untuk angka yang mau dipublikasikan. Dia menjalankan keempat strategi secara interleaved selama beberapa run (default 5), didahului satu warm-up run yang hasilnya dibuang. Semua run masuk ke satu folder `results/series-<waktu>-<label>/`.

`summarize.sh` membaca folder series tadi dan membuat tabel median. Kalau angkanya beda-beda antar-run, range min–max ikut ditulis di dalam kurung. `series.sh` memanggilnya otomatis di akhir dan menyimpan hasilnya ke `summary.md`.

`chaos.sh` menyalakan atau mematikan latency injection ke Postgres lewat Toxiproxy.

`export-replay.sh` mengubah isi `results-published/` jadi `docs/data.js` untuk halaman replay. Untuk tiap strategi, yang di-replay adalah run dengan p50 median dari lima run.

## Cara pakai

```bash
docker compose up -d --build                     # infra + app Go

# Contract test dulu (ganti STRATEGY untuk tiap strategi)
docker compose --profile bench run --rm -e STRATEGY=redis k6 run /bench/k6/smoke.js

./bench/run.sh redis                             # 5000 pembeli, stok 500
./bench/run.sh queue 10000 1000                  # load custom
./bench/compare.sh                               # keempat strategi

./bench/chaos.sh on 40 10                        # Postgres +40ms ±10ms
./bench/compare.sh
./bench/chaos.sh off

./bench/series.sh baseline                       # 5 run per strategi, lalu tabel median
./bench/summarize.sh results/series-<waktu>-baseline   # buat ulang tabelnya
```

Ada beberapa env yang bisa diatur kalau perlu. `APP_PORT` defaultnya 8088, `VUS` defaultnya 1000, dan `EVENT_ID` defaultnya 1. Isi `ADMIN_TOKEN` kalau server mewajibkan header `X-Admin-Token`. Untuk `compare.sh` ada juga `STRATEGIES` dan `COOLDOWN`.

## Format hasil

Setiap run menghasilkan satu folder `results/<YYYYmmdd-HHMMSS>-go-<strategi>/` berisi tiga file.

`meta.json` mencatat konteks run-nya: strategi, jumlah pembeli, stok, VU, implementasi, git commit (atau `uncommitted` kalau belum di-commit), spec mesin, status chaos, dan timestamp.

`report.json` adalah response `GET /admin/report` setelah persist lag nol. Di dalamnya ada angka oversold, status consistency, stok di DB dan Redis, outcome counter dari sisi server, dan `timeline` per 10 ms.

`k6-summary.json` berisi summary dari sisi client. Ada jumlah tiap hasil (`outcome_created`, `outcome_sold_out`, dan seterusnya), latency `purchase_latency`, `queue_wait`, dan `purchase_e2e`, plus parameter run di blok `war_tiket`.

Halaman replay di `docs/` membaca ketiga file ini lewat `export-replay.sh`. `timeline` dipakai untuk animasi penjualan, summary k6 untuk angka latency, dan `meta.json` untuk label. Jadi kalau nama atau strukturnya diubah, skrip ekspornya juga perlu ikut disesuaikan.

## Supaya hasilnya adil

Bandingkan angka hanya dari mesin dan setelan Docker yang sama. Container app sudah dibatasi 2 CPU dan 512 MB di `docker-compose.yml`, jadi yang perlu dijaga tinggal kondisi laptopnya.

Sebelum mulai, tutup aplikasi yang berat seperti browser dengan banyak tab atau IDE, dan colok charger kalau pakai laptop supaya CPU tidak diperlambat.

Pakai `series.sh` kalau angkanya mau dipublikasikan. Script itu sudah mengurus warm-up, mengulang tiap strategi lima kali, dan melaporkan median beserta range-nya, jadi tidak ada yang tergoda memilih hasil terbaik.

Ubah satu hal saja per percobaan, entah strateginya, chaos-nya, atau jumlah pembelinya. Cek `./bench/chaos.sh status` sebelum mulai supaya tidak lupa chaos masih menyala. Statusnya juga ikut tercatat di `meta.json`.

Commit kode sebelum benchmark. `meta.json` mencatat commit-nya, jadi setiap hasil bisa dilacak ke versi kode yang dipakai.

Satu hal lagi, k6 jalan di mesin yang sama dengan app, jadi angka absolutnya ikut terpengaruh. Yang paling bisa dipegang adalah perbandingan antarstrategi dalam kondisi yang sama.
