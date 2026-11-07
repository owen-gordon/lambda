package controlplane

import (
	"context"
	"errors"
	"io"
	"time"

	"unikraft.com/cloud/sdk/platform"
	imagespec "unikraft.com/x/image-spec"
	"unikraft.com/x/image-spec/schemes"
)

// PublishServiceImage is used only by the control-plane release tool. Function
// images are built by the hosted job worker from buildSpec.Source.
func PublishServiceImage(ctx context.Context, image, token string, cloud platform.Client, progress io.Writer) (string, error) {
	return publishImage(ctx, "deploy/controlplane", image, token, cloud, progress)
}

// PublishDashboardImage publishes the static dashboard VM's image.
func PublishDashboardImage(ctx context.Context, image, token string, cloud platform.Client, progress io.Writer) (string, error) {
	return publishImage(ctx, "deploy/dashboard", image, token, cloud, progress)
}

func publishImage(ctx context.Context, directory, image, token string, cloud platform.Client, progress io.Writer) (string, error) {
	location, err := imagespec.GuessLocation(image)
	if err != nil || image == "" || location.Scheme != schemes.OCI {
		return "", errors.New("release image must be a registry reference")
	}
	saved, err := buildImage(ctx, directory, image, token, cloud, progress)
	if err != nil {
		return "", err
	}
	idleCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := waitBuilderIdle(idleCtx, cloud, builderInstanceName); err != nil {
		return "", err
	}
	return pinnedImage(saved)
}
