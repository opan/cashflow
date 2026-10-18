package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const aioStatusTimeout = 2 * time.Second

// aioUserStatus is aio's answer to "is this user an admin right now?"
// (RFC-002 in all-in-one). A blocked user is never an admin.
type aioUserStatus struct {
	Admin   bool `json:"admin"`
	Blocked bool `json:"blocked"`
}

// userStatus asks aio about one user, identified by the ID token's sub. It is
// called on every admin request and never cached, so a user removed from
// aio's admin group loses access on their next request. A user aio doesn't
// know is simply not an admin; anything else unexpected is an error, which
// callers treat as "not admin".
func (a *AioAuth) userStatus(ctx context.Context, sub string) (aioUserStatus, error) {
	// aio user ids are UUIDs. Anything else can't be an aio admin, and only a
	// parsed id ever reaches the URL, so the request can't be steered to
	// another host or path. The host is operator config (AIO_INTERNAL_URL or
	// AIO_ISSUER).
	id, err := uuid.Parse(sub)
	if err != nil {
		return aioUserStatus{}, nil
	}
	endpoint := a.internalURL + "/api/v1/oidc/users/" + id.String() + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil) // #nosec G704 -- host from config, path segment is a parsed UUID
	if err != nil {
		return aioUserStatus{}, err
	}
	req.SetBasicAuth(a.clientID, a.clientSecret)
	resp, err := a.httpClient.Do(req) // #nosec G704 -- see endpoint above
	if err != nil {
		return aioUserStatus{}, fmt.Errorf("aio status: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return aioUserStatus{}, nil
	default:
		return aioUserStatus{}, fmt.Errorf("aio status: unexpected HTTP %d", resp.StatusCode)
	}
	var body struct {
		Success bool          `json:"success"`
		Data    aioUserStatus `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !body.Success {
		return aioUserStatus{}, fmt.Errorf("aio status: invalid response (decode error: %v)", err)
	}
	if body.Data.Blocked {
		body.Data.Admin = false
	}
	return body.Data, nil
}

// adminHint is the user's admin status at login, for showing the admin link.
// Failing to get it only hides the link.
func (a *AioAuth) adminHint(ctx context.Context, sub string) bool {
	st, err := a.userStatus(ctx, sub)
	if err != nil {
		log.Printf("aio login: admin hint: %v", err)
		return false
	}
	return st.Admin
}
