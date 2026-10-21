package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// maxGitHubResponse bounds what is read of a reply from GitHub.
const maxGitHubResponse = 1 << 20

// exchange swaps the code GitHub gave the browser for an access token.
func (a *Auth) exchange(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", errors.New("github returned no code")
	}
	form := url.Values{
		"client_id":     {a.cfg.ClientID},
		"client_secret": {a.cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {a.cfg.CallbackURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	var reply struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if _, err := a.call(req, &reply); err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	if reply.Error != "" || reply.AccessToken == "" {
		// GitHub answers 200 with an error, such as an expired code.
		return "", fmt.Errorf("exchange code: github refused it: %q", reply.Error)
	}
	return reply.AccessToken, nil
}

// membership returns the login of the user the token belongs to, and whether
// they are an active member of the organisation.
func (a *Auth) membership(ctx context.Context, token string) (login string, member bool, err error) {
	var user struct {
		Login string `json:"login"`
	}
	if _, err := a.api(ctx, token, "/user", &user); err != nil {
		return "", false, fmt.Errorf("look up github user: %w", err)
	}

	var m struct {
		State string `json:"state"`
	}
	status, err := a.api(ctx, token, "/user/memberships/orgs/"+url.PathEscape(a.cfg.Org), &m)
	switch {
	case status == http.StatusNotFound || status == http.StatusForbidden:
		// 404 is not a member. 403 is GitHub refusing the app: a GitHub
		// App needs the organisation's Members permission, and to be
		// installed on it, to see anyone's membership.
		slog.InfoContext(ctx, "github refused the membership check", "login", user.Login, "org", a.cfg.Org, "status", status)
		return user.Login, false, nil
	case err != nil:
		return "", false, fmt.Errorf("check %s membership of %s: %w", user.Login, a.cfg.Org, err)
	case m.State != "active":
		// Invited, and the invitation not yet accepted.
		slog.InfoContext(ctx, "membership not active", "login", user.Login, "org", a.cfg.Org, "state", m.State)
	}
	return user.Login, m.State == "active", nil
}

// api GETs path from GitHub's API with the user's token into v, and returns
// the status.
func (a *Auth) api(ctx context.Context, token, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.APIURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return a.call(req, v)
}

// call sends req and decodes a 200's JSON body into v. Any other status is
// an error, returned with the status.
func (a *Auth) call(req *http.Request, v any) (int, error) {
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxGitHubResponse)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("github answered %s", resp.Status)
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("decode github reply: %w", err)
	}
	return resp.StatusCode, nil
}
