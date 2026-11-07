# Hosted service operations

The control plane runs as `lambda-controlplane` in your home metro. Its endpoint
is assigned by Unikraft Cloud.
`controlplane-deployment.json` contains its image digest, instance UUID, volume,
and verification results. `.controlplane.env` holds the existing admin and
state-encryption keys; both files are ignored by Git.

## Service configuration

The image starts `/lambda` with no commands or flags. TLS terminates at the Cloud
edge on port 443 and forwards HTTP to port 8080. The service uses a persistent
volume mounted at `/data`, with job state in `/data/jobs` and `TMPDIR=/data/tmp`.
The native Unikraft scale-to-zero interface is required at startup.

Runtime environment:

| Variable | Purpose |
| --- | --- |
| `UKC_TOKEN` | Cloud API and registry credentials; required |
| `UKC_METRO` | Home/builder metro and default deployment metro; required |
| `LAMBDA_API_TOKEN` | Admin bearer token; required |
| `LAMBDA_STATE_KEY` | Base64-encoded 32-byte state encryption key; required |
| `LAMBDA_DASHBOARD_ORIGIN` | Optional exact HTTPS origin permitted for browser API requests |
| `TMPDIR` | Writable scratch directory; deployed as `/data/tmp` |
| `LAMBDA_MAX_PENDING_JOBS` | Queued plus running jobs; default 8 |
| `LAMBDA_MAX_JOBS` | Stored job records; default 64 |
| `LAMBDA_BUILDER_CACHE_MB` | Remote builder cache size; default 768 MiB |

Keep the state key stable across releases. Replacing it makes existing records
unreadable. The API token can rotate independently. Invocation tokens are
separate credentials belonging to individual functions. Credentials are
injected into instances and must not be added to image definitions.

## Release the control plane

Run from the repository with the operator's credentials exported:

```sh
set -a
source .env
source .controlplane.env
set +a
export UKC_METRO=dal
scripts/release-controlplane.sh YOUR_NAMESPACE/lambda-controlplane:<version>
```

This compiles the static service binary, packages its CA bundle, uses the remote
builder to publish the service image, and replaces the existing instance by
immutable digest. It preserves the state volume, runtime credentials, domain,
and scale-to-zero policy. It verifies health and waits for standby before
returning. The queue must be empty; schedule releases while submissions are
paused. A failed health check restores the previous image and startup arguments.

`deploy/controlplane/Kraftfile` defines the image. The dedicated release tool is
for maintainers updating the Go service; function authors use the hosted API.

## Persistence

Job records use AES-256-GCM with atomic replacement and fsync. State files are
mode 0600; their directory is 0700. Temporary build contexts contain source and
are removed when the job exits. Runtime env values and invocation credentials
are excluded from those build contexts and public job responses. Functions can
read and log their own env, so application logging remains their author's
responsibility.

An exclusive file lock permits one service process per state directory. Queued
jobs resume on restart. Running jobs become `interrupted` and are not replayed,
since an instance may already exist. Use the API to inspect or delete those jobs'
instances before retrying. Each job retains its deployment metro; queued jobs and management operations
continue using that target after restart. Older jobs retain their saved metro
without migration. Run one replica against the persistent volume.

## Scale to zero

The control plane uses `policy=idle,stateful=true,cooldown-time=2000`. It sleeps
when idle and resumes on the next request. A kernel counter hold protects startup
and all queued/running work. The hold is acquired before persisting submissions
and released only after the last job's terminal result is saved. Concurrent
submission and queue draining share a mutex. Rejected submissions, failed jobs,
and shutdown release their holds; coordination I/O failure stops the service.

Cloud may show effective `enabled: false` while the configured policy stays
`idle` during a build. That is the temporary wake lock. Inactive keep-alive
connections do not pin the instance awake. Cloud API inspection does not wake
the service, making it suitable for checking standby. See
[Unikraft's coordination interface](https://unikraft.com/docs/features/scale-to-zero#application-support-for-scale-to-zero).

## Current resource budget

- Control plane: 1 vCPU, 512 MiB RAM, 256 MiB persistent state volume.
- Dashboard: 1 vCPU, 128 MiB RAM, no volume, two-second idle cooldown.
- Builder: 1 vCPU, 2048 MiB RAM, 768 MiB cache volume.
- Function default: 1 vCPU, 128 MiB RAM, one-second scale-to-zero cooldown.
- Account: 1 GiB total volume storage, 4 GiB live RAM, two live vCPUs, five instances.

The builder must reach standby before the next function starts. Other active
functions can compete with builds for the remaining CPU. Cache volumes cannot
be resized in place; a size change requires recreating the cache. Existing
builder size mismatches are reported rather than migrated during function builds.

Local `lambda-state` backups are ignored and must never be committed.

## Release the dashboard

With the same credentials exported as above:

```sh
scripts/release-dashboard.sh YOUR_NAMESPACE/lambda-dashboard:<version>
```

Deploy a control-plane version supporting browser requests first. The tool
publishes the static dashboard service, creates or updates `lambda-dashboard`,
sets the API's exact allowed dashboard origin, checks health, and waits for
standby. It requires an idle deployment queue. Metadata is saved to the ignored
`dashboard-deployment.json`. The dashboard stores only the SHA-256 digest of the
admin token; rerun its release after rotating that token.

The browser talks directly to the API after loading the HTML, allowing the
dashboard VM to sleep during builds. Pending-job polling stops at completion.
Builder acquisition retries temporary quota contention for up to a minute; admin
invocation waits briefly for the dashboard to enter standby before waking the
function. The two-live-vCPU limit still applies to concurrent functions and
new page loads during builds.
