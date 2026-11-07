package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestServiceRequiresCloudConfiguration(t *testing.T) {
	t.Setenv("UKC_TOKEN", "cloud-token")
	t.Setenv("UKC_METRO", "dal")
	t.Setenv("LAMBDA_API_TOKEN", adminToken)
	t.Setenv("LAMBDA_STATE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("LAMBDA_MAX_JOBS", "")
	t.Setenv("LAMBDA_MAX_PENDING_JOBS", "")
	cfg, err := loadServiceConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.StateDir != "/data/jobs" || cfg.MaxJobs != 64 || cfg.MaxPending != 8 {
		t.Fatalf("unexpected service configuration: %+v", struct {
			Listen, StateDir    string
			MaxJobs, MaxPending int
		}{cfg.Listen, cfg.StateDir, cfg.MaxJobs, cfg.MaxPending})
	}
	t.Setenv("UKC_METRO", "")
	if _, err := loadServiceConfig(); err == nil {
		t.Fatal("silently selected a metro")
	}
}

func TestPersistedJobSchemaSurvivesServiceRefactor(t *testing.T) {
	// This is the shape written by the original hosted service. Go package
	// names and internal type visibility must not alter the wire format.
	data := `{"job":{"id":"old-job","name":"hello-old","status":"succeeded","metro":"dal","deployment":{"uuid":"old-instance","image":"unikraft.io/test/image@sha256:1234","url":"https://hello.example","ready":true,"protected":true}},"spec":{"source":"def handler(event): return event","image":"test/image","entry":"handler","memory_mb":128,"env":{"GREETING":"Hello"},"auth":{"mode":"bearer","token":"runtime-secret"}}}`
	var stored storedJob
	if err := json.Unmarshal([]byte(data), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Job.Deployment == nil || stored.Job.Deployment.UUID != "old-instance" || stored.Spec.Source == "" || stored.Spec.Auth.Token != "runtime-secret" {
		t.Fatal("lost persisted fields")
	}
	cloned := cloneJob(stored)
	if cloned.Job.Deployment == nil || cloned.Job.Deployment.URL != "https://hello.example" {
		t.Fatal("job clone dropped deployment metadata")
	}
	public, err := json.Marshal(cloned.Job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(public), `"deployment"`) || strings.Contains(string(public), "runtime-secret") || strings.Contains(string(public), "GREETING") {
		t.Fatalf("public job contract changed: %s", public)
	}
}
