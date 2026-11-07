package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"unikraft.com/cloud/sdk/platform"
	"unikraft.com/x/build"
)

const testUUID = "6f297f5b-5682-42fc-9b6c-0b461232c89b"

var testDigest = digest.FromString("built image")

type fakeCloud struct {
	request        platform.CreateInstanceRequest
	creates        int
	instance       platform.Instance
	createResponse *platform.Response[platform.CreateInstanceResponseData]
	deleteResponse *platform.Response[platform.DeleteInstancesResponseData]
	identifier     platform.NameOrUUID
	deleted        platform.DeleteInstanceRequestItem
}

func (f *fakeCloud) CreateInstance(_ context.Context, request platform.CreateInstanceRequest) (*platform.Response[platform.CreateInstanceResponseData], error) {
	f.request = request
	f.creates++
	if f.createResponse != nil {
		return f.createResponse, nil
	}
	return &platform.Response[platform.CreateInstanceResponseData]{
		Status: platform.ResponseStatusSuccess,
		Data:   &platform.CreateInstanceResponseData{Instances: []platform.Instance{f.instance}},
	}, nil
}

func (f *fakeCloud) GetInstances(_ context.Context, ids []platform.NameOrUUID, _ platform.GetInstancesOpts) (*platform.Response[platform.GetInstancesResponseData], error) {
	f.identifier = ids[0]
	return &platform.Response[platform.GetInstancesResponseData]{
		Status: platform.ResponseStatusSuccess,
		Data:   &platform.GetInstancesResponseData{Instances: []platform.Instance{f.instance}},
	}, nil
}

func (f *fakeCloud) DeleteInstances(_ context.Context, items []platform.DeleteInstanceRequestItem) (*platform.Response[platform.DeleteInstancesResponseData], error) {
	f.deleted = items[0]
	if f.deleteResponse != nil {
		return f.deleteResponse, nil
	}
	return &platform.Response[platform.DeleteInstancesResponseData]{
		Status: platform.ResponseStatusSuccess,
		Data:   &platform.DeleteInstancesResponseData{Instances: []platform.DeleteInstancesResponseInstance{{Status: platform.ResponseStatusSuccess}}},
	}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func newTestPipeline(t *testing.T) (pipeline, buildSpec, *fakeCloud) {
	t.Helper()
	source := "def handler(event):\n    return event\n"
	cloud := &fakeCloud{instance: platform.Instance{
		Uuid: testUUID, Name: "hello", State: platform.InstanceStateRunning, MemoryMb: 128,
		ServiceGroup: &platform.InstanceServiceGroup{Domains: []platform.ServiceGroupInstanceDomain{{Fqdn: "hello.dal.unikraft.app."}}},
	}}
	pipeline := pipeline{
		Cloud: cloud, Metro: "dal",
		Build: func(ctx context.Context, directory, output string) (build.Saved, error) {
			// Parse the actual generated Kraftfile through the production loader.
			opts, err := build.LoadBuildOpts(ctx, directory)
			if err != nil {
				return build.Saved{}, err
			}
			if err := opts.FilterArch("x86_64"); err != nil {
				return build.Saved{}, err
			}
			if strings.Join(opts.Cmd, " ") != "/usr/bin/python3 -u /src/server.py handler" {
				return build.Saved{}, errors.New("generated image has the wrong startup command")
			}
			if _, err := os.Stat(filepath.Join(directory, "function.py")); err != nil {
				return build.Saved{}, err
			}
			return build.Saved{Ref: "unikraft.io/test/hello:latest", Digest: testDigest}, nil
		},
		HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != "GET" || request.URL.String() != "https://hello.dal.unikraft.app/healthz" {
				t.Errorf("unexpected readiness request: %s %s", request.Method, request.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
		})},
	}
	return pipeline, buildSpec{Source: source, Entry: "handler", Image: "test/hello", Name: "hello", MemoryMB: 128, Auth: invocationAuth{Mode: "public"}}, cloud
}

func TestDeployBuildsAndCreatesPinnedInstance(t *testing.T) {
	pipeline, opts, cloud := newTestPipeline(t)
	record, err := pipeline.Deploy(context.Background(), opts, opts.Name)
	if err != nil {
		t.Fatal(err)
	}
	pinned := "unikraft.io/test/hello@" + testDigest.String()
	if record.Ready == nil || !*record.Ready || record.Image != pinned || record.URL != "https://hello.dal.unikraft.app" || record.Metro != "dal" {
		t.Fatalf("unexpected deployment: %+v", record)
	}
	if cloud.request.Image != platform.ImageReference(pinned) {
		t.Fatalf("deployed %v, want %s", cloud.request.Image, pinned)
	}
	if cloud.request.Autostart == nil || !*cloud.request.Autostart || *cloud.request.MemoryMb != 128 {
		t.Fatalf("unexpected instance resources: %+v", cloud.request)
	}
	service := cloud.request.ServiceGroup.Services[0]
	if service.Port != 443 || *service.DestinationPort != 8080 || len(service.Handlers) != 2 {
		t.Fatalf("unexpected service: %+v", service)
	}
	if cloud.request.ScaleToZero == nil || *cloud.request.ScaleToZero.Policy != platform.InstanceScaleToZeroPolicyOn {
		t.Fatal("scale to zero was not enabled")
	}
	if cloud.identifier.Uuid == nil || *cloud.identifier.Uuid != testUUID {
		t.Fatal("readiness did not inspect the created UUID")
	}
}

func TestBuildFailureNeverCreatesInstance(t *testing.T) {
	pipeline, opts, cloud := newTestPipeline(t)
	pipeline.Build = func(context.Context, string, string) (build.Saved, error) {
		return build.Saved{}, errors.New("build failed")
	}
	if record, err := pipeline.Deploy(context.Background(), opts, opts.Name); err == nil || record != nil || cloud.creates != 0 {
		t.Fatalf("record=%+v err=%v creates=%d", record, err, cloud.creates)
	}
}

func TestRuntimeSecretsAreNotBakedIntoImage(t *testing.T) {
	pipeline, opts, cloud := newTestPipeline(t)
	opts.Env = map[string]string{"GREETING": "private-env-value-abcdefgh"}
	opts.Auth = invocationAuth{Mode: "bearer", Token: runtimeToken}
	originalBuild := pipeline.Build
	pipeline.Build = func(ctx context.Context, directory, output string) (build.Saved, error) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), runtimeToken) || strings.Contains(string(data), opts.Env["GREETING"]) {
				t.Fatal("runtime secret was copied to build context")
			}
		}
		return originalBuild(ctx, directory, output)
	}
	pipeline.HTTP = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer "+runtimeToken {
			t.Error("readiness was unauthenticated")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
	})}
	record, err := pipeline.Deploy(context.Background(), opts, opts.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cloud.request.Env["GREETING"] != opts.Env["GREETING"] || cloud.request.Env["LAMBDA_INVOCATION_TOKEN"] != runtimeToken || cloud.request.Env["LAMBDA_AUTH_MODE"] != "bearer" {
		t.Fatal("runtime configuration was not passed to Cloud")
	}
	if len(opts.Env) != 1 {
		t.Fatal("mutated caller environment map")
	}
	data, _ := json.Marshal(record)
	if !record.Protected || strings.Contains(string(data), runtimeToken) || strings.Contains(string(data), opts.Env["GREETING"]) {
		t.Fatal("deployment record exposed credentials or lost auth mode")
	}
}

func TestReadinessTimeoutPreservesDeployment(t *testing.T) {
	pipeline, opts, _ := newTestPipeline(t)
	pipeline.ReadinessTimeout = 10 * time.Millisecond
	pipeline.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("starting"))}, nil
	})}
	record, err := pipeline.Deploy(context.Background(), opts, opts.Name)
	if !errors.Is(err, context.DeadlineExceeded) || record == nil || record.UUID != testUUID || record.Ready != nil {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("missing health failure detail: %v", err)
	}
}

func TestCreatePerItemErrorIsNotSuccess(t *testing.T) {
	pipeline, opts, cloud := newTestPipeline(t)
	cloud.instance.Status = new(platform.ResponseStatusError)
	cloud.instance.Message = new("memory quota exceeded")
	record, err := pipeline.Deploy(context.Background(), opts, opts.Name)
	if err == nil || !strings.Contains(err.Error(), "memory quota exceeded") || record == nil || record.UUID != testUUID {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestCreateMissingDataIsNotSuccess(t *testing.T) {
	pipeline, opts, cloud := newTestPipeline(t)
	cloud.createResponse = &platform.Response[platform.CreateInstanceResponseData]{Status: platform.ResponseStatusSuccess}
	if _, err := pipeline.Deploy(context.Background(), opts, opts.Name); err == nil {
		t.Fatal("accepted missing API data")
	}
}

func TestDeleteAndInspectIdentifiersAndItemErrors(t *testing.T) {
	pipeline, _, cloud := newTestPipeline(t)
	if _, err := pipeline.Inspect(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if cloud.identifier.Name == nil || *cloud.identifier.Name != "hello" {
		t.Fatal("inspect did not select instance by name")
	}
	if err := pipeline.Delete(context.Background(), testUUID); err != nil {
		t.Fatal(err)
	}
	if cloud.deleted.Uuid == nil || *cloud.deleted.Uuid != testUUID {
		t.Fatal("delete did not select instance by UUID")
	}
	cloud.deleteResponse = &platform.Response[platform.DeleteInstancesResponseData]{
		Status: platform.ResponseStatusSuccess,
		Data: &platform.DeleteInstancesResponseData{Instances: []platform.DeleteInstancesResponseInstance{{
			Status: platform.ResponseStatusError, Message: new("instance is locked"),
		}}},
	}
	if err := pipeline.Delete(context.Background(), "hello"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("accepted per-item delete failure: %v", err)
	}
}

func TestInvalidBuildSpecNeverBuild(t *testing.T) {
	for _, change := range []func(*buildSpec){
		func(o *buildSpec) { o.Entry = "handler; bad" },
		func(o *buildSpec) { o.MemoryMB = -1 },
		func(o *buildSpec) { o.MemoryMB = 64 },
		func(o *buildSpec) { o.Image = "oci-layout:///tmp/output" },
	} {
		pipeline, opts, cloud := newTestPipeline(t)
		change(&opts)
		pipeline.Build = func(context.Context, string, string) (build.Saved, error) {
			t.Fatal("invalid options reached builder")
			return build.Saved{}, nil
		}
		if _, err := pipeline.Deploy(context.Background(), opts, opts.Name); err == nil || cloud.creates != 0 {
			t.Fatalf("invalid options accepted: %+v, err=%v", opts, err)
		}
	}
}

func TestPreparedContextIsRemoved(t *testing.T) {
	pipeline, opts, _ := newTestPipeline(t)
	var directory string
	pipeline.Build = func(_ context.Context, path, _ string) (build.Saved, error) {
		directory = path
		return build.Saved{}, errors.New("stop build")
	}
	_, _ = pipeline.Deploy(context.Background(), opts, opts.Name)
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("temporary context remains: %s, %v", directory, err)
	}
}

func TestDeploymentWaitsForBuilderResources(t *testing.T) {
	pipeline, opts, cloud := newTestPipeline(t)
	called := false
	pipeline.AfterBuild = func(context.Context) error {
		called = true
		if cloud.creates != 0 {
			t.Fatal("created instance before releasing builder quota")
		}
		return nil
	}
	if _, err := pipeline.Deploy(context.Background(), opts, opts.Name); err != nil {
		t.Fatal(err)
	}
	if !called || cloud.creates != 1 {
		t.Fatal("builder resource wait was skipped")
	}
	cloud.creates = 0
	pipeline.AfterBuild = func(context.Context) error { return context.DeadlineExceeded }
	if _, err := pipeline.Deploy(context.Background(), opts, opts.Name); !errors.Is(err, context.DeadlineExceeded) || cloud.creates != 0 {
		t.Fatalf("created instance despite occupied quota: %v", err)
	}
}

func TestRejectedCreationDoesNotInventAnInstance(t *testing.T) {
	pipeline, opts, cloud := newTestPipeline(t)
	cloud.createResponse = &platform.Response[platform.CreateInstanceResponseData]{
		Status:  platform.ResponseStatusError,
		Message: "quota exceeded",
		Data:    &platform.CreateInstanceResponseData{Instances: []platform.Instance{{Status: new(platform.ResponseStatusError)}}},
	}
	if record, err := pipeline.Deploy(context.Background(), opts, opts.Name); err == nil || record != nil {
		t.Fatalf("invented a resource for rejected creation: record=%+v err=%v", record, err)
	}
}
