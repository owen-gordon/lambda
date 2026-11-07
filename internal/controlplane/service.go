package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"unikraft.com/cloud/sdk/platform"
	"unikraft.com/x/build"
)

// serviceConfig contains runtime configuration for the hosted service.
type serviceConfig struct {
	Listen          string
	StateDir        string
	MaxPending      int
	MaxJobs         int
	Token           string
	Metro           string
	APIToken        string
	StateKeyText    string
	StateKey        []byte
	DashboardOrigin string
}

func loadServiceConfig() (serviceConfig, error) {
	cfg := serviceConfig{
		Listen: ":8080", StateDir: "/data/jobs", MaxPending: 8, MaxJobs: 64,
		Token: os.Getenv("UKC_TOKEN"), Metro: os.Getenv("UKC_METRO"),
		APIToken: os.Getenv("LAMBDA_API_TOKEN"), StateKeyText: os.Getenv("LAMBDA_STATE_KEY"),
	}
	if cfg.Token == "" || cfg.Metro == "" {
		return cfg, errors.New("UKC_TOKEN and UKC_METRO are required")
	}
	if !validSecret(cfg.APIToken) {
		return cfg, errors.New("LAMBDA_API_TOKEN must be a random bearer token of 32 to 256 characters")
	}
	key, err := base64.StdEncoding.DecodeString(cfg.StateKeyText)
	if err != nil || len(key) != 32 {
		return cfg, errors.New("LAMBDA_STATE_KEY must be a base64-encoded 32-byte key")
	}
	cfg.StateKey = key
	cfg.DashboardOrigin, err = dashboardOrigin(os.Getenv("LAMBDA_DASHBOARD_ORIGIN"))
	if err != nil {
		return cfg, err
	}
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"LAMBDA_MAX_PENDING_JOBS", &cfg.MaxPending}, {"LAMBDA_MAX_JOBS", &cfg.MaxJobs},
	} {
		if value := os.Getenv(setting.name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return cfg, fmt.Errorf("%s must be a positive integer", setting.name)
			}
			*setting.target = n
		}
	}
	if cfg.MaxJobs < cfg.MaxPending {
		return cfg, errors.New("LAMBDA_MAX_JOBS must be at least LAMBDA_MAX_PENDING_JOBS")
	}
	return cfg, nil
}

// Run starts the service inside Unikraft Cloud. Clients submit work over HTTPS.
func Run(ctx context.Context, log io.Writer) error {
	cfg, err := loadServiceConfig()
	if err != nil {
		return err
	}
	cloud := platform.NewClient(platform.WithToken(cfg.Token), platform.WithDefaultMetro(cfg.Metro))
	factory := func(progress io.Writer) pipeline {
		return pipeline{
			Cloud: cloud, Metro: cfg.Metro, Progress: progress,
			ForMetro: func(metro string) instanceClient { return cloud.WithMetro(metro) },
			Build: func(ctx context.Context, directory, output string) (build.Saved, error) {
				return buildImage(ctx, directory, output, cfg.Token, cloud, progress)
			},
			AfterBuild: func(ctx context.Context) error {
				idleCtx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				return waitBuilderIdle(idleCtx, cloud, builderInstanceName)
			},
		}
	}
	return runService(ctx, cfg, factory, log)
}

func runService(ctx context.Context, cfg serviceConfig, factory func(io.Writer) pipeline, stderr io.Writer) error {
	awake, err := openAwakeGate(scaleToZeroDisablePath)
	if err != nil {
		return err
	}
	startupRelease, err := awake.hold()
	if err != nil {
		return err
	}
	defer startupRelease()
	store, err := openJobStore(cfg.StateDir, cfg.StateKey, cfg.MaxPending, cfg.MaxJobs)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := os.MkdirAll(os.TempDir(), 0700); err != nil {
		return fmt.Errorf("prepare temporary directory: %w", err)
	}
	api, err := newControlPlane(store, cfg.APIToken, cfg.Metro, func(ctx context.Context, spec buildSpec, name string, progress io.Writer) (*deployment, error) {
		return factory(progress).inMetro(spec.Metro).Deploy(ctx, spec, name)
	}, factory(io.Discard), cfg.Token, cfg.StateKeyText)
	if err != nil {
		return err
	}
	api.managerForMetro = func(metro string) instanceManager { return factory(io.Discard).inMetro(metro) }
	api.listMetros = cloudMetros(cfg.Token)
	api.awake = awake
	api.dashboardOrigin = cfg.DashboardOrigin
	if cfg.DashboardOrigin != "" {
		api.beforeInvoke = func(ctx context.Context) error { return waitDashboardIdle(ctx, factory(io.Discard).Cloud) }
	}
	if err := api.protectPending(); err != nil {
		return err
	}
	defer api.stopQueue()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := startupRelease(); err != nil {
		return err
	}
	server := &http.Server{
		Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan error, 1)
	go func() { workerDone <- api.Worker(workerCtx) }()
	httpDone := make(chan error, 1)
	go func() {
		httpDone <- server.Serve(listener)
	}()
	fmt.Fprintf(stderr, "Control plane listening on %s in %s; serial build worker ready\n", listener.Addr(), cfg.Metro)
	var result error
	workerFinished := false
	select {
	case <-ctx.Done():
	case err := <-workerDone:
		result, workerFinished = err, true
	case err := <-httpDone:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	stopWorker()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		result = errors.Join(result, err)
	}
	if !workerFinished {
		select {
		case err := <-workerDone:
			result = errors.Join(result, err)
		case <-shutdownCtx.Done():
			result = errors.Join(result, errors.New("worker shutdown timed out; interrupted job will be recovered on restart"))
		}
	}
	return result
}
