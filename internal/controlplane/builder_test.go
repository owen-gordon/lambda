package controlplane

import (
	"context"
	"errors"
	"io"
	"testing"
	"unikraft.com/x/buildkit"

	"unikraft.com/cloud/sdk/platform"
)

type idleCloud struct {
	instance platform.Instance
	calls    []string
}

func (f *idleCloud) GetInstances(_ context.Context, ids []platform.NameOrUUID, _ platform.GetInstancesOpts) (*platform.Response[platform.GetInstancesResponseData], error) {
	f.calls = append(f.calls, "get")
	return &platform.Response[platform.GetInstancesResponseData]{
		Status: platform.ResponseStatusSuccess,
		Data:   &platform.GetInstancesResponseData{Instances: []platform.Instance{f.instance}},
	}, nil
}

func TestWaitForBuilderStandby(t *testing.T) {
	cloud := &idleCloud{instance: platform.Instance{Uuid: testUUID, State: platform.InstanceStateStandby}}
	if err := waitBuilderIdle(context.Background(), cloud, "builder"); err != nil {
		t.Fatal(err)
	}
	cloud.instance.State = platform.InstanceStateRunning
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitBuilderIdle(ctx, cloud, "builder"); err != context.Canceled {
		t.Fatalf("expected canceled wait, got %v", err)
	}
}

func TestCapacityWaitHonorsCancellationAndDoesNotRetryOtherErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireBuilder(ctx, io.Discard, func(context.Context) (*buildkit.Daemon, error) { return nil, buildkit.ErrQuota }); !errors.Is(err, context.Canceled) {
		t.Fatalf("capacity wait did not stop: %v", err)
	}
	calls := 0
	want := errors.New("authentication failed")
	if _, err := acquireBuilder(context.Background(), io.Discard, func(context.Context) (*buildkit.Daemon, error) { calls++; return nil, want }); !errors.Is(err, want) || calls != 1 {
		t.Fatal("retried a non-quota error")
	}
}
