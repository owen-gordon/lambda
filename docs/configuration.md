# Local configuration

Copy the blank templates before configuring a deployment:

```sh
umask 077
cp .env.example .env
cp .controlplane.env.example .controlplane.env
cp .endpoints.env.example .endpoints.env
chmod 600 .env .controlplane.env .endpoints.env
```

Set `UKC_TOKEN` to your Unikraft Cloud credential and `UKC_METRO` to the home
metro. Generate `LAMBDA_API_TOKEN` with `openssl rand -hex 32`, and
`LAMBDA_STATE_KEY` with `openssl rand -base64 32`. Keep the state key stable for
an existing deployment; changing it prevents reading encrypted jobs. Existing
operators should keep their current files and keys rather than copying over them.

Set `LAMBDA_API_URL` and `LAMBDA_DASHBOARD_URL` to the HTTPS endpoints in your
private deployment receipts. These URLs are not credentials, but are excluded
from the public repository to keep deployment details local.

Export values for operator commands:

```sh
set -a
source .env
source .controlplane.env
source .endpoints.env
set +a
```

`.env`, `.controlplane.env`, `.endpoints.env`, deployment receipts, function
submission receipts, state backups, generated binaries, and generated CA bundles
are ignored. Do not force-add them. Submission receipts contain function tokens;
state encryption keys and state backups must be stored securely and separately.

The control-plane release tool updates an existing `lambda-controlplane`
instance; it does not provision an account or bootstrap its state volume. See
[operations](operations.md) for runtime configuration and resource requirements.
