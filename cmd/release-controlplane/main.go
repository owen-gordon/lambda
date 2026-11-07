// release-controlplane publishes and rolls out the hosted service itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/owen-gordon/lambda/internal/controlplane"
	"unikraft.com/cloud/sdk/platform"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: scripts/release-controlplane.sh <image-reference>")
		os.Exit(2)
	}
	if err := release(ctx, os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func release(ctx context.Context, image string) error {
	token, metro, apiToken := os.Getenv("UKC_TOKEN"), os.Getenv("UKC_METRO"), os.Getenv("LAMBDA_API_TOKEN")
	if token == "" || metro == "" || apiToken == "" {
		return errors.New("export UKC_TOKEN, UKC_METRO, and LAMBDA_API_TOKEN for a service release")
	}
	cloud := platform.NewClient(platform.WithToken(token), platform.WithDefaultMetro(metro))
	name := "lambda-controlplane"
	response, err := cloud.GetInstances(ctx, []platform.NameOrUUID{{Name: &name}}, platform.GetInstancesOpts{})
	if err != nil {
		return err
	}
	if response.Data == nil || len(response.Data.Instances) != 1 || response.Data.Instances[0].Uuid == "" {
		return errors.New("hosted control plane was not found")
	}
	instance := response.Data.Instances[0]
	owned := false
	for _, tag := range instance.Tags {
		if tag == "lambda-controlplane" {
			owned = true
		}
	}
	if !owned {
		return errors.New("refusing to replace an instance without the lambda-controlplane tag")
	}
	if instance.ServiceGroup == nil || len(instance.ServiceGroup.Domains) == 0 {
		return errors.New("control plane has no domain")
	}
	url := "https://" + strings.TrimSuffix(instance.ServiceGroup.Domains[0].Fqdn, ".")
	if err := requireIdleQueue(ctx, url, apiToken); err != nil {
		return err
	}
	pinned, err := controlplane.PublishServiceImage(ctx, image, token, cloud, os.Stderr)
	if err != nil {
		return err
	}
	if err := requireIdleQueue(ctx, url, apiToken); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Rolling out", pinned)
	err = replace(ctx, cloud, instance.Uuid, pinned, []string{"/lambda"})
	if err == nil {
		err = waitHealthy(ctx, url, apiToken)
	}
	if err != nil {
		// Keep the previous image and its startup contract recoverable even
		// when the new service fails validation.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rollbackErr := replace(rollbackCtx, cloud, instance.Uuid, strings.TrimPrefix(instance.Image, "oci://"), instance.Args)
		return errors.Join(fmt.Errorf("service rollout failed: %w", err), rollbackErr)
	}
	if err := waitState(ctx, cloud, instance.Uuid, platform.InstanceStateStandby); err != nil {
		return fmt.Errorf("service healthy but did not return to standby: %w", err)
	}
	if err := saveDeployment(instance.Uuid, name, metro, pinned, url); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"name": name, "image": pinned, "url": url, "state": "standby"})
}

func requireIdleQueue(ctx context.Context, url, token string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/jobs", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("queue check returned HTTP %d", response.StatusCode)
	}
	var jobs []struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Status == "queued" || job.Status == "running" {
			return errors.New("wait for the deployment queue to drain before releasing the service")
		}
	}
	return nil
}

func replace(ctx context.Context, cloud platform.Client, id, image string, args []string) error {
	stopped, err := cloud.StopInstanceByUUID(ctx, id, platform.StopInstanceByUUIDRequestBody{DrainTimeoutMs: new(uint64(0))})
	if err != nil {
		return err
	}
	if stopped.Status != platform.ResponseStatusSuccess {
		return errors.New("control-plane stop failed")
	}
	if err := waitState(ctx, cloud, id, platform.InstanceStateStopped); err != nil {
		return err
	}
	if len(args) == 0 {
		args = []string{"/lambda"}
	}
	for _, property := range []struct {
		prop  platform.MutableInstanceProperty
		value any
	}{
		{platform.MutableInstancePropertyImage, image}, {platform.MutableInstancePropertyArgs, args},
	} {
		updated, err := cloud.UpdateInstanceByUUID(ctx, id, platform.UpdateInstanceByUUIDRequestBody{Prop: property.prop, Op: platform.MutableInstanceOperationSet, Value: &property.value})
		if err != nil {
			return err
		}
		if updated.Status != platform.ResponseStatusSuccess || updated.Data == nil || len(updated.Data.Instances) != 1 || updated.Data.Instances[0].Status != platform.ResponseStatusSuccess {
			return errors.New("control-plane update failed")
		}
	}
	started, err := cloud.StartInstanceByUUID(ctx, id, platform.StartInstanceByUUIDRequestBody{})
	if err != nil {
		return err
	}
	if started.Status != platform.ResponseStatusSuccess {
		return errors.New("control-plane start failed")
	}
	return nil
}

func waitState(ctx context.Context, cloud platform.Client, id string, state platform.InstanceState) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		response, err := cloud.GetInstanceByUUID(ctx, id, platform.GetInstanceByUUIDOpts{})
		if err != nil {
			return err
		}
		if response.Data == nil || len(response.Data.Instances) != 1 {
			return errors.New("instance state unavailable")
		}
		if response.Data.Instances[0].State == state {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func waitHealthy(ctx context.Context, url, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func saveDeployment(id, name, metro, image, url string) error {
	path := "controlplane-deployment.json"
	record := map[string]any{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for key, value := range map[string]any{"uuid": id, "name": name, "metro": metro, "image": image, "url": url, "released_at": time.Now().UTC().Format(time.RFC3339)} {
		record[key] = value
	}
	data, err = json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
