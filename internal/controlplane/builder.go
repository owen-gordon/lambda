package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"unikraft.com/cloud/sdk/platform"
	"unikraft.com/x/buildkit"
)

const builderKey = "my-builder-slim"
const builderInstanceName = "remote-builder-" + builderKey

// waitBuilderIdle frees the builder's live CPU/memory quota before a function
// starts. Merely closing its connection does not synchronously enter standby.
func waitBuilderIdle(ctx context.Context, cloud interface {
	GetInstances(context.Context, []platform.NameOrUUID, platform.GetInstancesOpts) (*platform.Response[platform.GetInstancesResponseData], error)
}, name string) error {
	for {
		response, err := cloud.GetInstances(ctx, []platform.NameOrUUID{{Name: &name}}, platform.GetInstancesOpts{})
		if err != nil {
			return err
		}
		if err := responseError(response); err != nil {
			return err
		}
		if len(response.Data.Instances) != 1 {
			return fmt.Errorf("builder %s not found while waiting for standby", name)
		}
		instance := response.Data.Instances[0]
		if err := instanceError(instance); err != nil {
			return err
		}
		if instance.State == platform.InstanceStateStandby || instance.State == platform.InstanceStateStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// A just-served dashboard or function may temporarily occupy the spare vCPU.
// Wait for it to sleep rather than failing a newly accepted build immediately.
func acquireBuilder(ctx context.Context, progress io.Writer, acquire func(context.Context) (*buildkit.Daemon, error)) (*buildkit.Daemon, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	reported := false
	for {
		daemon, err := acquire(ctx)
		if err == nil {
			return daemon, nil
		}
		if !errors.Is(err, buildkit.ErrQuota) {
			return nil, err
		}
		if !reported {
			fmt.Fprintln(progress, "Waiting for builder capacity...")
			reported = true
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}

// The static console must release its vCPU before a function can wake on the
// two-vCPU tier. The browser itself keeps working while that VM is asleep.
func waitDashboardIdle(ctx context.Context, cloud instanceClient) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name := "lambda-dashboard"
	for {
		response, err := cloud.GetInstances(ctx, []platform.NameOrUUID{{Name: &name}}, platform.GetInstancesOpts{})
		if platform.ErrorContainsOnly(err, platform.APIHTTPErrorNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := responseError(response); err != nil {
			return err
		}
		if len(response.Data.Instances) != 1 {
			return fmt.Errorf("dashboard state unavailable")
		}
		state := response.Data.Instances[0].State
		if state == platform.InstanceStateStandby || state == platform.InstanceStateStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
