package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	distref "github.com/distribution/reference"
	"github.com/google/uuid"
	"unikraft.com/cloud/sdk/platform"
	"unikraft.com/x/build"
	"unikraft.com/x/image-spec/reference"
)

// instanceClient is the control-plane surface the pipeline needs. Keeping this
// small keeps the hosted deployment worker testable without Cloud resources.
type instanceClient interface {
	CreateInstance(context.Context, platform.CreateInstanceRequest) (*platform.Response[platform.CreateInstanceResponseData], error)
	GetInstances(context.Context, []platform.NameOrUUID, platform.GetInstancesOpts) (*platform.Response[platform.GetInstancesResponseData], error)
	DeleteInstances(context.Context, []platform.DeleteInstanceRequestItem) (*platform.Response[platform.DeleteInstancesResponseData], error)
}

type pipeline struct {
	Cloud            instanceClient
	ForMetro         func(string) instanceClient
	Build            func(context.Context, string, string) (build.Saved, error)
	AfterBuild       func(context.Context) error
	Metro            string
	Progress         io.Writer
	HTTP             *http.Client
	ReadinessTimeout time.Duration
}

// inMetro changes only instance operations; Build and AfterBuild keep the home builder.
func (p pipeline) inMetro(metro string) pipeline {
	if metro != "" && metro != p.Metro {
		p.Cloud = p.ForMetro(metro)
		p.Metro = metro
	}
	return p
}

// deployment contains no credentials. Inspect refreshes the state from Cloud;
// Ready only appears after an HTTP readiness check, not just a running VM state.
type deployment struct {
	UUID      string                 `json:"uuid"`
	Name      string                 `json:"name"`
	Metro     string                 `json:"metro"`
	Image     string                 `json:"image"`
	State     platform.InstanceState `json:"state"`
	URL       string                 `json:"url,omitempty"`
	Ready     *bool                  `json:"ready,omitempty"`
	MemoryMB  uint64                 `json:"memory_mb"`
	Protected bool                   `json:"protected"`
}

func (p pipeline) Deploy(ctx context.Context, spec buildSpec, name string) (*deployment, error) {
	if err := spec.normalize(); err != nil {
		return nil, err
	}
	readinessTimeout := p.ReadinessTimeout
	if readinessTimeout == 0 {
		readinessTimeout = 2 * time.Minute
	}
	if readinessTimeout < 0 {
		return nil, errors.New("readiness timeout must be positive")
	}

	directory, cleanup, err := prepareFunction(spec.Source, spec.Entry)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	p.report("Prepared Python function entrypoint %s\n", spec.Entry)
	saved, err := p.Build(ctx, directory, spec.Image)
	if err != nil {
		return nil, err
	}
	image, err := pinnedImage(saved)
	if err != nil {
		return nil, err
	}
	if p.AfterBuild != nil {
		p.report("Waiting for builder to enter standby...\n")
		if err := p.AfterBuild(ctx); err != nil {
			return nil, fmt.Errorf("release builder resources before deployment: %w", err)
		}
	}
	request := deploymentRequest(spec, name, image)
	p.report("Creating instance in %s using %s...\n", p.Metro, image)
	response, err := p.Cloud.CreateInstance(ctx, request)
	// Even an API error may contain a resource that was created but failed to
	// start. Return its identity so it remains manageable by the caller.
	var record *deployment
	if response != nil && response.Data != nil && len(response.Data.Instances) == 1 && response.Data.Instances[0].Uuid != "" {
		record = p.record(response.Data.Instances[0])
		record.Image = image
		record.Protected = spec.Auth.Token != ""
	}
	if err != nil {
		return record, fmt.Errorf("create instance: %w", err)
	}
	if err := responseError(response); err != nil {
		return record, fmt.Errorf("create instance: %w", err)
	}
	if record == nil || record.UUID == "" {
		return record, errors.New("create instance: API returned no instance UUID")
	}
	instance := response.Data.Instances[0]
	if err := instanceError(instance); err != nil {
		return record, fmt.Errorf("create instance %s: %w", record.UUID, err)
	}

	p.report("Created %s (%s); waiting for HTTP readiness...\n", record.Name, record.UUID)
	waitCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	record, err = p.waitReady(waitCtx, record, spec.Auth.Token)
	if err != nil {
		err = p.readinessFailure(ctx, record, err)
	}
	return record, err
}

func pinnedImage(saved build.Saved) (string, error) {
	if err := saved.Digest.Validate(); err != nil {
		return "", fmt.Errorf("invalid built image digest: %w", err)
	}
	named, err := reference.ParseNormalizedNamed(saved.Ref)
	if err != nil {
		return "", fmt.Errorf("parse built image: %w", err)
	}
	pinned, err := distref.WithDigest(distref.TrimNamed(named), saved.Digest)
	if err != nil {
		return "", err
	}
	return pinned.String(), nil
}

func deploymentRequest(spec buildSpec, name, image string) platform.CreateInstanceRequest {
	request := platform.CreateInstanceRequest{
		Image:    platform.ImageReference(image),
		MemoryMb: &spec.MemoryMB,
		Vcpus:    new(int32(1)), Autostart: new(true),
		Tags: []string{"lambda"},
		ServiceGroup: &platform.CreateInstanceRequestServiceGroup{
			Services: []platform.Service{{
				Port: 443, DestinationPort: new(uint32(8080)),
				Protocol: new(platform.ServiceProtocol("tcp")),
				Handlers: []platform.ConnectionHandler{"tls", "http"},
			}},
		},
		ScaleToZero: &platform.CreateInstanceScaleToZero{
			Policy:         new(platform.InstanceScaleToZeroPolicyOn),
			CooldownTimeMs: new(int32(1000)),
		},
	}
	if name != "" {
		request.Name = &name
	}
	request.Env = make(map[string]string, len(spec.Env)+2)
	for key, value := range spec.Env {
		request.Env[key] = value
	}
	request.Env["LAMBDA_AUTH_MODE"] = "public"
	if spec.Auth.Token != "" {
		request.Env["LAMBDA_AUTH_MODE"] = "bearer"
		request.Env["LAMBDA_INVOCATION_TOKEN"] = spec.Auth.Token
	}
	return request
}

func (p pipeline) waitReady(ctx context.Context, record *deployment, invocationToken string) (*deployment, error) {
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	var lastErr error
	for {
		current, err := p.Inspect(ctx, record.UUID)
		if err != nil {
			return record, fmt.Errorf("inspect created instance %s: %w", record.UUID, err)
		}
		// Preserve the exact built reference even if Cloud reports a short name.
		current.Image = record.Image
		current.Protected = record.Protected
		record = current
		if record.State == platform.InstanceStateStopped || record.State == platform.InstanceStateDeleted {
			return record, fmt.Errorf("instance %s entered state %s before becoming ready", record.UUID, record.State)
		}
		if record.URL != "" {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, record.URL+"/healthz", nil)
			if err != nil {
				return record, err
			}
			if invocationToken != "" {
				request.Header.Set("Authorization", "Bearer "+invocationToken)
			}
			response, err := client.Do(request)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					record.Ready = new(true)
					return record, nil
				}
				err = fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
			}
			lastErr = err
		} else {
			lastErr = errors.New("instance has no public domain yet")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return record, fmt.Errorf("instance %s not ready (%v): %w", record.UUID, lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func (p pipeline) Inspect(ctx context.Context, id string) (*deployment, error) {
	response, err := p.Cloud.GetInstances(ctx, []platform.NameOrUUID{instanceIdentifier(id)}, platform.GetInstancesOpts{})
	if err != nil {
		return nil, err
	}
	if err := responseError(response); err != nil {
		return nil, err
	}
	if len(response.Data.Instances) != 1 {
		return nil, fmt.Errorf("expected one instance for %q, received %d", id, len(response.Data.Instances))
	}
	instance := response.Data.Instances[0]
	if err := instanceError(instance); err != nil {
		return nil, err
	}
	return p.record(instance), nil
}

func (p pipeline) Delete(ctx context.Context, id string) error {
	identifier := instanceIdentifier(id)
	response, err := p.Cloud.DeleteInstances(ctx, []platform.DeleteInstanceRequestItem{{
		Uuid: identifier.Uuid, Name: identifier.Name,
		TimeoutS: new(int64(30)),
	}})
	if err != nil {
		return err
	}
	if err := responseError(response); err != nil {
		return err
	}
	if len(response.Data.Instances) != 1 {
		return errors.New("delete instance: API returned no deletion result")
	}
	item := response.Data.Instances[0]
	if item.Status != platform.ResponseStatusSuccess {
		return fmt.Errorf("delete instance: %s", itemMessage(item.Message, string(item.Status)))
	}
	return nil
}

func instanceIdentifier(id string) platform.NameOrUUID {
	if _, err := uuid.Parse(id); err == nil {
		return platform.NameOrUUID{Uuid: &id}
	}
	return platform.NameOrUUID{Name: &id}
}

func (p pipeline) record(instance platform.Instance) *deployment {
	record := &deployment{
		UUID: instance.Uuid, Name: instance.Name, Metro: p.Metro,
		Image: instance.Image, State: instance.State, MemoryMB: instance.MemoryMb,
		Protected: instance.Env["LAMBDA_AUTH_MODE"] == "bearer",
	}
	if instance.ServiceGroup != nil {
		for _, domain := range instance.ServiceGroup.Domains {
			if domain.Fqdn != "" {
				record.URL = "https://" + strings.TrimSuffix(domain.Fqdn, ".")
				break
			}
		}
	}
	return record
}

func responseError[T any](response *platform.Response[T]) error {
	if response == nil {
		return errors.New("API returned an empty response")
	}
	if response.Status != platform.ResponseStatusSuccess {
		return fmt.Errorf("API status %s: %s", response.Status, response.Message)
	}
	if response.Data == nil {
		return errors.New("API returned no response data")
	}
	return nil
}

func instanceError(instance platform.Instance) error {
	if instance.Status != nil && *instance.Status != platform.ResponseStatusSuccess {
		return errors.New(itemMessage(instance.Message, string(*instance.Status)))
	}
	return nil
}

func itemMessage(message *string, fallback string) string {
	if message != nil && *message != "" {
		return *message
	}
	return fallback
}

func (p pipeline) report(format string, args ...any) {
	if p.Progress != nil {
		fmt.Fprintf(p.Progress, format, args...)
	}
}
