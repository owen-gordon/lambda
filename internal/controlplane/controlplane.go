package controlplane

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"unikraft.com/cloud/sdk/platform"
)

type deployRunner func(context.Context, buildSpec, string, io.Writer) (*deployment, error)

type instanceManager interface {
	Inspect(context.Context, string) (*deployment, error)
	Delete(context.Context, string) error
}

type controlPlane struct {
	store           *jobStore
	run             deployRunner
	instances       instanceManager
	managerForMetro func(string) instanceManager
	listMetros      func(context.Context) ([]metro, error)
	metro           string
	tokenHash       [32]byte
	redact          []string
	wake            chan struct{}
	manageMu        sync.Mutex
	workMu          sync.Mutex
	awake           *awakeGate
	queueRelease    func() error
	stopping        bool
	failures        chan error
	dashboardOrigin string
	invokeTransport http.RoundTripper
	beforeInvoke    func(context.Context) error
}

func newControlPlane(store *jobStore, token, metro string, runner deployRunner, manager instanceManager, redact ...string) (*controlPlane, error) {
	if !validSecret(token) {
		return nil, errors.New("LAMBDA_API_TOKEN must be 32 to 256 bearer-token characters")
	}
	for _, job := range store.list() {
		if !metroCode.MatchString(job.Metro) {
			return nil, errors.New("stored job has an invalid metro")
		}
	}
	return &controlPlane{store: store, run: runner, instances: manager, metro: metro, tokenHash: sha256.Sum256([]byte(token)), redact: append(redact, token), wake: make(chan struct{}, 1), awake: &awakeGate{}, failures: make(chan error, 1)}, nil
}

func (c *controlPlane) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/deployments", c.submit)
	mux.HandleFunc("GET /v1/metros", c.getMetros)
	mux.HandleFunc("POST /v1/deployments/{id}/invoke", c.invoke)
	mux.HandleFunc("POST /v1/deployments/{id}/health", c.checkHealth)
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, c.store.list()) })
	mux.HandleFunc("GET /v1/jobs/{id}", c.getJob)
	mux.HandleFunc("DELETE /v1/jobs/{id}", c.removeJob)
	mux.HandleFunc("GET /v1/deployments/{id}", c.inspect)
	mux.HandleFunc("DELETE /v1/deployments/{id}", c.deleteInstance)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !c.browserAccess(w, r) {
			return
		}
		headers := r.Header.Values("Authorization")
		var candidate string
		if len(headers) == 1 {
			parts := strings.SplitN(headers[0], " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				candidate = parts[1]
			}
		}
		hash := sha256.Sum256([]byte(candidate))
		if subtle.ConstantTimeCompare(hash[:], c.tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (c *controlPlane) submit(w http.ResponseWriter, r *http.Request) {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		apiError(w, 415, "expected application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var spec buildSpec
	if err := decoder.Decode(&spec); err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			apiError(w, 413, "request exceeds 2 MiB")
		} else {
			apiError(w, 400, "invalid build spec JSON or unknown fields")
		}
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		apiError(w, 400, "request must contain exactly one JSON object")
		return
	}
	if err := spec.normalize(); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if !c.selectMetro(w, r, &spec) {
		return
	}
	job, err := c.enqueue(spec)
	if err != nil {
		if errors.Is(err, errQueueFull) || errors.Is(err, errJobLimit) {
			apiError(w, 429, err.Error())
		} else {
			apiError(w, 503, "cannot accept deployment job")
		}
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	w.Header().Set("Location", "/v1/jobs/"+job.Job.ID)
	writeJSON(w, http.StatusAccepted, struct {
		Job             Job    `json:"job"`
		InvocationToken string `json:"invocation_token,omitempty"`
	}{Job: job.Job, InvocationToken: spec.Auth.Token})
}

func (c *controlPlane) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := c.store.get(r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "job not found")
		return
	}
	writeJSON(w, 200, job.Job)
}

func (c *controlPlane) removeJob(w http.ResponseWriter, r *http.Request) {
	c.manageMu.Lock()
	defer c.manageMu.Unlock()
	if err := c.store.remove(r.PathValue("id")); err != nil {
		if errors.Is(err, errJobNotFound) {
			apiError(w, 404, "job not found")
		} else {
			apiError(w, 409, "finish the job and delete its instance before removing the record")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *controlPlane) manageableJob(w http.ResponseWriter, r *http.Request) (storedJob, bool) {
	job, err := c.store.get(r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "job not found")
		return storedJob{}, false
	}
	if job.Job.Status == "running" || job.Job.Status == "queued" {
		apiError(w, 409, "deployment job has not finished")
		return storedJob{}, false
	}
	if job.Job.DeletedAt != nil {
		apiError(w, 410, "instance was deleted")
		return storedJob{}, false
	}
	return job, true
}

func (c *controlPlane) inspect(w http.ResponseWriter, r *http.Request) {
	c.manageMu.Lock()
	defer c.manageMu.Unlock()
	job, ok := c.manageableJob(w, r)
	if !ok {
		return
	}
	id := job.Job.Name
	if job.Job.Deployment != nil && job.Job.Deployment.UUID != "" {
		id = job.Job.Deployment.UUID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	record, err := c.manager(job.Job.Metro).Inspect(ctx, id)
	if err != nil {
		status := http.StatusBadGateway
		if platform.ErrorContainsOnly(err, platform.APIHTTPErrorNotFound) {
			status = http.StatusNotFound
		}
		apiError(w, status, redactMessage(err.Error(), job.Spec, c.redact))
		return
	}
	if err := c.store.update(job.Job.ID, func(job *storedJob) { job.Job.Deployment = record }); err != nil {
		apiError(w, 500, "cannot persist inspection result")
		return
	}
	writeJSON(w, 200, record)
}

func (c *controlPlane) deleteInstance(w http.ResponseWriter, r *http.Request) {
	c.manageMu.Lock()
	defer c.manageMu.Unlock()
	job, err := c.store.get(r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "job not found")
		return
	}
	if job.Job.DeletedAt != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	job, ok := c.manageableJob(w, r)
	if !ok {
		return
	}
	id := job.Job.Name
	if job.Job.Deployment != nil && job.Job.Deployment.UUID != "" {
		id = job.Job.Deployment.UUID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	if err := c.manager(job.Job.Metro).Delete(ctx, id); err != nil && !platform.ErrorContainsOnly(err, platform.APIHTTPErrorNotFound) {
		apiError(w, 502, redactMessage(err.Error(), job.Spec, c.redact))
		return
	}
	if err := c.store.update(job.Job.ID, func(job *storedJob) {
		job.Job.DeletedAt = new(time.Now().UTC())
		if job.Job.Deployment != nil {
			job.Job.Deployment.State = "deleted"
			job.Job.Deployment.Ready = new(false)
		}
	}); err != nil {
		apiError(w, 500, "instance deleted but could not persist deletion result")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Worker runs exactly one deployment at a time. Accepted requests are durable
// before entering the queue, so request cancellation does not cancel deployment.
func (c *controlPlane) Worker(ctx context.Context) (workerErr error) {
	defer func() { workerErr = errors.Join(workerErr, c.stopQueue()) }()
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case err := <-c.failures:
			return err
		default:
		}
		job, err := c.claimProtected()
		if err != nil {
			return fmt.Errorf("claim job: %w", err)
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-c.wake:
			case err := <-c.failures:
				return err
			}
			continue
		}
		if err := c.execute(ctx, *job); err != nil {
			return err
		}
	}
}

func (c *controlPlane) execute(ctx context.Context, job storedJob) error {
	// A bounded job context prevents a stalled builder from blocking the queue
	// indefinitely. It does not impose an invocation execution limit.
	jobCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	progress := &jobProgress{store: c.store, id: job.Job.ID}
	spec := job.Spec
	spec.Metro = job.Job.Metro // Legacy jobs did not persist metro inside the spec.
	record, runErr := c.run(jobCtx, spec, job.Job.Name, progress)
	if progress.err != nil {
		return fmt.Errorf("persist job progress: %w", progress.err)
	}
	return c.store.update(job.Job.ID, func(stored *storedJob) {
		stored.Job.Deployment = record
		stored.Job.FinishedAt = new(time.Now().UTC())
		stored.Job.Status, stored.Job.Phase = "succeeded", "ready"
		if runErr != nil {
			stored.Job.Status, stored.Job.Phase = "failed", "failed"
			stored.Job.Error = redactMessage(runErr.Error(), job.Spec, c.redact)
		}
		if ctx.Err() != nil {
			stored.Job.Status, stored.Job.Phase = "interrupted", "interrupted"
			stored.Job.Error = "server stopped during deployment; inspect the instance before retrying"
		}
	})
}

type jobProgress struct {
	store *jobStore
	id    string
	err   error
}

func (p *jobProgress) Write(data []byte) (int, error) {
	phase := ""
	message := string(data)
	switch {
	case strings.HasPrefix(message, "Waiting for builder capacity"):
		phase = "waiting_for_capacity"
	case strings.HasPrefix(message, "Building image"):
		phase = "building"
	case strings.HasPrefix(message, "Waiting for builder"):
		phase = "waiting_for_builder"
	case strings.HasPrefix(message, "Pushing image"):
		phase = "pushing"
	case strings.HasPrefix(message, "Creating instance"):
		phase = "deploying"
	case strings.HasPrefix(message, "Created "):
		phase = "waiting_for_readiness"
	}
	if phase != "" && p.err == nil {
		p.err = p.store.update(p.id, func(job *storedJob) { job.Job.Phase = phase })
	}
	return len(data), p.err
}

func redactMessage(message string, spec buildSpec, secrets []string) string {
	values := append([]string{}, secrets...)
	values = append(values, spec.Auth.Token, spec.Source)
	for _, value := range spec.Env {
		values = append(values, value)
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		message = strings.ReplaceAll(message, value, "[redacted]")
		encoded, _ := json.Marshal(value)
		message = strings.ReplaceAll(message, string(encoded[1:len(encoded)-1]), "[redacted]")
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	return message
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func apiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
