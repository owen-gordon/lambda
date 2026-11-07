package controlplane

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeSleepKernel struct {
	mu            sync.Mutex
	count         int
	commands      []string
	beforeRelease func()
	fail          string
}

func (k *fakeSleepKernel) write(command string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if command == k.fail {
		return errors.New("kernel interface failed")
	}
	if command == "-" && k.beforeRelease != nil {
		k.beforeRelease()
	}
	switch command {
	case "+":
		k.count++
	case "-":
		k.count--
	default:
		return errors.New("counter must never be reset")
	}
	k.commands = append(k.commands, command)
	return nil
}

func (k *fakeSleepKernel) value() int { k.mu.Lock(); defer k.mu.Unlock(); return k.count }

func waitAsleep(t *testing.T, kernel *fakeSleepKernel) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if kernel.value() == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("idle worker kept its wake lock")
}

func TestAwakeGateBalancesOverlappingAndRepeatedReleases(t *testing.T) {
	kernel := &fakeSleepKernel{count: 3} // Locks owned by other components.
	gate := &awakeGate{write: kernel.write}
	startup, err := gate.hold()
	if err != nil {
		t.Fatal(err)
	}
	queue, err := gate.hold()
	if err != nil {
		t.Fatal(err)
	}
	if kernel.value() != 4 {
		t.Fatal("did not aggregate local holds")
	}
	if err := startup(); err != nil {
		t.Fatal(err)
	}
	if err := startup(); err != nil {
		t.Fatal(err)
	}
	if kernel.value() != 4 {
		t.Fatal("released while another activity was pending")
	}
	if err := queue(); err != nil {
		t.Fatal(err)
	}
	if err := queue(); err != nil {
		t.Fatal(err)
	}
	if kernel.value() != 3 {
		t.Fatal("changed another component's locks")
	}
}

func TestWakeLockCoversAcceptedAndQueuedJobsUntilResultsPersist(t *testing.T) {
	kernel := &fakeSleepKernel{}
	started := make(chan string, 2)
	finish := make(chan struct{}, 2)
	api := testAPI(t, func(ctx context.Context, spec buildSpec, name string, progress io.Writer) (*deployment, error) {
		if kernel.value() != 1 {
			t.Error("worker ran without a wake lock")
		}
		started <- name
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-finish:
		}
		return &deployment{UUID: testUUID}, nil
	})
	api.awake = &awakeGate{write: kernel.write}
	kernel.beforeRelease = func() {
		if api.store.pending() {
			t.Error("released before the final result was persisted")
		}
	}
	first, _ := submitTestJob(t, api, testSpec())
	second, _ := submitTestJob(t, api, testSpec())
	if kernel.value() != 1 {
		t.Fatal("accepted jobs are unprotected before worker startup")
	}
	startTestWorker(t, api)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	finish <- struct{}{}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("second job did not start")
	}
	if kernel.value() != 1 {
		t.Fatal("released between queued jobs")
	}
	finish <- struct{}{}
	waitJob(t, api, first.ID, "succeeded")
	waitJob(t, api, second.ID, "succeeded")
	waitAsleep(t, kernel)
}

func TestFailedJobAllowsIdleSleep(t *testing.T) {
	kernel := &fakeSleepKernel{}
	api := testAPI(t, func(context.Context, buildSpec, string, io.Writer) (*deployment, error) {
		return nil, errors.New("build failed")
	})
	api.awake = &awakeGate{write: kernel.write}
	job, _ := submitTestJob(t, api, testSpec())
	startTestWorker(t, api)
	waitJob(t, api, job.ID, "failed")
	waitAsleep(t, kernel)
}

func TestRejectedOrUnpersistedSubmissionReleasesWakeLock(t *testing.T) {
	for _, mode := range []string{"capacity", "storage"} {
		t.Run(mode, func(t *testing.T) {
			kernel := &fakeSleepKernel{}
			api := testAPI(t, nil)
			api.awake = &awakeGate{write: kernel.write}
			if mode == "capacity" {
				api.store.maxJobs = 0
			} else {
				api.store.directory = filepath.Join(t.TempDir(), "missing")
			}
			response := apiRequest(api, "POST", "/v1/deployments", testSpec(), adminToken)
			if response.Code == 202 {
				t.Fatal("accepted an unpersisted job")
			}
			if kernel.value() != 0 || len(api.store.list()) != 0 {
				t.Fatal("rejected submission leaked work or wake lock")
			}
		})
	}
}

func TestWakeLockFailureRejectsSubmissionAndStopsWorker(t *testing.T) {
	kernel := &fakeSleepKernel{fail: "+"}
	api := testAPI(t, nil)
	api.awake = &awakeGate{write: kernel.write}
	response := apiRequest(api, "POST", "/v1/deployments", testSpec(), adminToken)
	if response.Code != 503 || len(api.store.list()) != 0 {
		t.Fatal("accepted work without protection")
	}
	if err := api.Worker(context.Background()); err == nil {
		t.Fatal("worker ignored broken wake interface")
	}
}

func TestReleaseFailureStopsServiceInsteadOfKeepingIdleWorkerAlive(t *testing.T) {
	kernel := &fakeSleepKernel{fail: "-"}
	api := testAPI(t, func(context.Context, buildSpec, string, io.Writer) (*deployment, error) {
		return nil, errors.New("build failed")
	})
	api.awake = &awakeGate{write: kernel.write}
	job, _ := submitTestJob(t, api, testSpec())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := api.Worker(ctx); err == nil {
		t.Fatal("worker stayed alive after wake lock release failed")
	}
	if stored, _ := api.store.get(job.ID); stored.Job.Status != "failed" {
		t.Fatal("lost terminal result before lock failure")
	}
	if response := apiRequest(api, "POST", "/v1/deployments", testSpec(), adminToken); response.Code != 503 {
		t.Fatal("stopped worker accepted more work")
	}
}

func TestRecoveredQueueRemainsAwakeDuringStartupHandoff(t *testing.T) {
	kernel := &fakeSleepKernel{}
	gate := &awakeGate{write: kernel.write}
	startup, err := gate.hold()
	if err != nil {
		t.Fatal(err)
	}
	api := testAPI(t, func(context.Context, buildSpec, string, io.Writer) (*deployment, error) {
		return nil, errors.New("test build stopped")
	})
	api.awake = gate
	spec := testSpec()
	if err := spec.normalize(); err != nil {
		t.Fatal(err)
	}
	job, err := api.store.submit(spec, "dal")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.protectPending(); err != nil {
		t.Fatal(err)
	}
	if err := startup(); err != nil {
		t.Fatal(err)
	}
	if kernel.value() != 1 {
		t.Fatal("startup released a recovered queue")
	}
	startTestWorker(t, api)
	waitJob(t, api, job.Job.ID, "failed")
	waitAsleep(t, kernel)
}

func TestConcurrentEnqueueAndQueueDrainNeverSleepWithPendingWork(t *testing.T) {
	kernel := &fakeSleepKernel{}
	api := testAPI(t, func(context.Context, buildSpec, string, io.Writer) (*deployment, error) {
		if kernel.value() != 1 {
			t.Error("unprotected concurrent job")
		}
		return nil, errors.New("test build stopped")
	})
	api.awake = &awakeGate{write: kernel.write}
	kernel.beforeRelease = func() {
		if api.store.pending() {
			t.Error("queue drain raced submission")
		}
	}
	startTestWorker(t, api)
	var submissions sync.WaitGroup
	for range 24 {
		submissions.Go(func() {
			response := apiRequest(api, "POST", "/v1/deployments", testSpec(), adminToken)
			if response.Code != 202 && response.Code != 429 {
				t.Errorf("unexpected submit status %d", response.Code)
			}
		})
	}
	submissions.Wait()
	for _, job := range api.store.list() {
		waitJob(t, api, job.ID, "failed")
	}
	waitAsleep(t, kernel)
}

func TestMissingKernelInterfaceFailsStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if _, err := openAwakeGate(path); err == nil {
		t.Fatal("missing kernel interface silently ignored")
	}
}
