package controlplane

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRecoveredFunctionHealthKeepsDeploymentHistory(t *testing.T) {
	api := testAPI(t, nil)
	job, _ := submitTestJob(t, api, testSpec())
	stored, _ := api.store.get(job.ID)
	if err := api.store.update(job.ID, func(j *storedJob) {
		j.Job.Status = "failed"
		j.Job.Phase = "failed"
		j.Job.Error = "original readiness timeout"
		j.Job.Deployment = &deployment{UUID: testUUID, URL: "https://function.sfo.unikraft.app", State: "standby", MemoryMB: 128}
	}); err != nil {
		t.Fatal(err)
	}
	api.invokeTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://function.sfo.unikraft.app/healthz" || r.Method != "GET" {
			t.Error("health check invoked user function or wrong endpoint")
		}
		if r.Header.Get("Authorization") != "Bearer "+stored.Spec.Auth.Token {
			t.Error("health check lost function credential")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
	})
	path := "/v1/deployments/" + job.ID + "/health"
	if got := apiRequest(api, "POST", path, nil, "").Code; got != 401 {
		t.Fatal("anonymous health check accepted")
	}
	response := apiRequest(api, "POST", path, nil, adminToken)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"healthy":true`) {
		t.Fatalf("health check failed: %d %s", response.Code, response.Body)
	}
	recovered, _ := api.store.get(job.ID)
	if recovered.Job.Status != "failed" || recovered.Job.Error != "original readiness timeout" || recovered.Job.Health == nil || !recovered.Job.Health.Healthy || recovered.Job.Health.CheckedAt.IsZero() {
		t.Fatal("health result overwrote history or was not persisted")
	}
	// Inspection refreshes VM state while retaining the independent health result.
	if apiRequest(api, "GET", "/v1/deployments/"+job.ID, nil, adminToken).Code != 200 {
		t.Fatal("inspection failed")
	}
	recovered, _ = api.store.get(job.ID)
	if recovered.Job.Health == nil || !recovered.Job.Health.Healthy {
		t.Fatal("inspection discarded health")
	}
}

func TestHealthCheckRejectsRedirectsAndHidesFunctionOutput(t *testing.T) {
	api := testAPI(t, nil)
	job, _ := submitTestJob(t, api, testSpec())
	_ = api.store.update(job.ID, func(j *storedJob) {
		j.Job.Status = "failed"
		j.Job.Deployment = &deployment{UUID: testUUID, URL: "https://function.dal.unikraft.app"}
	})
	calls := 0
	api.invokeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.example"}}, Body: io.NopCloser(strings.NewReader("private application output"))}, nil
	})
	response := apiRequest(api, "POST", "/v1/deployments/"+job.ID+"/health", nil, adminToken)
	if calls != 1 || response.Code != 200 || !strings.Contains(response.Body.String(), `"healthy":false`) || strings.Contains(response.Body.String(), "private application output") {
		t.Fatal("redirect followed or response exposed")
	}
	_ = api.store.update(job.ID, func(j *storedJob) { j.Job.Deployment.URL = "https://127.0.0.1/private" })
	if apiRequest(api, "POST", "/v1/deployments/"+job.ID+"/health", nil, adminToken).Code != 502 || calls != 1 {
		t.Fatal("untrusted health target accepted")
	}
}
