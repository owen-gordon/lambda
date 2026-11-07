// release-dashboard deploys the static console on its own Unikraft instance.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		fmt.Fprintln(os.Stderr, "usage: scripts/release-dashboard.sh <image-reference>")
		os.Exit(2)
	}
	if err := release(ctx, os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func release(ctx context.Context, image string) error {
	token, metro, access := os.Getenv("UKC_TOKEN"), os.Getenv("UKC_METRO"), os.Getenv("LAMBDA_API_TOKEN")
	if token == "" || metro == "" || access == "" {
		return errors.New("export UKC_TOKEN, UKC_METRO, and LAMBDA_API_TOKEN")
	}
	cloud := platform.NewClient(platform.WithToken(token), platform.WithDefaultMetro(metro))
	api, err := get(ctx, cloud, "lambda-controlplane")
	if err != nil {
		return err
	}
	apiURL, err := endpoint(api)
	if err != nil {
		return err
	}
	if err := idleQueue(ctx, apiURL, access); err != nil {
		return err
	}
	pinned, err := controlplane.PublishDashboardImage(ctx, image, token, cloud, os.Stderr)
	if err != nil {
		return err
	}
	name := "lambda-dashboard"
	dash, err := get(ctx, cloud, name)
	if err != nil && !platform.ErrorContainsOnly(err, platform.APIHTTPErrorNotFound) {
		return err
	}
	hash := sha256.Sum256([]byte(access))
	env := map[string]string{"LAMBDA_ACCESS_TOKEN_SHA256": hex.EncodeToString(hash[:]), "LAMBDA_CONTROLPLANE_URL": apiURL, "GOMEMLIMIT": "64MiB"}
	if err != nil {
		created, err := cloud.CreateInstance(ctx, platform.CreateInstanceRequest{
			Name: &name, Image: platform.ImageReference(pinned), Autostart: new(false), MemoryMb: new(int64(128)), Vcpus: new(int32(1)), Env: env,
			Tags: []string{"lambda-dashboard"}, RestartPolicy: new(platform.InstanceRestartPolicyOnFailure),
			ScaleToZero:  &platform.CreateInstanceScaleToZero{Policy: new(platform.InstanceScaleToZeroPolicyIdle), Stateful: new(true), CooldownTimeMs: new(int32(2000))},
			ServiceGroup: &platform.CreateInstanceRequestServiceGroup{Services: []platform.Service{{Port: 443, DestinationPort: new(uint32(8080)), Protocol: new(platform.ServiceProtocol("tcp")), Handlers: []platform.ConnectionHandler{"tls", "http"}}}},
		})
		if err != nil {
			return err
		}
		if created.Status != platform.ResponseStatusSuccess || created.Data == nil || len(created.Data.Instances) != 1 {
			return errors.New("dashboard creation failed")
		}
		dash = created.Data.Instances[0]
	} else {
		owned := false
		for _, tag := range dash.Tags {
			if tag == "lambda-dashboard" {
				owned = true
			}
		}
		if !owned {
			return errors.New("refusing to replace an unrelated instance")
		}
		if err := stop(ctx, cloud, dash.Uuid); err != nil {
			return err
		}
		if err := update(ctx, cloud, dash.Uuid, platform.MutableInstancePropertyImage, platform.MutableInstanceOperationSet, pinned); err != nil {
			return err
		}
		if err := update(ctx, cloud, dash.Uuid, platform.MutableInstancePropertyEnv, platform.MutableInstanceOperationAdd, env); err != nil {
			return err
		}
		if err := update(ctx, cloud, dash.Uuid, platform.MutableInstancePropertyScaleToZero, platform.MutableInstanceOperationSet, map[string]any{"policy": "idle", "stateful": true, "cooldown_time_ms": 2000}); err != nil {
			return err
		}
	}
	dashURL, err := endpoint(dash)
	if err != nil {
		return err
	}
	if api.Env["LAMBDA_DASHBOARD_ORIGIN"] != dashURL {
		if err := idleQueue(ctx, apiURL, access); err != nil {
			return err
		}
		if err := stop(ctx, cloud, api.Uuid); err != nil {
			return err
		}
		if err := update(ctx, cloud, api.Uuid, platform.MutableInstancePropertyEnv, platform.MutableInstanceOperationAdd, map[string]string{"LAMBDA_DASHBOARD_ORIGIN": dashURL}); err != nil {
			return err
		}
		if err := start(ctx, cloud, api.Uuid); err != nil {
			return err
		}
		if err := healthy(ctx, apiURL, access); err != nil {
			return err
		}
		if err := wait(ctx, cloud, api.Uuid, platform.InstanceStateStandby); err != nil {
			return err
		}
	}
	if err := start(ctx, cloud, dash.Uuid); err != nil {
		return err
	}
	if err := healthy(ctx, dashURL, access); err != nil {
		return err
	}
	if err := wait(ctx, cloud, dash.Uuid, platform.InstanceStateStandby); err != nil {
		return err
	}
	record := map[string]any{"uuid": dash.Uuid, "name": name, "metro": metro, "image": pinned, "url": dashURL, "controlplane_url": apiURL, "memory_mb": 128, "scale_to_zero": map[string]any{"policy": "idle", "stateful": true, "cooldown_time_ms": 2000}, "released_at": time.Now().UTC().Format(time.RFC3339)}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile("dashboard-deployment.json", append(data, '\n'), 0600); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(record)
}

func get(ctx context.Context, c platform.Client, name string) (platform.Instance, error) {
	r, err := c.GetInstances(ctx, []platform.NameOrUUID{{Name: &name}}, platform.GetInstancesOpts{})
	if err != nil {
		return platform.Instance{}, err
	}
	if r.Status != platform.ResponseStatusSuccess || r.Data == nil || len(r.Data.Instances) != 1 || r.Data.Instances[0].Uuid == "" {
		return platform.Instance{}, errors.New("instance unavailable")
	}
	return r.Data.Instances[0], nil
}
func endpoint(i platform.Instance) (string, error) {
	if i.ServiceGroup == nil || len(i.ServiceGroup.Domains) == 0 {
		return "", errors.New("instance has no public domain")
	}
	return "https://" + strings.TrimSuffix(i.ServiceGroup.Domains[0].Fqdn, "."), nil
}
func update(ctx context.Context, c platform.Client, id string, prop platform.MutableInstanceProperty, op platform.MutableInstanceOperation, value any) error {
	r, err := c.UpdateInstanceByUUID(ctx, id, platform.UpdateInstanceByUUIDRequestBody{Prop: prop, Op: op, Value: &value})
	if err != nil {
		return err
	}
	if r.Status != platform.ResponseStatusSuccess || r.Data == nil || len(r.Data.Instances) != 1 || r.Data.Instances[0].Status != platform.ResponseStatusSuccess {
		return errors.New("instance update failed")
	}
	return nil
}
func stop(ctx context.Context, c platform.Client, id string) error {
	r, err := c.GetInstanceByUUID(ctx, id, platform.GetInstanceByUUIDOpts{})
	if err != nil {
		return err
	}
	if r.Data == nil || len(r.Data.Instances) != 1 {
		return errors.New("instance unavailable")
	}
	if r.Data.Instances[0].State == platform.InstanceStateStopped {
		return nil
	}
	if _, err := c.StopInstanceByUUID(ctx, id, platform.StopInstanceByUUIDRequestBody{DrainTimeoutMs: new(uint64(0))}); err != nil {
		return err
	}
	return wait(ctx, c, id, platform.InstanceStateStopped)
}
func start(ctx context.Context, c platform.Client, id string) error {
	_, err := c.StartInstanceByUUID(ctx, id, platform.StartInstanceByUUIDRequestBody{})
	return err
}
func wait(ctx context.Context, c platform.Client, id string, state platform.InstanceState) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		r, err := c.GetInstanceByUUID(ctx, id, platform.GetInstanceByUUIDOpts{})
		if err != nil {
			return err
		}
		if r.Data == nil || len(r.Data.Instances) != 1 {
			return errors.New("instance unavailable")
		}
		if r.Data.Instances[0].State == state {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
func healthy(ctx context.Context, origin, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		r, err := http.NewRequestWithContext(ctx, "GET", origin+"/healthz", nil)
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(r)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
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
func idleQueue(ctx context.Context, origin, token string) error {
	r, err := http.NewRequestWithContext(ctx, "GET", origin+"/v1/jobs", nil)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("cannot inspect deployment queue")
	}
	var jobs []struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Status == "running" || job.Status == "queued" {
			return errors.New("wait for the deployment queue to drain")
		}
	}
	return nil
}
