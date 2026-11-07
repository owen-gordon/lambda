package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"unikraft.com/cloud/sdk/platform"
)

type diagnosticCloud struct {
	fakeCloud
	output string
	err    error
}

func (c *diagnosticCloud) GetInstanceLogsByUUID(context.Context, string, platform.GetInstanceLogsByUUIDRequestBody) (*platform.Response[platform.GetInstancesLogsResponseData], error) {
	return &platform.Response[platform.GetInstancesLogsResponseData]{Data: &platform.GetInstancesLogsResponseData{Instances: []platform.GetInstancesLogsResponseLoggedInstance{{Uuid: testUUID, Output: base64.StdEncoding.EncodeToString([]byte(c.output))}}}}, c.err
}

func TestReadinessMemoryDiagnosisDoesNotLeakLogs(t *testing.T) {
	c := &diagnosticCloud{output: "private application token\n[ 0.07] Kernel panic - not syncing: System is deadlocked on memory\n"}
	p := pipeline{Cloud: c}
	original := context.DeadlineExceeded
	err := p.readinessFailure(context.Background(), &deployment{UUID: testUUID, MemoryMB: 128}, original)
	if !strings.Contains(err.Error(), "increase memory_mb") || !strings.Contains(err.Error(), "128 MiB") || !errors.Is(err, original) || strings.Contains(err.Error(), "private application token") {
		t.Fatalf("incorrect or leaking diagnosis: %v", err)
	}
	c.output = "[ 0.03] PCI: Fatal: No config space access function found"
	if err := p.readinessFailure(context.Background(), &deployment{UUID: testUUID}, original); err != original {
		t.Fatal("mistook PCI boot message for memory failure")
	}
	c.err = errors.New("logs unavailable")
	if err := p.readinessFailure(context.Background(), &deployment{UUID: testUUID}, original); err != original {
		t.Fatal("masked readiness failure with log error")
	}
}
