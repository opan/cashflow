package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseAmount parses an Indonesian-style rupiah input into whole rupiah.
// Accepts "1.000.000", "1000000", "Rp 1.000.000", "50 000". IDR has no
// sub-unit in everyday use, so fractional input is rejected.
func parseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.ToLower(s), "rp")
	// Thousand separators and spaces are noise.
	s = strings.NewReplacer(".", "", " ", "", ",", "").Replace(s)
	if s == "" {
		return 0, errors.New("jumlah tidak boleh kosong")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("gunakan angka saja")
		}
		n = n*10 + int64(r-'0')
		if n < 0 { // overflow
			return 0, errors.New("jumlah terlalu besar")
		}
	}
	if n <= 0 {
		return 0, errors.New("jumlah harus lebih dari 0")
	}
	return n, nil
}

// formatRupiah renders whole rupiah as "Rp 1.000.000".
func formatRupiah(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	pre := len(digits) % 3
	if pre > 0 {
		b.WriteString(digits[:pre])
	}
	for i := pre; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(digits[i : i+3])
	}
	out := "Rp " + b.String()
	if neg {
		out = "-" + out
	}
	return out
}

var bulanID = [...]string{"", "Jan", "Feb", "Mar", "Apr", "Mei", "Jun",
	"Jul", "Agu", "Sep", "Okt", "Nov", "Des"}

// formatTanggal renders a date the Indonesian way, e.g. "4 Sep 2026".
func formatTanggal(t time.Time) string {
	return fmt.Sprintf("%d %s %d", t.Day(), bulanID[int(t.Month())], t.Year())
}

var bulanFullID = [...]string{"", "Januari", "Februari", "Maret", "April", "Mei",
	"Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}

// formatBulanTahun renders a month + year, e.g. "September 2025".
func formatBulanTahun(t time.Time) string {
	return fmt.Sprintf("%s %d", bulanFullID[int(t.Month())], t.Year())
}
