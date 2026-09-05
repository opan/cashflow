# Cashflow

Aplikasi web sederhana untuk melacak arus kas (cash flow) bersama, transparan, dan
tidak bisa dimanipulasi. Daftar akun, buat sebuah **cashplan** (mis. "Uang kas
sekolah anak") dengan tautan pilihan Anda, catat pemasukan & pengeluaran, lalu
bagikan tautannya. Riwayat bersifat **append-only** — tidak bisa diubah atau
dihapus, sehingga semua orang melihat data yang sama.

> Antarmuka aplikasi berbahasa Indonesia. Dokumen ini dwibahasa.

## Fitur

- **Akun pengguna** (nama pengguna + kata sandi). Nama pengguna bersifat unik,
  siapa cepat dia dapat. Cashplan tersimpan di akun Anda, jadi
  tautan kelolanya tidak akan hilang — cukup masuk untuk mengaksesnya kembali.
- **Buat cashplan** dengan nama, deskripsi, dan **tautan (slug) pilihan sendiri**
  (mis. `/p/uang-kas-3a`). Slug unik lintas cashplan, siapa cepat dia dapat.
- **Satu formulir transaksi** dengan pilihan Jenis (Pemasukan/Pengeluaran):
  - Pemasukan: **pembayar**, jumlah, tanggal (otomatis, bisa diubah), catatan.
  - Pengeluaran: **keterangan** (mis. "beli kaos"), **penerima** (opsional), jumlah, tanggal.
- **Ringkasan**: total pemasukan, total pengeluaran, saldo, jumlah pembayar, jumlah transaksi.
- **Rincian per pembayar**: total, jumlah setoran, dan setoran terakhir tiap pembayar (khusus pemasukan), diurutkan dari kontributor terbesar.
- **Cari & halaman** pada riwayat: cari berdasarkan pembayar atau keterangan, dengan pagination (20 per halaman) karena riwayat bisa panjang.
- **Bagikan** cashplan lewat tautan `/p/{slug}` — pemilik (yang login) bisa mengedit,
  siapa pun yang punya tautan bisa melihat.
- **Riwayat append-only** — dijamin di level database (lihat di bawah).

## Tech stack

- **Backend + frontend**: Go (single binary) — `net/http` (routing bawaan Go 1.22+),
  `html/template` untuk server-side rendering, sedikit vanilla JS (salin tautan,
  formulir transaksi dinamis, saran slug).
- **Database**: PostgreSQL (driver `jackc/pgx/v5`).
- **Auth**: kata sandi di-hash dengan `bcrypt`; sesi berbasis cookie (httpOnly, SameSite=Lax) disimpan di tabel `sessions`.
- Template & aset statis di-*embed* ke dalam binary (`go:embed`) → satu berkas biner.

## Menjalankan

### Opsi A — Docker Compose (paling mudah)

```bash
docker compose up --build
```

Buka <http://localhost:8080>, lalu **Daftar** untuk membuat akun. Postgres berjalan
otomatis di dalam compose; skema dibuat otomatis saat aplikasi start.

### Opsi B — Lokal (Go + Postgres sendiri)

Butuh Go 1.26+ dan Postgres yang bisa diakses.

```bash
docker compose up -d db     # jalankan database saja
export DATABASE_URL="postgres://cashflow:cashflow@localhost:5432/cashflow?sslmode=disable"
export PORT=8080
go run .
```

## Konfigurasi

| Variabel       | Default                                                                 | Keterangan     |
| -------------- | ----------------------------------------------------------------------- | -------------- |
| `DATABASE_URL` | `postgres://cashflow:cashflow@localhost:5432/cashflow?sslmode=disable`   | DSN PostgreSQL |
| `PORT`         | `8080`                                                                   | Port HTTP      |

## Rute (routes)

| Method + Path                | Akses                    | Fungsi                                    |
| ---------------------------- | ------------------------ | ----------------------------------------- |
| `GET  /`                     | publik                   | Landing (belum login) / Dasbor (login)    |
| `GET/POST /register`         | publik                   | Daftar akun                               |
| `GET/POST /login`            | publik                   | Masuk                                     |
| `POST /logout`               | login                    | Keluar                                    |
| `POST /cashplans`            | login                    | Buat cashplan (title, slug, description)  |
| `GET  /kelola/{slug}`        | pemilik                  | Kelola: tambah transaksi, ringkasan, bagikan |
| `POST /kelola/{slug}/entry`  | pemilik                  | Tambah transaksi (income/expense)         |
| `GET  /p/{slug}`             | siapa pun dengan tautan  | Lihat (baca-saja) ringkasan & riwayat     |

## Model akses (design decision)

- **Kepemilikan = akun.** Setiap cashplan dimiliki oleh user yang membuatnya. Untuk
  membuat/mengelola cashplan, user harus masuk. Dasbor menampilkan semua cashplan
  milik user, sehingga tautan kelola tidak pernah hilang.
- **Berbagi = slug publik.** Tautan lihat `/p/{slug}` bisa diakses siapa saja tanpa
  login. Mengelola (`/kelola/{slug}`) hanya untuk pemilik yang sudah login;
  mengakses cashplan milik orang lain mengembalikan 404 (keberadaannya tidak dibocorkan).

> **Catatan keamanan (untuk ditindaklanjuti nanti):** slug bersifat pilihan pengguna
> sehingga bisa ditebak. Karena mode lihat memang publik-lewat-tautan, ini berisiko
> rendah untuk saat ini. Mengedit tetap aman karena dilindungi login + kepemilikan.
> Peningkatan berikutnya bisa berupa: opsi cashplan privat, rate-limiting login,
> dan token CSRF eksplisit (saat ini mengandalkan SameSite=Lax).

## Jaminan integritas (truthfulness)

Kejujuran data ditegakkan di level **database** oleh trigger `entries_guard`:

1. Entri **tidak bisa dihapus**, dan **jumlah (amount) serta jenis** (pemasukan/
   pengeluaran) **tidak bisa diubah** — terkunci agar angka tidak bisa dimanipulasi.
2. Hanya **pembayar/penerima, keterangan, dan tanggal** yang dapat diperbaiki. Setiap
   perubahan menyimpan nilai lama ke tabel append-only `entry_revisions`, sehingga
   **riwayat versi tetap utuh dan terlihat semua orang** (label "telah diperbarui" +
   timeline versi pada tiap catatan).
3. `entry_revisions` sendiri append-only (tidak bisa di-`UPDATE`/`DELETE`).

Semua aturan ini berlaku bahkan dari luar aplikasi (mis. via `psql`) — mencoba
mengubah amount/type atau menghapus entri akan ditolak database.

## Struktur berkas

```
main.go            Konfigurasi, koneksi DB, routing, middleware
handlers.go        HTTP handler + rendering template
auth.go            Hash kata sandi (bcrypt), sesi cookie, middleware user
store.go           Lapisan akses data (users, sessions, cashplans, entries)
slug.go            Normalisasi & validasi slug
money.go           Parsing & format Rupiah, format tanggal Indonesia
schema.sql         Skema DB (diterapkan otomatis saat start)
templates/         html/template (layout, partials, halaman)
static/            style.css, app.js
Dockerfile         Build biner (multi-stage, image kecil)
docker-compose.yml Postgres + aplikasi
```

## Model data

```
users(id, username, password_hash, created_at)
sessions(id, user_id, created_at, expires_at)
cashplans(id, owner_id, slug, title, description, created_at)
entries(id, cashplan_id, type['income'|'expense'], party, description,
        amount /* whole rupiah, immutable */, occurred_at, created_at,
        attachment_url, attachment_name)
entry_revisions(id, entry_id, party, description, occurred_at, revised_at)
```

`party` = pembayar (income) / penerima (expense). `description` = catatan (income)
/ keterangan-alasan (expense).

## Catatan

- Jumlah uang disimpan sebagai bilangan bulat rupiah (`bigint`) — tanpa desimal,
  sesuai kebiasaan IDR — sehingga tidak ada galat pembulatan floating-point.
- Tanggal default memakai zona waktu `Asia/Jakarta` (WIB).
- Reset total (hapus semua data): `docker compose down -v`.
