package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const adminToken = "test-admin-token-0123456789abcdef0123456789"
const runtimeToken = "test-runtime-token-0123456789abcdef01234567"

func testSpec() buildSpec {
	return buildSpec{Source: "def handler(event):\n    return event\n", Image: "test/function", Env: map[string]string{"GREETING": "private-environment-value"}}
}

type fakeManager struct {
	mu        sync.Mutex
	inspected string
	deleted   string
}

func (m *fakeManager) Inspect(_ context.Context, id string) (*deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inspected = id
	return &deployment{UUID: testUUID, Name: "managed", Metro: "dal", State: "standby", Protected: true}, nil
}
func (m *fakeManager) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = id
	return nil
}

func testStore(t *testing.T, pending int) *jobStore {
	t.Helper()
	store, err := openJobStore(t.TempDir(), bytes.Repeat([]byte{7}, 32), pending, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testAPI(t *testing.T, runner deployRunner) *controlPlane {
	t.Helper()
	api, err := newControlPlane(testStore(t, 8), adminToken, "dal", runner, &fakeManager{})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func apiRequest(api *controlPlane, method, path string, value any, token string) *httptest.ResponseRecorder {
	var body io.Reader
	if value != nil {
		data, _ := json.Marshal(value)
		body = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	return recorder
}

func submitTestJob(t *testing.T, api *controlPlane, spec buildSpec) (Job, string) {
	t.Helper()
	response := apiRequest(api, "POST", "/v1/deployments", spec, adminToken)
	if response.Code != 202 {
		t.Fatalf("submit: HTTP %d %s", response.Code, response.Body)
	}
	var accepted struct {
		Job             Job    `json:"job"`
		InvocationToken string `json:"invocation_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if response.Header().Get("Location") != "/v1/jobs/"+accepted.Job.ID {
		t.Fatal("missing job location")
	}
	return accepted.Job, accepted.InvocationToken
}

func waitJob(t *testing.T, api *controlPlane, id, status string) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := api.store.get(id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Job.Status == status {
			return job.Job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", id, status)
	return Job{}
}

func startTestWorker(t *testing.T, api *controlPlane) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- api.Worker(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	})
}

func TestControlPlaneRequiresAuthenticationOnEveryRoute(t *testing.T) {
	api := testAPI(t, nil)
	for _, path := range []string{"/healthz", "/v1/jobs", "/v1/deployments", "/v1/jobs/missing", "/unknown"} {
		for _, token := range []string{"", "wrong-token"} {
			response := apiRequest(api, "GET", path, nil, token)
			if response.Code != 401 || response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("unprotected route %s: %d", path, response.Code)
			}
		}
	}
	if response := apiRequest(api, "GET", "/healthz", nil, adminToken); response.Code != 200 {
		t.Fatal(response.Body)
	}
	request := httptest.NewRequest("GET", "/healthz?token="+adminToken, nil)
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("accepted URL token")
	}
	request.Header.Add("Authorization", "Bearer "+adminToken)
	request.Header.Add("Authorization", "Bearer wrong-token")
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("accepted duplicate auth headers")
	}
}

func TestSubmissionReturnsTokenOnceAndHidesInputs(t *testing.T) {
	api := testAPI(t, nil)
	spec := testSpec()
	job, token := submitTestJob(t, api, spec)
	if !job.Protected || !validSecret(token) {
		t.Fatal("submission was not protected by default")
	}
	for _, path := range []string{"/v1/jobs", "/v1/jobs/" + job.ID} {
		response := apiRequest(api, "GET", path, nil, adminToken)
		for _, secret := range []string{spec.Source, spec.Env["GREETING"], token, "\"env\"", "\"source\"", "invocation_token"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("secret in status response: %s", secret)
			}
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("status may be cached")
		}
	}
	stored, err := api.store.get(job.ID)
	if err != nil || stored.Spec.Auth.Token != token {
		t.Fatalf("token was not durably persisted: %v", err)
	}
	public := testSpec()
	public.Auth.Mode = "public"
	publicJob, publicToken := submitTestJob(t, api, public)
	if publicJob.Protected || publicToken != "" {
		t.Fatal("explicit public mode was ignored")
	}
}

func TestWorkerIsSerialAndPersistsProgress(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	var active, peak atomic.Int32
	api := testAPI(t, func(ctx context.Context, spec buildSpec, name string, progress io.Writer) (*deployment, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > peak.Load() {
			peak.Store(n)
		}
		fmt.Fprintln(progress, "Pushing image...")
		started <- name
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		return &deployment{UUID: testUUID, Name: name, Protected: spec.Auth.Mode == "bearer"}, nil
	})
	first, _ := submitTestJob(t, api, testSpec())
	second, _ := submitTestJob(t, api, testSpec())
	startTestWorker(t, api)
	select {
	case name := <-started:
		if name != first.Name {
			t.Fatal("queue order changed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	if job, _ := api.store.get(first.ID); job.Job.Phase != "pushing" {
		t.Fatalf("progress missing: %+v", job.Job)
	}
	if job, _ := api.store.get(second.ID); job.Job.Status != "queued" {
		t.Fatal("second job ran concurrently")
	}
	select {
	case <-started:
		t.Fatal("concurrent build")
	default:
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("second job did not start")
	}
	release <- struct{}{}
	waitJob(t, api, first.ID, "succeeded")
	waitJob(t, api, second.ID, "succeeded")
	if peak.Load() != 1 {
		t.Fatalf("peak concurrency %d", peak.Load())
	}
}

func TestValidationAndQueueLimits(t *testing.T) {
	api := testAPI(t, nil)
	api.store.maxPending = 1
	for _, body := range []any{
		map[string]any{"source": "x", "image": "test/image", "unknown": true},
		map[string]any{"source": "x", "image": "test/image", "env": map[string]string{"LAMBDA_AUTH_MODE": "public"}},
		map[string]any{"source": "x", "image": "test/image", "auth": map[string]string{"mode": "oops"}},
		map[string]any{"source": "x", "image": "oci-layout:///tmp/out"},
	} {
		response := apiRequest(api, "POST", "/v1/deployments", body, adminToken)
		if response.Code != 400 {
			t.Fatalf("accepted invalid spec: HTTP %d %s", response.Code, response.Body)
		}
	}
	submitTestJob(t, api, testSpec())
	response := apiRequest(api, "POST", "/v1/deployments", testSpec(), adminToken)
	if response.Code != 429 {
		t.Fatalf("queue limit ignored: %d", response.Code)
	}
	request := httptest.NewRequest("POST", "/v1/deployments", strings.NewReader(`{"source":"`+strings.Repeat("x", (2<<20)+1)+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != 413 {
		t.Fatalf("body limit ignored: %d", response.Code)
	}
}

func TestManagedInstanceAndRecordDeletion(t *testing.T) {
	api := testAPI(t, nil)
	job, _ := submitTestJob(t, api, testSpec())
	if response := apiRequest(api, "DELETE", "/v1/deployments/"+job.ID, nil, adminToken); response.Code != 409 {
		t.Fatal("deleted queued job")
	}
	if err := api.store.update(job.ID, func(job *storedJob) { job.Job.Status = "succeeded"; job.Job.Deployment = &deployment{UUID: testUUID} }); err != nil {
		t.Fatal(err)
	}
	if response := apiRequest(api, "DELETE", "/v1/jobs/"+job.ID, nil, adminToken); response.Code != 409 {
		t.Fatal("forgot a live instance")
	}
	if response := apiRequest(api, "GET", "/v1/deployments/"+job.ID, nil, adminToken); response.Code != 200 {
		t.Fatalf("inspect: %s", response.Body)
	}
	if response := apiRequest(api, "DELETE", "/v1/deployments/"+job.ID, nil, adminToken); response.Code != 204 {
		t.Fatalf("delete: %s", response.Body)
	}
	manager := api.instances.(*fakeManager)
	manager.mu.Lock()
	id := manager.deleted
	manager.mu.Unlock()
	if id != testUUID {
		t.Fatal("deleted instance unrelated to recorded deployment")
	}
	if response := apiRequest(api, "DELETE", "/v1/deployments/"+testUUID, nil, adminToken); response.Code != 404 {
		t.Fatal("accepted arbitrary resource UUID")
	}
	if response := apiRequest(api, "DELETE", "/v1/jobs/"+job.ID, nil, adminToken); response.Code != 204 {
		t.Fatalf("remove record: %s", response.Body)
	}
	if response := apiRequest(api, "GET", "/v1/jobs/"+job.ID, nil, adminToken); response.Code != 404 {
		t.Fatal("removed record remains visible")
	}
}

func TestFailuresRedactSecretsAndRetainInstance(t *testing.T) {
	spec := testSpec()
	spec.Auth.Token = runtimeToken
	api := testAPI(t, func(context.Context, buildSpec, string, io.Writer) (*deployment, error) {
		return &deployment{UUID: testUUID}, errors.New("failed " + spec.Env["GREETING"] + " " + runtimeToken + " " + adminToken)
	})
	job, _ := submitTestJob(t, api, spec)
	startTestWorker(t, api)
	failed := waitJob(t, api, job.ID, "failed")
	if failed.Deployment == nil || failed.Deployment.UUID != testUUID {
		t.Fatal("lost resource after failure")
	}
	for _, secret := range []string{spec.Env["GREETING"], runtimeToken, adminToken} {
		if strings.Contains(failed.Error, secret) {
			t.Fatal("error leaked secret")
		}
	}
}

func TestEncryptedStoreRecoveryAndLock(t *testing.T) {
	directory := t.TempDir()
	key := bytes.Repeat([]byte{3}, 32)
	store, err := openJobStore(directory, key, 4, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openJobStore(directory, key, 4, 8); err == nil {
		t.Fatal("second writer acquired store")
	}
	spec := testSpec()
	if err := spec.normalize(); err != nil {
		t.Fatal(err)
	}
	queued, err := store.submit(spec, "dal")
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.claim()
	if err != nil || active == nil {
		t.Fatalf("claim: %v", err)
	}
	second, err := store.submit(spec, "dal")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, queued.Job.ID+".job"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{spec.Source, spec.Env["GREETING"], spec.Auth.Token} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("plaintext job input on disk")
		}
	}
	store.Close()
	if _, err := openJobStore(directory, bytes.Repeat([]byte{4}, 32), 4, 8); err == nil {
		t.Fatal("wrong encryption key accepted")
	}
	reopened, err := openJobStore(directory, key, 4, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, _ := reopened.get(active.Job.ID)
	if recovered.Job.Status != "interrupted" {
		t.Fatal("running job would be silently replayed")
	}
	next, err := reopened.claim()
	if err != nil || next == nil || next.Job.ID != second.Job.ID {
		t.Fatalf("queued job not resumed: %v", err)
	}
	if next.Spec.Auth.Token != spec.Auth.Token {
		t.Fatal("invocation credential changed after restart")
	}
	if _, err := newControlPlane(reopened, adminToken, "fra", nil, &fakeManager{}); err != nil {
		t.Fatal("rejected valid multi-metro state:", err)
	}
}

func TestFailedJobWithEmptyUUIDUsesGeneratedName(t *testing.T) {
	api := testAPI(t, nil)
	job, _ := submitTestJob(t, api, testSpec())
	if err := api.store.update(job.ID, func(stored *storedJob) {
		stored.Job.Status = "failed"
		stored.Job.Deployment = &deployment{}
	}); err != nil {
		t.Fatal(err)
	}
	response := apiRequest(api, "DELETE", "/v1/deployments/"+job.ID, nil, adminToken)
	if response.Code != 204 {
		t.Fatalf("cleanup failed: %s", response.Body)
	}
	manager := api.instances.(*fakeManager)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.deleted != job.Name {
		t.Fatalf("cleanup used %q instead of the generated name", manager.deleted)
	}
}
