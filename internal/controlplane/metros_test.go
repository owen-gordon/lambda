package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestSelectedMetroPersistsAndRoutesDeploymentAndManagement(t *testing.T) {
	api := testAPI(t, nil)
	api.listMetros = func(context.Context) ([]metro, error) {
		return []metro{{Code: "dal"}, {Code: "fra", Country: "de"}}, nil
	}
	response := apiRequest(api, "GET", "/v1/metros", nil, adminToken)
	var catalog struct {
		Default string  `json:"default"`
		Metros  []metro `json:"metros"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil || response.Code != 200 || catalog.Default != "dal" || len(catalog.Metros) != 2 {
		t.Fatal("invalid metro catalog")
	}
	if apiRequest(api, "GET", "/v1/metros", nil, "").Code != 401 {
		t.Fatal("anonymous metro discovery accepted")
	}
	for _, invalid := range []string{"zzz", "FRA", "https://evil.example", "../fra"} {
		spec := testSpec()
		spec.Metro = invalid
		if got := apiRequest(api, "POST", "/v1/deployments", spec, adminToken).Code; got != 400 {
			t.Fatalf("metro %q returned %d", invalid, got)
		}
	}
	spec := testSpec()
	spec.Metro = "fra"
	job, _ := submitTestJob(t, api, spec)
	if job.Metro != "fra" {
		t.Fatal("selected metro lost in receipt")
	}
	stored, _ := api.store.get(job.ID)
	if stored.Spec.Metro != "fra" {
		t.Fatal("selected metro not persisted")
	}
	// Older jobs carry the target only in job metadata. Execution must use that
	// value even if the service's home/default metro changes after a restart.
	if err := api.store.update(job.ID, func(j *storedJob) { j.Spec.Metro = "" }); err != nil {
		t.Fatal(err)
	}
	resumed, err := newControlPlane(api.store, adminToken, "dal", func(_ context.Context, s buildSpec, _ string, _ io.Writer) (*deployment, error) {
		if s.Metro != "fra" {
			t.Errorf("worker selected %s", s.Metro)
		}
		return &deployment{UUID: testUUID, Metro: s.Metro}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ = api.store.get(job.ID)
	if err := resumed.execute(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{}
	resumed.managerForMetro = func(metro string) instanceManager {
		if metro != "fra" {
			t.Errorf("management used metro %s", metro)
		}
		return manager
	}
	if got := apiRequest(resumed, "GET", "/v1/deployments/"+job.ID, nil, adminToken).Code; got != 200 {
		t.Fatalf("inspect returned %d", got)
	}
	if got := apiRequest(resumed, "DELETE", "/v1/deployments/"+job.ID, nil, adminToken).Code; got != 204 {
		t.Fatalf("delete returned %d", got)
	}
	if manager.inspected != testUUID || manager.deleted != testUUID {
		t.Fatal("management did not use saved instance")
	}
	defaultJob, _ := submitTestJob(t, api, testSpec())
	if defaultJob.Metro != "dal" {
		t.Fatal("omitted metro did not default to service metro")
	}
}

func TestTargetMetroKeepsHomeBuilder(t *testing.T) {
	home, target := &fakeCloud{}, &fakeCloud{}
	var built bool
	p := pipeline{Cloud: home, Metro: "dal", ForMetro: func(metro string) instanceClient {
		if metro != "fra" {
			t.Errorf("wrong metro %s", metro)
		}
		return target
	}, AfterBuild: func(context.Context) error { built = true; return nil }}
	selected := p.inMetro("fra")
	if selected.Cloud != target || selected.Metro != "fra" || p.Cloud != home || p.Metro != "dal" {
		t.Fatal("metro routing modified home pipeline")
	}
	if err := selected.AfterBuild(context.Background()); err != nil || !built {
		t.Fatal("home builder callback lost")
	}
}
