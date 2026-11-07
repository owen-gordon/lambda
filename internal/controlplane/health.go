package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type healthCheck struct {
	Healthy   bool      `json:"healthy"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

// A health check records an observation without rewriting the deployment job's
// history. It wakes the function only when explicitly requested by the operator.
func (c *controlPlane) checkHealth(w http.ResponseWriter, r *http.Request) {
	c.manageMu.Lock()
	defer c.manageMu.Unlock()
	job, ok := c.manageableJob(w, r)
	if !ok {
		return
	}
	if job.Job.Deployment == nil || job.Job.Deployment.URL == "" {
		apiError(w, 409, "job has no deployed endpoint")
		return
	}
	endpoint, err := functionEndpoint(job.Job.Deployment.URL)
	if err != nil {
		apiError(w, 502, "invalid function endpoint")
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoint.String(), "/")+"/healthz", nil)
	if err != nil {
		apiError(w, 502, "cannot check function health")
		return
	}
	if job.Spec.Auth.Token != "" {
		request.Header.Set("Authorization", "Bearer "+job.Spec.Auth.Token)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: c.invokeTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	check := &healthCheck{}
	if err == nil {
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		response.Body.Close()
		var result struct {
			Status string `json:"status"`
		}
		check.Healthy = readErr == nil && len(payload) <= 4096 && response.StatusCode == 200 && json.Unmarshal(payload, &result) == nil && result.Status == "ok"
	}
	check.CheckedAt = time.Now().UTC()
	if !check.Healthy {
		check.Error = "function did not return a healthy response"
	}
	if err := c.store.update(job.Job.ID, func(j *storedJob) { j.Job.Health = check }); err != nil {
		apiError(w, 500, "cannot persist health result")
		return
	}
	writeJSON(w, 200, check)
}
