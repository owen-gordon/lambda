# API contract

Base URL: `https://YOUR_CONTROLPLANE_FQDN`.
Every control-plane route requires `Authorization: Bearer <admin-token>`.
The existing token is in `.controlplane.env` as `LAMBDA_API_TOKEN`.
Use HTTPS; credentials in query strings are not accepted.

## Submit a function

`POST /v1/deployments` accepts `Content-Type: application/json`:

```json
{
  "name": "hello",
  "metro": "fra",
  "image": "YOUR_NAMESPACE/python-function",
  "source": "def handler(event):\n    return {'message': 'Hello, ' + event['name']}\n",
  "entry": "handler",
  "memory_mb": 128,
  "env": {"GREETING": "Hello"},
  "auth": {"mode": "bearer"}
}
```

| Field | Meaning |
| --- | --- |
| `source` | Required Python source; one file, up to 1 MiB of UTF-8 |
| `image` | Required registry destination in your namespace |
| `metro` | Deployment metro code; defaults to the control plane’s home metro (`dal`) |
| `name` | Optional label; a job-ID suffix makes the instance name unique |
| `entry` | Callable name; defaults to `handler` |
| `memory_mb` | Instance memory; defaults to 128, allowed range 128–2048 |
| `env` | String values injected at runtime; at most 128 keys and 32 KiB total |
| `auth.mode` | `bearer` by default, or explicit `public` |
| `auth.token` | Optional caller-supplied token; otherwise generated for bearer mode |

Unknown fields are rejected. Env names must be identifiers; `LAMBDA_` is reserved.
A supplied invocation token must contain 32–256 ASCII bearer-token characters.
Public mode must not include a token. The account's remaining quota can still
prevent instance creation. The Python runtime requires at least 128 MiB; smaller
requests are rejected before any build work.

The service generates its build context from the submitted source and embedded
runtime, then builds, pushes, and deploys an immutable image digest. Source is
never imported or executed in the control-plane process. Env values and invocation
tokens are passed only to instance creation, never the image build.

`202 Accepted` is returned after the encrypted job is durably saved:

```json
{
  "job": {"id": "<job-id>", "status": "queued", "phase": "queued"},
  "invocation_token": "<save-this-token>"
}
```

The actual job object also includes its generated instance name, metro,
protection mode, and timestamps. The invocation token is returned once.
Disconnecting does not cancel the job. POST is not idempotent; check the jobs
list before resubmitting after a lost response.

## Jobs and instances

| Method | Route | Result |
| --- | --- | --- |
| GET | `/v1/metros` | Available metro codes/countries and default |
| POST | `/v1/deployments` | Build, push, and deploy; return job and invocation token |
| GET | `/v1/jobs` | List job metadata |
| GET | `/v1/jobs/{job-id}` | Status, phase, error, and deployment record |
| POST | `/v1/deployments/{job-id}/health` | Check endpoint health and save an observation |
| POST | `/v1/deployments/{job-id}/invoke` | Invoke using the stored function credential |
| GET | `/v1/deployments/{job-id}` | Refresh the associated instance from Cloud |
| DELETE | `/v1/deployments/{job-id}` | Delete the instance; safe to repeat |
| DELETE | `/v1/jobs/{job-id}` | Remove a terminal job after instance cleanup |
| GET | `/healthz` | Authenticated liveness |

Management routes use job IDs. Status responses omit source, env maps, and
invocation credentials. A successful job includes `deployment.image` as a
repository pinned to its digest, and `deployment.url` as its HTTPS endpoint.

Jobs are `queued`, `running`, `succeeded`, `failed`, or `interrupted`.
Phases include `building`, `pushing`, `waiting_for_builder`, `deploying`, and
`waiting_for_readiness`. Readiness failures include a fixed out-of-memory
diagnosis when kernel logs show a memory panic; raw application logs are never
returned in job errors. Builds run one at a time. Jobs have a 20-minute overall
deadline and a two-minute HTTP readiness deadline after creation. The builder
enters standby before the function starts, freeing its live CPU quota. Builds
remain in the control plane’s home metro; instance operations use each job’s
saved target metro, including after service restart.

The service retains a created instance's identity if readiness fails. Failed or
interrupted jobs can be inspected/deleted using their generated instance names
when the create response did not include a UUID. Instance deletion leaves the
registry image and shared build cache intact; Cloud retention policy applies.
Delete the instance (or confirm its absence through DELETE) before deleting its
job record. Full queues or record stores return 429.

## Instance health and job history

Job status records the original deployment attempt. Repairing an instance does
not rewrite a failed job as a successful build. `deployment.state` describes the
instance as last inspected; `GET /v1/deployments/{job-id}` refreshes it through
the Cloud API without waking the function.

`POST /v1/deployments/{job-id}/health` explicitly wakes the function and requests
its authenticated `/healthz` endpoint, without executing its handler. The result
is saved separately as `health: {healthy, checked_at, error?}` and returned with
HTTP 200 when the observation is recorded, including unhealthy results. It
requires admin bearer auth, uses the function credential, rejects redirects,
and never returns raw function output. Capacity or configuration errors return
4xx/5xx without overwriting the previous observation. Health is a timestamped
observation, not continuous monitoring.

## Invoke through the control plane

`POST /v1/deployments/{job-id}/invoke` accepts JSON with the admin bearer token.
The service invokes the saved deployment with its stored function credential;
the admin token is never forwarded to the function. Requests are limited to
1 MiB, responses to 2 MiB, and the proxy request to 30 seconds. A timeout does
not cancel Python execution. Only recorded Unikraft HTTPS endpoints are allowed;
redirects are rejected. Function response status and body are returned.

The configured dashboard origin can make authenticated browser requests directly
to the API. CORS preflight is unauthenticated; actual requests still require the
admin bearer token. Other explicit browser origins are rejected.

## Invoke a function directly

`POST /` and `POST /invoke` accept a JSON body and call `handler(event)`.
The event may be any JSON value. The function must be synchronous and return a
JSON-serializable value. Success returns status 200 and the serialized result.

Private endpoints require their invocation token on every request, including
`GET /healthz`. Control-plane readiness checks include that token and never call
the function. Public endpoints omit this check.

Malformed JSON returns 400; requests over 1 MiB return 413; non-JSON content
returns 415. Request bodies need Content-Length; chunked uploads are unsupported.
Exceptions and non-serializable results return a generic 500 response, with
tracebacks in instance logs.

Invocation is serial per instance. There is no function execution deadline;
long-running code occupies the instance until it returns. The ten-second socket
timeout only limits request I/O. Only one source file and the available Python
standard library are supported.

The admin token controls every job in this single-operator service. There are no
user accounts or tenant roles. Endpoint authentication protects execution over
hostnames or direct network routes, but does not prevent traffic from reaching
the edge or waking an instance. Gateway rate limiting is not implemented.
