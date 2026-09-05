package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNextcloudUpload(t *testing.T) {
	var putPath, sharePath, shareType, ocsHeader string
	var mkcolPaths []string
	var sawPut, sawShare bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "user" || pass != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "MKCOL":
			mkcolPaths = append(mkcolPaths, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut:
			sawPut = true
			putPath = r.URL.Path
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/shares"):
			sawShare = true
			ocsHeader = r.Header.Get("OCS-APIRequest")
			_ = r.ParseForm()
			sharePath = r.PostForm.Get("path")
			shareType = r.PostForm.Get("shareType")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ocs":{"meta":{"statuscode":200,"message":"OK"},"data":{"url":"https://nc.example/s/AbC123"}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	nc := &Nextcloud{baseURL: srv.URL, user: "user", pass: "pass", folder: "cashflow", client: srv.Client()}
	got, err := nc.Upload(context.Background(), "kas-3a", "My Receipt 2026.png", []byte("imagebytes"), "image/png")
	if err != nil {
		t.Fatalf("Upload error: %v", err)
	}

	if got != "https://nc.example/s/AbC123" {
		t.Errorf("share url = %q, want the OCS data.url", got)
	}
	if len(mkcolPaths) == 0 || !sawPut || !sawShare {
		t.Errorf("expected MKCOL+PUT+share POST; got mkcol=%d put=%v share=%v", len(mkcolPaths), sawPut, sawShare)
	}
	// Both the base folder and the per-cashplan subfolder must be created.
	var madeBase, madeSub bool
	for _, p := range mkcolPaths {
		if strings.HasSuffix(p, "/cashflow") {
			madeBase = true
		}
		if strings.HasSuffix(p, "/cashflow/kas-3a") {
			madeSub = true
		}
	}
	if !madeBase || !madeSub {
		t.Errorf("MKCOL should create both cashflow and cashflow/kas-3a; got %v", mkcolPaths)
	}
	if ocsHeader != "true" {
		t.Errorf("OCS-APIRequest header = %q, want \"true\"", ocsHeader)
	}
	if shareType != "3" {
		t.Errorf("shareType = %q, want 3 (public link)", shareType)
	}
	if !strings.Contains(putPath, "/remote.php/dav/files/user/cashflow/kas-3a/") {
		t.Errorf("PUT path = %q, want it under cashflow/kas-3a/", putPath)
	}
	if strings.ContainsAny(putPath, " ") || strings.Contains(putPath, "%20") {
		t.Errorf("PUT path %q must not contain spaces (filename must be sanitized)", putPath)
	}
	if !strings.HasSuffix(putPath, "-My-Receipt-2026.png") {
		t.Errorf("PUT path %q should end with the sanitized filename", putPath)
	}
	if !strings.HasPrefix(sharePath, "/cashflow/kas-3a/") || !strings.HasSuffix(sharePath, ".png") {
		t.Errorf("share path = %q, want /cashflow/kas-3a/<name>.png", sharePath)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"My Receipt.png":      "My-Receipt.png",
		"../../etc/passwd":    "passwd",
		"foto bukti (1).jpeg": "foto-bukti-1.jpeg",
		"":                    "berkas",
		"...":                 "berkas",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNextcloudDisabled(t *testing.T) {
	var nc *Nextcloud // as returned by NewNextcloudFromEnv when unconfigured
	if nc.Enabled() {
		t.Error("nil *Nextcloud should report Enabled() == false")
	}
}
