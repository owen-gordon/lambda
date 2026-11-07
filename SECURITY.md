# Credentials and publication

Real credentials belong only in the ignored `.env` and `.controlplane.env`
files, with mode 0600. `.endpoints.env` holds deployment-specific URLs. Public
`.example` templates contain empty credential values. Follow
[configuration](docs/configuration.md); never overwrite an existing state key.

Function submission receipts contain invocation tokens. Deployment metadata,
state files and backups, generated binaries, and generated CA bundles must
remain ignored. Do not force-add any of these files. Authentication literals in
tests are synthetic fixtures, not deployed credentials.

Before sharing a change, inspect `git diff --cached` and run a secret scan:

```sh
gitleaks git --redact=100 --no-banner
```

This repository includes a pre-commit hook that scans staged changes and blocks
commits if Gitleaks is unavailable. Install Gitleaks, then enable it per clone:

```sh
git config core.hooksPath .githooks
```

GitHub Actions scans the full commit history and runs the test suite. Hooks and
scanners are additional checks, not guarantees; ignored files can still be
force-added and hooks can be bypassed. If a credential is exposed, revoke/rotate
it immediately before cleaning repository history. Do not post credentials,
private submission receipts, source, or logs containing them in public issues.

The dashboard accepts the admin token in its URL query, so a complete access URL
is a credential. Keep it out of screenshots, issue reports, logs, and commits.
The service is a single-operator prototype: deploying the source does not make
submitted Python code safe to trust or provide tenant isolation.
