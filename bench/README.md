# bench/

Di folder ini ada semua alat untuk menguji War Tiket: uji kontrak, load test, dan skrip untuk sengaja bikin database melambat. k6-nya jalan di container lewat `docker compose --profile bench`, jadi di laptop cukup ada `docker`, `curl`, dan `jq`.

## Isinya

`k6/smoke.js` adalah uji kontrak. Setiap implementasi wajib lulus ini dulu sebelum boleh di-benchmark. Script ini me-reset stok jadi 3, mengecek semua kode status (201, 200, 409, 400, 404), mencoba alur antrean kalau strateginya `queue`, lalu memastikan di akhir tidak ada oversold dan datanya konsisten. Satu cek saja gagal, exit code-nya langsung bukan 0.

`k6/spike.js` mensimulasikan flash sale. Defaultnya 5.000 pembeli unik dengan 1.000 VU, paling lama 60 detik. Untuk strategi `queue`, tiap pembeli mengecek `poll_url` setiap 200 ms sampai statusnya `confirmed` atau `sold_out`, maksimal 30 detik. Script ini tidak me-reset apa pun karena itu tugas `run.sh`.

`run.sh` menjalankan satu run dari awal sampai akhir. Dia mengecek app sudah hidup, me-reset event, menjalankan `spike.js`, menunggu semua pesanan masuk ke Postgres, menyimpan hasilnya ke `results/<run>/`, lalu mencetak ringkasan satu baris.

`compare.sh` menjalankan `run.sh` untuk keempat strategi berturut-turut dengan beban yang sama, lalu mencetak tabel perbandingan dalam Markdown. Tabelnya juga disimpan ke `results/compare-<timestamp>.md`.

`chaos.sh` menyalakan atau mematikan latency tambahan ke Postgres lewat Toxiproxy.

## Cara pakai

```bash
docker compose up -d --build                     # infra + app Go

# Uji kontrak dulu (ganti STRATEGY untuk tiap strategi)
docker compose --profile bench run --rm -e STRATEGY=redis k6 run /bench/k6/smoke.js

./bench/run.sh redis                             # 5000 pembeli, stok 500
./bench/run.sh queue 10000 1000                  # beban custom
./bench/compare.sh                               # keempat strategi

./bench/chaos.sh on 40 10                        # Postgres +40ms ±10ms
./bench/compare.sh
./bench/chaos.sh off
```

Ada beberapa env yang bisa diatur kalau perlu. `APP_PORT` defaultnya 8088, `VUS` defaultnya 1000, dan `EVENT_ID` defaultnya 1. Isi `ADMIN_TOKEN` kalau server mewajibkan header `X-Admin-Token`. Untuk `compare.sh` ada juga `STRATEGIES` dan `COOLDOWN`.

## Format rekaman

Setiap run menghasilkan satu folder `results/<YYYYmmdd-HHMMSS>-go-<strategi>/` berisi tiga file.

`meta.json` mencatat konteks run-nya: strategi, jumlah pembeli, stok, VU, implementasi, commit git (atau `uncommitted` kalau belum di-commit), spesifikasi mesin, status chaos, dan waktu run.

`report.json` adalah respons `GET /admin/report` setelah semua pesanan tersimpan. Di dalamnya ada angka oversold, status konsistensi, stok di DB dan Redis, hitungan hasil dari sisi server, dan `timeline` per 10 ms.

`k6-summary.json` berisi ringkasan dari sisi pembeli. Ada jumlah tiap hasil (`outcome_created`, `outcome_sold_out`, dan seterusnya), latency `purchase_latency`, `queue_wait`, dan `purchase_e2e`, plus parameter run di blok `war_tiket`.

Halaman demo replay nanti membaca ketiga file ini. `timeline` dipakai untuk animasi penjualan, ringkasan k6 untuk angka latency, dan `meta.json` untuk label. Jadi kalau nama atau strukturnya diubah, halaman demonya juga perlu ikut disesuaikan.

## Supaya hasilnya adil

Bandingkan angka hanya dari mesin dan setelan Docker yang sama. Container app sudah dibatasi 2 CPU dan 512 MB di `docker-compose.yml`, jadi yang perlu dijaga tinggal kondisi laptopnya.

Sebelum mulai, tutup aplikasi yang berat seperti browser dengan banyak tab atau IDE, dan colok charger kalau pakai laptop supaya CPU tidak diperlambat.

Lakukan satu run pemanasan dulu, misalnya `./bench/run.sh redis 1000 100`, lalu hapus hasilnya. Setelah itu ulangi tiap strategi minimal lima kali dan laporkan mediannya, bukan hasil terbaik. Kalau angkanya naik-turun jauh, sebutkan juga rentangnya.

Ubah satu hal saja per percobaan, entah strateginya, chaos-nya, atau jumlah pembelinya. Cek `./bench/chaos.sh status` sebelum mulai supaya tidak lupa chaos masih menyala. Statusnya juga ikut tercatat di `meta.json`.

Commit kode sebelum benchmark. `meta.json` mencatat commit-nya, jadi setiap hasil bisa dilacak ke versi kode yang dipakai.

Satu hal lagi, k6 jalan di mesin yang sama dengan app, jadi angka absolutnya ikut terpengaruh. Yang paling bisa dipegang adalah perbandingan antarstrategi dalam kondisi yang sama.
