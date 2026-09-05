package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// Nextcloud uploads files over WebDAV and returns a public share link. It is
// optional: NewNextcloudFromEnv returns nil when config is absent, and a nil
// *Nextcloud reports Enabled() == false, so callers can guard uploads.
type Nextcloud struct {
	baseURL string
	user    string
	pass    string
	folder  string
	client  *http.Client
}

func NewNextcloudFromEnv() *Nextcloud {
	// Primary names are NEXTCLOUD_*; NC_HOST / NC_APP_LOGIN / NC_APP_PASSWORD are
	// accepted as aliases.
	base := strings.TrimRight(firstEnv("NEXTCLOUD_URL", "NC_HOST"), "/")
	if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base // assume TLS when the host omits a scheme
	}
	user := firstEnv("NEXTCLOUD_USER", "NC_APP_LOGIN")
	pass := firstEnv("NEXTCLOUD_APP_PASSWORD", "NC_APP_PASSWORD")
	if base == "" || user == "" || pass == "" {
		return nil
	}
	folder := firstEnv("NEXTCLOUD_FOLDER", "NC_FOLDER")
	if folder == "" {
		folder = "cashflow"
	}
	return &Nextcloud{
		baseURL: base,
		user:    user,
		pass:    pass,
		folder:  folder,
		client:  &http.Client{Timeout: 45 * time.Second},
	}
}

// firstEnv returns the first non-empty value among the given environment keys.
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func (n *Nextcloud) Enabled() bool { return n != nil }

// Upload stores data under a unique name inside <folder>/<subfolder> and returns
// a public share URL. subfolder groups files (e.g. per cashplan by slug); an
// empty or unsafe subfolder falls back to the base folder.
func (n *Nextcloud) Upload(ctx context.Context, subfolder, filename string, data []byte, contentType string) (string, error) {
	dir := n.folder
	if sub := strings.Trim(subfolder, "/"); sub != "" && !strings.Contains(sub, "..") {
		dir += "/" + sub
	}
	remotePath := dir + "/" + newToken() + "-" + sanitizeFilename(filename)
	if err := n.mkcolAll(ctx, dir); err != nil {
		return "", err
	}
	if err := n.put(ctx, remotePath, data, contentType); err != nil {
		return "", err
	}
	return n.createShare(ctx, "/"+remotePath)
}

// davURL builds a WebDAV URL with each path segment escaped.
func (n *Nextcloud) davURL(remotePath string) string {
	segs := append([]string{"remote.php", "dav", "files", n.user}, strings.Split(remotePath, "/")...)
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return n.baseURL + "/" + strings.Join(segs, "/")
}

// mkcolAll creates each collection level of dirPath in order (WebDAV MKCOL only
// creates one level at a time). Idempotent.
func (n *Nextcloud) mkcolAll(ctx context.Context, dirPath string) error {
	segs := strings.Split(dirPath, "/")
	for i := range segs {
		if err := n.mkcol(ctx, strings.Join(segs[:i+1], "/")); err != nil {
			return err
		}
	}
	return nil
}

func (n *Nextcloud) mkcol(ctx context.Context, dirPath string) error {
	req, err := http.NewRequestWithContext(ctx, "MKCOL", n.davURL(dirPath), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(n.user, n.pass)
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	// 201 Created; 405 Method Not Allowed means the folder already exists.
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return fmt.Errorf("nextcloud mkcol %q: status %d", dirPath, resp.StatusCode)
}

func (n *Nextcloud) put(ctx context.Context, remotePath string, data []byte, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, n.davURL(remotePath), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth(n.user, n.pass)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("nextcloud upload: status %d", resp.StatusCode)
	}
	return nil
}

func (n *Nextcloud) createShare(ctx context.Context, remotePath string) (string, error) {
	endpoint := n.baseURL + "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json"
	form := url.Values{
		"path":        {remotePath},
		"shareType":   {"3"}, // public link
		"permissions": {"1"}, // read-only
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(n.user, n.pass)
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		OCS struct {
			Meta struct {
				StatusCode int    `json:"statuscode"`
				Message    string `json:"message"`
			} `json:"meta"`
			Data struct {
				URL string `json:"url"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("nextcloud share: unexpected response (status %d)", resp.StatusCode)
	}
	// OCS success is 100 (v1) or 200 (v2).
	if out.OCS.Meta.StatusCode != 200 && out.OCS.Meta.StatusCode != 100 {
		return "", fmt.Errorf("nextcloud share: %s (code %d)", out.OCS.Meta.Message, out.OCS.Meta.StatusCode)
	}
	if out.OCS.Data.URL == "" {
		return "", fmt.Errorf("nextcloud share: empty url in response")
	}
	return out.OCS.Data.URL, nil
}

// sanitizeFilename reduces a filename to a safe, readable base name.
func sanitizeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "berkas"
	}
	if len(out) > 80 { // keep the tail so the extension survives
		out = out[len(out)-80:]
	}
	return out
}
