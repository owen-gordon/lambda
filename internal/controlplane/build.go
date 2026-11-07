package controlplane

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"unikraft.com/cloud/sdk/platform"
	"unikraft.com/x/build"
	"unikraft.com/x/build/registryauth"
	"unikraft.com/x/buildkit"
	imagespec "unikraft.com/x/image-spec"
)

func buildImage(ctx context.Context, directory, output, token string, cloud platform.Client, progress io.Writer) (build.Saved, error) {
	// Leave 256 MiB of the free tier's 1 GiB total storage quota for state.
	cacheMB := uint64(768)
	if value := os.Getenv("LAMBDA_BUILDER_CACHE_MB"); value != "" {
		var err error
		cacheMB, err = strconv.ParseUint(value, 10, 64)
		if err != nil || cacheMB == 0 {
			return build.Saved{}, fmt.Errorf("LAMBDA_BUILDER_CACHE_MB must be a positive integer")
		}
	}
	// Read the generated image build context.
	opts, err := build.LoadBuildOpts(ctx, directory)
	if err != nil {
		return build.Saved{}, fmt.Errorf("load build options: %w", err)
	}
	if err := opts.FilterArch("x86_64"); err != nil {
		return build.Saved{}, fmt.Errorf("select architecture: %w", err)
	}

	dest, err := imagespec.GuessLocation(output)
	if err != nil {
		return build.Saved{}, fmt.Errorf("parse output: %w", err)
	}

	// Use the Cloud token for registry pulls and pushes.
	creds, err := registryauth.FromToken(token)
	if err != nil {
		return build.Saved{}, fmt.Errorf("registry credentials: %w", err)
	}

	hosts := docker.ConfigureDefaultRegistries(
		docker.WithAuthorizer(creds.Authorizer()),
	)
	accessor := imagespec.NewAccessor(
		imagespec.WithResolver(docker.NewResolver(docker.ResolverOptions{
			Hosts: hosts,
		})),
		imagespec.WithRegistryHosts(hosts),
	)

	backend := buildkit.NewPlatform(cloud,
		buildkit.WithVCPUs(1),
		buildkit.WithMemory(2048),
		buildkit.WithCacheSize(cacheMB),
	)

	daemon, err := acquireBuilder(ctx, progress, func(ctx context.Context) (*buildkit.Daemon, error) {
		return backend.Acquire(ctx, buildkit.Key{Name: builderKey, MemoryMB: 2048, CacheMB: cacheMB})
	})

	if err != nil {
		return build.Saved{}, fmt.Errorf("connect to Docker BuildKit: %w", err)
	}
	defer daemon.Release()

	builder := build.Builder{
		Images: func(context.Context) (*imagespec.Accessor, error) {
			return accessor, nil
		},
		Buildkit: func(context.Context) (*bkclient.Client, func(), error) {
			// The deferred Release above owns connection cleanup.
			return daemon.Client, nil, nil
		},
		Session: func(context.Context) ([]session.Attachable, error) {
			return []session.Attachable{creds.Attachable()}, nil
		},
		// Run filesystem packing inside BuildKit.
		Packer: build.DefaultPacker,
	}

	fmt.Fprintln(progress, "Building image...")
	images, err := builder.Build(ctx, opts)
	if err != nil && strings.Contains(err.Error(), "no space left on device") && ctx.Err() == nil {
		fmt.Fprintln(progress, "Builder disk is full; clearing unused build cache and retrying...")
		// Include unused source images left by earlier builds. In-use records
		// are preserved, even with PruneAll.
		if pruneErr := daemon.Client.Prune(ctx, nil, bkclient.PruneAll); pruneErr != nil {
			return build.Saved{}, fmt.Errorf("build image: %w; clearing unused cache: %v", err, pruneErr)
		}
		images, err = builder.Build(ctx, opts)
	}
	if err != nil {
		return build.Saved{}, fmt.Errorf("build image: %w", err)
	}
	defer func() {
		for _, img := range images {
			img.Close()
		}
	}()

	fmt.Fprintln(progress, "Pushing image...")
	saved, err := builder.Save(ctx, dest, images...)
	if err != nil {
		return build.Saved{}, fmt.Errorf("push image: %w", err)
	}

	fmt.Fprintf(progress, "Pushed %s\nDigest: %s\n", saved.Ref, saved.Digest)
	return saved, nil
}
