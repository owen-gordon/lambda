package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func dashboardOrigin(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("LAMBDA_DASHBOARD_ORIGIN must be an HTTPS origin")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// browserAccess handles only the configured console origin. Preflight grants
// browser permission; every actual API request still requires its bearer token.
func (c *controlPlane) browserAccess(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	w.Header().Add("Vary", "Origin")
	if origin == "" {
		return true
	}
	if c.dashboardOrigin == "" || origin != c.dashboardOrigin {
		apiError(w, 403, "origin is not allowed")
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	if r.Method != http.MethodOptions {
		return true
	}
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	method := r.Header.Get("Access-Control-Request-Method")
	if method != "GET" && method != "POST" && method != "DELETE" {
		apiError(w, 403, "method is not allowed")
		return false
	}
	if r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/v1/") {
		apiError(w, 404, "not found")
		return false
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		header = strings.ToLower(strings.TrimSpace(header))
		if header != "" && header != "authorization" && header != "content-type" {
			apiError(w, 403, "header is not allowed")
			return false
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "300")
	w.WriteHeader(http.StatusNoContent)
	return false
}

func functionEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".unikraft.app") || (u.Port() != "" && u.Port() != "443") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid function endpoint")
	}
	return u, nil
}

// invoke uses the job's stored invocation token, keeping it out of browser
// requests. The admin token authorizes invoking only a recorded deployment.
func (c *controlPlane) invoke(w http.ResponseWriter, r *http.Request) {
	job, ok := c.manageableJob(w, r)
	if !ok {
		return
	}
	if job.Job.Deployment == nil || job.Job.Deployment.URL == "" {
		apiError(w, 409, "job has no deployed endpoint")
		return
	}
	u, err := functionEndpoint(job.Job.Deployment.URL)
	if err != nil {
		apiError(w, 502, "invalid function endpoint")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			apiError(w, 413, "request exceeds 1 MiB")
		} else {
			apiError(w, 400, "cannot read request")
		}
		return
	}
	if !json.Valid(body) {
		apiError(w, 400, "request must contain JSON")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if c.beforeInvoke != nil {
		if err := c.beforeInvoke(ctx); err != nil {
			apiError(w, 503, "function capacity is busy; retry shortly")
			return
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(u.String(), "/")+"/", strings.NewReader(string(body)))
	if err != nil {
		apiError(w, 502, "cannot invoke function")
		return
	}
	request.Header.Set("Content-Type", "application/json")
	if job.Spec.Auth.Token != "" {
		request.Header.Set("Authorization", "Bearer "+job.Spec.Auth.Token)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c.invokeTransport != nil {
		client.Transport = c.invokeTransport
	}
	response, err := client.Do(request)
	if err != nil {
		apiError(w, 502, "function request failed")
		return
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(payload) > 2<<20 {
		apiError(w, 502, "function response could not be read or exceeds 2 MiB")
		return
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		apiError(w, 502, "function redirects are unsupported")
		return
	}
	contentType := "text/plain; charset=utf-8"
	if json.Valid(payload) {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}
