package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseRequiresAnAuthenticatedIdleQueue(t *testing.T) {
	for _, state := range []string{"queued", "running", "succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/jobs" || r.Header.Get("Authorization") != "Bearer operator-token" {
					t.Error("bad queue check")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"status":"` + state + `"}]`))
			}))
			defer server.Close()
			err := requireIdleQueue(context.Background(), server.URL, "operator-token")
			if (err != nil) != (state == "queued" || state == "running") {
				t.Fatalf("unsafe release decision for %s: %v", state, err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	if err := requireIdleQueue(context.Background(), server.URL, "wrong-token"); err == nil {
		t.Fatal("released without permission to inspect the queue")
	}
}
