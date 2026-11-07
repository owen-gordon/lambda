package controlplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardCORSDoesNotBypassAuthentication(t *testing.T) {
	api := testAPI(t, nil)
	api.dashboardOrigin = "https://dashboard.dal.unikraft.app"
	request := httptest.NewRequest("OPTIONS", "/v1/deployments", nil)
	request.Header.Set("Origin", api.dashboardOrigin)
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != 204 || response.Header().Get("Access-Control-Allow-Origin") != api.dashboardOrigin {
		t.Fatalf("preflight failed: %d", response.Code)
	}
	for _, origin := range []string{api.dashboardOrigin, "https://evil.example", api.dashboardOrigin + ".evil.example"} {
		r := httptest.NewRequest("GET", "/v1/jobs", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, r)
		if origin == api.dashboardOrigin {
			if w.Code != 401 {
				t.Fatal("CORS bypassed bearer auth")
			}
		} else if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("untrusted origin allowed")
		}
	}
	request.Header.Set("Access-Control-Request-Headers", "cookie")
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("unsupported preflight header accepted")
	}
}

func TestConsoleInvokeUsesStoredFunctionCredential(t *testing.T) {
	api := testAPI(t, nil)
	job, _ := submitTestJob(t, api, testSpec())
	stored, _ := api.store.get(job.ID)
	if err := api.store.update(job.ID, func(j *storedJob) {
		j.Job.Status = "succeeded"
		j.Job.Deployment = &deployment{UUID: testUUID, URL: "https://function.dal.unikraft.app"}
	}); err != nil {
		t.Fatal(err)
	}
	api.invokeTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+stored.Spec.Auth.Token || r.Header.Get("Authorization") == "Bearer "+adminToken {
			t.Error("wrong invocation credential")
		}
		if r.URL.String() != "https://function.dal.unikraft.app/" || r.Method != "POST" {
			t.Error("wrong target")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"name":"Owen"}` {
			t.Error("body changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"message":"Hello"}`))}, nil
	})
	response := apiRequest(api, "POST", "/v1/deployments/"+job.ID+"/invoke", map[string]string{"name": "Owen"}, adminToken)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Hello") || strings.Contains(response.Body.String(), stored.Spec.Auth.Token) {
		t.Fatalf("invocation failed: %d %s", response.Code, response.Body)
	}
	if err := api.store.update(job.ID, func(j *storedJob) { j.Job.Deployment.URL = "https://127.0.0.1/private" }); err != nil {
		t.Fatal(err)
	}
	response = apiRequest(api, "POST", "/v1/deployments/"+job.ID+"/invoke", map[string]string{}, adminToken)
	if response.Code != 502 {
		t.Fatal("untrusted invocation endpoint accepted")
	}
	if response := apiRequest(api, "POST", "/v1/deployments/"+job.ID+"/invoke", map[string]string{}, ""); response.Code != 401 {
		t.Fatal("unauthorized invocation accepted")
	}
}

func TestDashboardOriginConfiguration(t *testing.T) {
	for _, value := range []string{"http://dashboard.example", "https://dashboard.example/path", "https://dashboard.example?token=secret", "https://user@dashboard.example"} {
		if _, err := dashboardOrigin(value); err == nil {
			t.Fatalf("accepted invalid origin %s", value)
		}
	}
	if value, err := dashboardOrigin("https://dashboard.example/"); err != nil || value != "https://dashboard.example" {
		t.Fatal("valid origin rejected")
	}
}
