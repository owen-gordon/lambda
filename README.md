# Lambda

A hosted service that turns a Python function into an HTTPS endpoint on Unikraft
Cloud. Submit source and configuration; the service builds and pushes an image,
deploys it by digest, and returns its URL.

The API and dashboard are deployed to your own Unikraft Cloud account. Keep
your deployment URLs in the ignored `.endpoints.env` file; see
[configuration](docs/configuration.md).

## Browser console

Open `https://YOUR_DASHBOARD_FQDN/?token=YOUR_TOKEN`,
replacing `YOUR_TOKEN` with `LAMBDA_API_TOKEN` from `.controlplane.env`.
Missing or invalid query tokens return 401 before the page is served.

The plain HTML console builds functions, shows jobs and endpoints, inspects and
deletes instances, and invokes functions with a JSON request. See the
[dashboard guide](docs/dashboard.md).

## Build and deploy

Write a single Python file with `handler(event)`. The JSON request body becomes
`event`; the return value becomes the JSON response. Start with `examples/hello.py`.

From this directory:

```sh
source .controlplane.env
source .endpoints.env
API=$LAMBDA_API_URL
umask 077

jq --rawfile source examples/hello.py '.source = $source' \
  examples/build-spec.json > build.json

curl --fail-with-body -sS "$API/v1/deployments" \
  -H "Authorization: Bearer $LAMBDA_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @build.json > submission.json
```

Edit `examples/build-spec.json` to choose the image name, env vars, memory, and
access policy. Add `"metro": "fra"` to deploy to another metro; omission defaults
to Dallas. The dashboard provides a metro selector. This request builds, pushes, **and deploys** the function. It
returns `202` with a job ID and invocation token. Keep `submission.json`: the
invocation token is returned once.

Check progress:

```sh
JOB_ID=$(jq -er '.job.id' submission.json)
curl --fail-with-body -sS "$API/v1/jobs/$JOB_ID" \
  -H "Authorization: Bearer $LAMBDA_API_TOKEN" > job.json
jq '{status, phase, error, deployment}' job.json
```

Repeat that status request until `status` is `succeeded` or `failed`.
A successful job contains `deployment.image` (the exact image digest) and
`deployment.url` (the function endpoint).

Invoke it:

```sh
FUNCTION_URL=$(jq -er '.deployment.url' job.json)
INVOCATION_TOKEN=$(jq -er '.invocation_token' submission.json)
curl --fail-with-body -sS "$FUNCTION_URL/" \
  -H "Authorization: Bearer $INVOCATION_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Owen"}'
```

To remove it:

```sh
curl --fail-with-body -sS -X DELETE "$API/v1/deployments/$JOB_ID" \
  -H "Authorization: Bearer $LAMBDA_API_TOKEN"
```

Functions are private by default. Set `auth.mode` to `public` for an unauthenticated
endpoint. Env vars are injected into the instance at runtime and stay out of the
image. The control plane sleeps when idle and holds a wake lock while work is
queued or running.

This version supports one Python file and the standard library. It does not
install pip dependencies. See the [API contract](docs/api.md) for limits and
management routes.

## Code

- `main.go`: service entrypoint.
- `internal/controlplane/`: HTTP API, queue, encrypted state, image builder, and deployment worker.
- `internal/controlplane/runtime/`: embedded Python HTTP adapter and image template.
- `internal/dashboard/`: static console and query-token gate.
- `deploy/controlplane/` and `deploy/dashboard/`: separate service image definitions.
- `cmd/release-controlplane/` and `scripts/release-controlplane.sh`: maintainer release tooling.
- `examples/`: function source and request spec.

Tests:

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s internal/controlplane/runtime -p 'test_*.py'
```

Service releases and infrastructure configuration are in the
[operations guide](docs/operations.md).
