# Cashflow

Aplikasi web sederhana untuk melacak arus kas (cash flow) bersama, transparan, dan
tidak bisa dimanipulasi. Daftar akun, buat sebuah **cashplan** (mis. "Uang kas
sekolah anak") dengan tautan pilihan Anda, catat pemasukan & pengeluaran, lalu
bagikan tautannya. Riwayat bersifat **append-only** — tidak bisa diubah atau
dihapus, sehingga semua orang melihat data yang sama.

> Antarmuka aplikasi berbahasa Indonesia. Dokumen ini tersedia dalam dua bahasa:
> **Bahasa Indonesia** · [English](README.en.md)

## Fitur

- **Akun pengguna** (nama pengguna + kata sandi). Nama pengguna bersifat unik,
  siapa cepat dia dapat. Cashplan tersimpan di akun Anda, jadi
  tautan kelolanya tidak akan hilang — cukup masuk untuk mengaksesnya kembali.
- **Masuk lewat All-in-one** (opsional): satu akun untuk cashflow dan aplikasi lain
  yang memakai All-in-one, dengan single sign-on. Lihat
  [Masuk lewat All-in-one](#masuk-lewat-all-in-one-opsional).
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
  Opsional: login lewat All-in-one dengan OpenID Connect (`coreos/go-oidc` + `x/oauth2`).
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

Variabel lain (unggah lampiran ke Nextcloud, rate limiting, metrik OpenTelemetry) ada di
[`.env.example`](.env.example). Variabel untuk login lewat All-in-one dijelaskan di
[bagian berikut](#konfigurasi-login-all-in-one).

## Rute (routes)

| Method + Path                | Akses                    | Fungsi                                    |
| ---------------------------- | ------------------------ | ----------------------------------------- |
| `GET  /`                     | publik                   | Landing (belum login) / Dasbor (login)    |
| `GET/POST /register`         | publik                   | Daftar akun (dengan `AUTH_PROVIDER=aio`: dialihkan ke alur All-in-one) |
| `GET/POST /login`            | publik                   | Masuk (dengan `AUTH_PROVIDER=aio`: dialihkan ke alur All-in-one) |
| `GET  /auth/login`           | publik                   | Mulai login di All-in-one (`?next=`, `?signup=1`) |
| `GET  /auth/callback`        | publik                   | Kembali dari All-in-one: tukar kode, buat sesi |
| `POST /logout`               | login                    | Keluar (dengan `AUTH_PROVIDER=aio`: juga keluar dari All-in-one) |
| `POST /cashplans`            | login                    | Buat cashplan (title, slug, description)  |
| `GET  /kelola/{slug}`        | pemilik                  | Kelola: tambah transaksi, ringkasan, bagikan |
| `POST /kelola/{slug}/entry`  | pemilik                  | Tambah transaksi (income/expense)         |
| `GET  /p/{slug}`             | siapa pun dengan tautan  | Lihat (baca-saja) ringkasan & riwayat     |

## Masuk lewat All-in-one (opsional)

Cashflow bisa memakai **All-in-one (aio)** sebagai layanan login lewat OpenID Connect. Fitur
ini mati secara default (`AUTH_PROVIDER=local`): daftar dan masuk tetap memakai nama
pengguna + kata sandi milik cashflow, persis seperti sebelumnya.

Dengan `AUTH_PROVIDER=aio`:

- **Masuk** dan **Daftar** membawa pengguna ke halaman login aio. Halaman itu tampil sebagai
  Cashflow: ikon 💰, warna teal, bahasa Indonesia, dengan catatan kecil "Login diamankan oleh
  All-in-one". Cashflow tidak pernah melihat kata sandi pengguna.
- Pengguna yang sudah login di aio langsung masuk tanpa formulir (*single sign-on*).
- **Keluar** dari cashflow juga mengakhiri sesi aio.
- Formulir kata sandi lokal tidak dipakai lagi: `POST /login` dan `POST /register` dialihkan
  ke alur aio.

> ⚠️ **Sudah punya pengguna? Pindahkan dulu akun mereka ke aio** sebelum mengaktifkan
> `AUTH_PROVIDER=aio` (lihat [Memindahkan pengguna lama](#memindahkan-pengguna-lama-ke-all-in-one)).
> Tanpa itu, kata sandi lama tidak berlaku lagi dan masuk lewat aio membuat akun baru yang kosong.

### Alur masuk

```mermaid
sequenceDiagram
    autonumber
    participant B as Browser
    participant C as Cashflow
    participant A as All-in-one
    B->>C: GET /login (klik "Masuk")
    C->>B: Alihkan ke /auth/login, lalu ke aio /authorize
    Note over C: Simpan state, nonce, dan verifier PKCE<br/>di cookie cashflow_oidc (10 menit)
    B->>A: GET /api/v1/oauth2/authorize (client_id, PKCE, ui_locales=id)
    A->>B: Alihkan ke halaman serah-terima /oauth/login
    alt Belum login di aio
        B->>A: Formulir masuk/daftar aio (tampil sebagai Cashflow)
    end
    B->>A: Selesaikan permintaan masuk (hanya dari browser yang memulainya)
    A->>B: Alihkan ke cashflow /auth/callback?code=...&state=...
    B->>C: GET /auth/callback
    C->>A: POST /api/v1/oauth2/token (kode + client secret + verifier PKCE)
    A->>C: ID token
    Note over C: Verifikasi tanda tangan, issuer, audience,<br/>kedaluwarsa, dan nonce. Cari atau buat user lokal.
    C->>B: Alihkan ke halaman tujuan + cookie sesi cashflow
```

1. **Cashflow memulai** (`/auth/login`): membuat `state`, `nonce`, dan verifier PKCE, menyimpannya
   di cookie `cashflow_oidc` (HttpOnly, 10 menit), lalu mengalihkan ke aio dengan
   `ui_locales=id` (dan `prompt=create` untuk **Daftar**).
2. **aio memeriksa permintaan** (client terdaftar, `redirect_uri` cocok, PKCE S256), mengikatnya ke
   browser ini, dan menampilkan halaman serah-terima. Tanpa sesi aio, pengguna masuk atau
   mendaftar di aio; dengan sesi aio, langkah ini dilewati.
3. **aio mengembalikan kode sekali pakai** ke `/auth/callback`.
4. **Cashflow menyelesaikan** (`/auth/callback`): mencocokkan `state` dengan cookie, menukar kode
   dengan ID token **dari server ke server** (client secret + verifier PKCE), memverifikasi ID token
   dengan kunci publik aio, lalu membuat sesi cashflow sendiri. Setelah itu cashflow tidak
   memanggil aio lagi sampai pengguna keluar.

### Alur keluar

1. `POST /logout`: cashflow menghapus sesinya, lalu mengalihkan ke `end_session` aio dengan ID
   token yang disimpan saat masuk (`id_token_hint`) dan `post_logout_redirect_uri`.
2. aio mengakhiri sesinya (hanya jika ID token itu milik pengguna sesi aio di browser tersebut),
   lalu mengalihkan kembali ke cashflow.

Jika aio sedang tidak bisa dihubungi saat keluar, cashflow tetap mengeluarkan pengguna dari
cashflow saja.

### Akun pengguna

- User cashflow ditautkan ke akun aio lewat `sub` di ID token (`users.aio_user_id`) dan dibuat
  otomatis saat pertama kali masuk, tanpa kata sandi lokal.
- Nama pengguna diambil dari `preferred_username` aio, disesuaikan dengan aturan cashflow (huruf
  kecil, angka, `_`, 3–30 karakter).
  - Sudah dipakai akun aio lain (mis. akun yang berganti nama): ditambah akhiran `_xxxxxxxx`.
  - Sudah dipakai **akun cashflow lama** (lokal): masuk ditolak. Akun lama tidak pernah diambil
    alih otomatis; pindahkan dulu dengan `users:import` (di bawah).

### Pemasangan

1. **Di aio**: aktifkan provider (`auth.oidc.enabled: true` dan `auth.oidc.issuer`, URL publik aio).
   Jalankan aio dengan **satu replika** (strategi `Recreate`): permintaan login disimpan di memori.
2. **Daftarkan cashflow di aio** beserta brandingnya:

   ```bash
   all-in-one oidc:client:create --id cashflow --name Cashflow \
     --redirect-uri https://cashflow.example.com/auth/callback \
     --post-logout-redirect-uri https://cashflow.example.com/ \
     --brand-color '#0f766e' --icon 💰
   ```

   Simpan *client secret* yang tercetak; hanya ditampilkan sekali. Branding (nama, warna, ikon)
   bisa diubah kapan saja dengan `all-in-one oidc:client:update cashflow --brand-color ... --icon ...`.
3. **Di cashflow**: isi variabel di bawah, lalu jalankan ulang.

Untuk pengembangan lokal, jalankan aio dan cashflow di host berbeda, mis. aio di
`http://127.0.0.1:18080` dan cashflow di `http://localhost:8090` (aio menerima redirect URI
`http` hanya untuk alamat loopback), dengan `AIO_REDIRECT_URL=http://localhost:8090/auth/callback`.

### Memindahkan pengguna lama ke All-in-one

Perintah `all-in-one users:import` menyalin akun lokal cashflow (nama pengguna + hash kata sandi
bcrypt apa adanya) ke aio, lalu menautkannya di cashflow (`users.aio_user_id`). Hasilnya:

- **Kata sandi tetap sama.** Pengguna masuk di halaman aio dengan nama pengguna dan kata sandi
  yang sudah mereka pakai. Tidak ada yang perlu mendaftar ulang atau mengganti kata sandi.
- **Data tidak berpindah.** Cashplan, transaksi, dan iuran tetap di database cashflow; baris
  `users` tidak berubah id-nya, hanya ditambah tautan ke akun aio.
- **Sesi yang berjalan tetap berlaku** sampai kedaluwarsa (30 hari); setelah itu pengguna masuk
  lewat aio.

Perintah ini membaca database cashflow langsung (cashflow dan aio biasanya di server Postgres
yang sama) dari variabel lingkungan `CASHFLOW_DATABASE_URL`, bukan dari flag, agar kata sandi
database tidak muncul di daftar proses. Tanpa `--apply` perintah ini hanya menampilkan rencana.

1. **Deploy versi cashflow dengan login All-in-one, tetap `AUTH_PROVIDER=local`.** Saat start,
   cashflow menambah kolom `aio_user_id`; pengguna tidak merasakan perubahan apa pun.
2. **Daftarkan cashflow di aio** (langkah 2 di [Pemasangan](#pemasangan)) dan siapkan variabel
   `AIO_*`, tetapi jangan ubah `AUTH_PROVIDER` dulu.
3. **Uji coba (dry run)** dengan konfigurasi aio (lihat contoh Job Kubernetes di bawah):

   ```bash
   export CASHFLOW_DATABASE_URL='postgres://cashflow:...@<server-pg>:5432/cashflow?sslmode=disable'
   all-in-one users:import
   ```

   ```
   USERNAME  RESULT           AIO ACCOUNT                           NOTE
   budi      created          59d879b8-8c96-43dc-91c9-9a14dcef7fbe
   siti      skipped          -                                     aio already has this username for another account: ...
   admin     skipped          -                                     reserved in aio (aio's bootstrap admin, ...): rename it in cashflow first
   opan      skipped          -                                     aio already has this username for another account: ...
   ```

4. **Selesaikan akun yang di-*skip*.** `--apply` tidak menulis apa pun selama masih ada yang di-skip.

   | Penyebab | Tindakan |
   | --- | --- |
   | Nama pengguna sudah ada di aio, **orang yang sama** (mis. akun Anda sendiri) | Tambahkan `--link-existing <nama>`. Akun cashflow ditautkan ke akun aio itu; ia masuk dengan **kata sandi aio-nya**. |
   | Nama pengguna sudah ada di aio, **orang lain** | Ganti nama di cashflow: `UPDATE users SET username = 'siti_kas' WHERE username = 'siti';` lalu beri tahu penggunanya nama barunya. |
   | Nama admin awal aio (`rbac.admin_username`, mis. `admin`) | Ganti nama di cashflow (aio memberi grup admin ke nama ini jika belum ada admin), atau `--link-existing` jika itu memang akun admin Anda di aio. |
   | Akun demo bersama aio (`demo_mode.username`) | Ganti nama di cashflow; akun demo tidak bisa dipakai untuk masuk ke aplikasi lain. |
   | Nama pengguna atau hash tidak valid | Perbaiki datanya di cashflow. |

5. **Jalankan sungguhan:** `all-in-one users:import --apply` (ditambah `--link-existing ...` bila
   perlu). Akun aio dibuat dalam satu transaksi, lalu tautannya ditulis di cashflow dalam satu
   transaksi. Menjalankan ulang aman: akun yang sudah dipindah dikenali dan hanya ditautkan.

   ```
   Imported into aio: 3 new, 0 already there, 1 linked to existing accounts. Linked in cashflow: 4.
   Every cashflow account is linked: cashflow can switch to AUTH_PROVIDER=aio.
   ```

6. **Ubah ke `AUTH_PROVIDER=aio`** dan jalankan ulang cashflow.

**Kembali ke login lokal** (jika perlu): set `AUTH_PROVIDER=local` lagi. Hash kata sandi lokal
masih disimpan cashflow, jadi semua orang bisa masuk seperti sebelumnya (perubahan kata sandi yang
dibuat di aio setelah peralihan tidak ikut). Akun di aio tidak mengganggu dan bisa dibiarkan.

<details>
<summary>Contoh Job Kubernetes (aio memakai image distroless, jadi tidak bisa lewat <code>kubectl exec</code>)</summary>

Job ini memakai image, konfigurasi, dan secret aio yang sama dengan deployment-nya, dan mengambil
`CASHFLOW_DATABASE_URL` dari secret cashflow. Jalankan dulu tanpa `--apply`, baca lognya
(`kubectl -n app logs job/all-in-one-users-import`), hapus Job-nya, lalu jalankan lagi dengan
`--apply`.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: all-in-one-users-import
  namespace: app
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: users-import
          image: opanmustopah/all-in-one:latest
          args: ["users:import"]   # then: ["users:import", "--apply", "--link-existing", "opan"]
          env:
            - name: CASHFLOW_DATABASE_URL
              valueFrom: { secretKeyRef: { name: cashflow-secrets, key: DATABASE_URL } }
            - name: ALLINONE_AUTH_JWT_SECRET
              valueFrom: { secretKeyRef: { name: all-in-one-secrets, key: jwt-secret } }
            - name: ALLINONE_AUTH_TOTP_ENCRYPTION_KEY
              valueFrom: { secretKeyRef: { name: all-in-one-secrets, key: totp_encryption_secret } }
            - { name: ALLINONE_STORAGE_TYPE, value: "postgres" }
            - { name: ALLINONE_STORAGE_POSTGRES_HOST, value: "192.168.68.124" }
            - { name: ALLINONE_STORAGE_POSTGRES_PORT, value: "5432" }
            - { name: ALLINONE_STORAGE_POSTGRES_USER, value: "allinone" }
            - { name: ALLINONE_STORAGE_POSTGRES_DBNAME, value: "allinone" }
            - { name: ALLINONE_STORAGE_POSTGRES_SSLMODE, value: "disable" }
            - name: ALLINONE_STORAGE_POSTGRES_PASSWORD
              valueFrom: { secretKeyRef: { name: all-in-one-secrets, key: postgres_password } }
          volumeMounts:
            - { name: config, mountPath: /app/config, readOnly: true }
      volumes:
        - name: config
          configMap: { name: all-in-one-config }
```

</details>

### Konfigurasi login All-in-one

| Variabel              | Contoh                                       | Keterangan |
| --------------------- | -------------------------------------------- | ---------- |
| `AUTH_PROVIDER`       | `aio`                                        | `local` (default) atau `aio` |
| `AIO_ISSUER`          | `https://auth.example.com`                   | URL publik aio (issuer OIDC) |
| `AIO_CLIENT_ID`       | `cashflow`                                   | `--id` saat didaftarkan |
| `AIO_CLIENT_SECRET`   | (dari `oidc:client:create`)                  | Rahasia; simpan di secret, bukan di repo |
| `AIO_REDIRECT_URL`    | `https://cashflow.example.com/auth/callback` | Harus sama persis dengan `--redirect-uri` |
| `AIO_POST_LOGOUT_URL` | `https://cashflow.example.com/`              | Opsional; tujuan setelah keluar dari aio (harus terdaftar) |

Dengan `AUTH_PROVIDER=aio`, konfigurasi yang kurang lengkap membuat cashflow gagal start (bukan
diam-diam kembali ke login lokal).

### Saat aio bermasalah

- **Gagal tertutup**: jika aio tidak bisa dihubungi, pengguna baru tidak bisa masuk. Pengguna yang
  sudah masuk tetap bisa bekerja, karena setiap permintaan memakai sesi cashflow sendiri (30 hari).
- Cashflow tetap bisa start walaupun aio mati: konfigurasi aio (*discovery*) diambil saat login
  pertama dan dicoba lagi pada login berikutnya.
- Header CSP `form-action` menyertakan origin aio, karena keluar adalah POST yang dialihkan ke aio.

### Pesan galat

| Pesan di cashflow | Penyebab | Yang perlu dilakukan |
| --- | --- | --- |
| Layanan masuk sedang tidak tersedia | aio tidak bisa dihubungi | Periksa aio dan `AIO_ISSUER` |
| Sesi masuk tidak valid atau sudah kedaluwarsa | Cookie `cashflow_oidc` hilang, lewat 10 menit, atau login dimulai lagi di tab lain | Masuk lagi dari awal |
| Proses masuk dibatalkan atau ditolak | aio mengirim galat kembali ke cashflow (mis. tautan callback dibuka di browser yang tidak menyelesaikan login) | Masuk lagi dari cashflow di browser yang sama |
| Gagal menyelesaikan proses masuk | Penukaran kode gagal (mis. `AIO_CLIENT_SECRET` salah, atau kode sudah kedaluwarsa) | Periksa konfigurasi, cek log cashflow |
| Gagal memverifikasi identitas | ID token ditolak (issuer, audience, tanda tangan, atau nonce) | Pastikan `AIO_ISSUER` dan `AIO_CLIENT_ID` benar |
| Nama pengguna ini sudah dipakai akun cashflow lama | Nama pengguna aio sama dengan akun lokal cashflow yang belum ditautkan | Pindahkan akun lama dengan `users:import` (pakai `--link-existing` jika orangnya sama) |
| (di halaman aio) galat `redirect_uri` atau *unknown client* | `AIO_REDIRECT_URL` atau `AIO_CLIENT_ID` tidak cocok dengan pendaftaran di aio | Samakan dengan `--id` dan `--redirect-uri`; cek `all-in-one oidc:client:list` |

Galat yang terjadi di halaman aio (mis. akun demo bersama ditolak, atau tautan login dibuka di
browser lain) ditampilkan oleh aio sendiri, dalam bahasa Indonesia, dengan tombol **Kembali**.

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
aioauth.go         Login lewat All-in-one (OpenID Connect): /auth/login, /auth/callback, logout
store.go           Lapisan akses data (users, sessions, cashplans, entries)
slug.go            Normalisasi & validasi slug
money.go           Parsing & format Rupiah, format tanggal Indonesia
schema.sql         Skema DB (diterapkan otomatis saat start)
templates/         html/template (layout, partials, halaman; autherror.html = galat login aio)
static/            style.css, app.js
Dockerfile         Build biner (multi-stage, image kecil)
docker-compose.yml Postgres + aplikasi
```

## Model data

```
users(id, username, password_hash /* NULL untuk akun All-in-one */, aio_user_id, created_at)
sessions(id, user_id, created_at, expires_at, id_token /* untuk logout di aio */)
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
