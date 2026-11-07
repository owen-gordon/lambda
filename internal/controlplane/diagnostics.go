package controlplane

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"unikraft.com/cloud/sdk/platform"
)

// Only return a fixed diagnosis, never raw application logs: they can contain
// source, environment values, or invocation credentials.
func (p pipeline) readinessFailure(ctx context.Context, record *deployment, original error) error {
	logs, ok := p.Cloud.(interface {
		GetInstanceLogsByUUID(context.Context, string, platform.GetInstanceLogsByUUIDRequestBody) (*platform.Response[platform.GetInstancesLogsResponseData], error)
	})
	if !ok || record == nil || record.UUID == "" || ctx.Err() != nil {
		return original
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := logs.GetInstanceLogsByUUID(ctx, record.UUID, platform.GetInstanceLogsByUUIDRequestBody{Offset: new(int64(-16384)), Limit: new(int64(16384))})
	if err != nil || response == nil || response.Data == nil {
		return original
	}
	for _, item := range response.Data.Instances {
		if item.Uuid != record.UUID {
			continue
		}
		output, err := base64.StdEncoding.DecodeString(item.Output)
		if err != nil {
			continue
		}
		if strings.Contains(string(output), "Kernel panic - not syncing: System is deadlocked on memory") {
			return fmt.Errorf("instance exhausted memory during startup at %d MiB; increase memory_mb: %w", record.MemoryMB, original)
		}
	}
	return original
}
