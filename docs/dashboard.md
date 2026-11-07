# Browser dashboard

URL: `https://YOUR_DASHBOARD_FQDN/?token=YOUR_TOKEN`

Use the `LAMBDA_API_TOKEN` from `.controlplane.env` as `YOUR_TOKEN`.
The server checks the query parameter before serving any HTML. Missing, wrong,
or duplicate tokens return 401. This is the same full-access admin credential
used by the API, so keep the complete URL private. Query URLs remain in browser
history and may appear in infrastructure logs; the dashboard application does
not log request URLs. Responses disable caching and referrers.

1. Enter an image destination in your registry namespace and a function name.
2. Paste Python source or choose a `.py` file. Set the entry function, memory,
   env JSON, deployment metro, and private/public access.
3. Click **Build and deploy**. Save the receipt for the private invocation token.
4. Watch the job, inspect the instance, or select it under **Invoke a function**
   and send JSON. The admin API supplies the stored invocation credential.
5. Delete the instance when finished. **Remove record** removes its saved job
   after instance deletion; the registry image remains.

The page is plain HTML with embedded JavaScript, served by its own 128 MiB
Unikraft VM. JavaScript calls the control plane directly with bearer auth; the
API allows only this dashboard's origin. No token is saved in local storage.
Both VMs scale to zero when idle. Only queued/running jobs trigger automatic
refresh; use **Refresh** for later changes made elsewhere.

This console has one operator and no separate user roles. It supports the same
single-file Python standard-library functions as the API.

The metro selector loads available locations from Unikraft through `/v1/metros`.
Dallas is the default. The job table shows each function’s target metro. Builds
use the existing Dallas builder regardless of where the function is deployed.

The table separates the original deployment attempt from the instance’s last
reported state. A failed attempt can have a recovered, healthy instance. Original
errors are under **Original deployment error**. **Refresh** inspects VM states
without waking functions; **Check health** explicitly wakes one and saves a
timestamped health result. There is no background health polling.
