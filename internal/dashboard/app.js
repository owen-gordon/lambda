'use strict';
const apiOrigin = document.querySelector('meta[name="lambda-api"]').content;
const token = new URL(window.location.href).searchParams.get('token');
const $ = id => document.getElementById(id);
let timer = null;
let latestReceipt = null;
let refreshInFlight = false;
let refreshAgain = false;
let inspectAgain = false;

function message(text) { $('message').textContent = text; }
function show(value) { $('result').textContent = typeof value === 'string' ? value : JSON.stringify(value, null, 2); }
function fail(error) { message(error.message); show(error.message); }
async function request(path, options = {}) {
  const response = await fetch(apiOrigin + path, {
    ...options,
    credentials: 'omit',
    referrerPolicy: 'no-referrer',
    headers: {Authorization: 'Bearer ' + token, ...(options.body !== undefined ? {'Content-Type': 'application/json'} : {})}
  });
  const text = await response.text();
  let value;
  try { value = text ? JSON.parse(text) : null; } catch { value = text; }
  if (!response.ok) throw new Error(value?.error || ('HTTP ' + response.status + ': ' + text));
  return value;
}
function button(label, action) {
  const node = document.createElement('button'); node.type = 'button'; node.textContent = label;
  node.addEventListener('click', async () => {
    node.disabled = true;
    try { await action(); } catch (error) { fail(error); } finally { node.disabled = false; }
  });
  return node;
}
function cell(row, text) { const node = row.insertCell(); node.textContent = text; return node; }
async function refresh(inspect = false) {
  clearTimeout(timer);
  if (refreshInFlight) { refreshAgain = true; inspectAgain ||= inspect; return; }
  refreshInFlight = true;
  try {
    const jobs = await request('/v1/jobs');
    if (inspect) await Promise.all(jobs.filter(job => job.deployment && !job.deleted_at && !['queued', 'running'].includes(job.status)).map(async job => {
      try { job.deployment = await request('/v1/deployments/' + job.id); }
      catch (error) { job.inspection_error = error.message; }
    }));
    const selection = $('invoke-job').value;
    $('jobs').replaceChildren();
    $('invoke-job').replaceChildren(new Option('Choose a deployed function', ''));
    for (const job of [...jobs].reverse()) {
      const row = document.createElement('tr');
      cell(row, job.name);
      cell(row, job.metro);
      const pending = ['queued', 'running'].includes(job.status);
      const status = cell(row, pending ? job.status + ' / ' + job.phase : (job.status === 'succeeded' ? 'Deployment completed' : 'Deployment attempt ' + job.status));
      if (job.error) {
        const details = document.createElement('details');
        const summary = document.createElement('summary'); summary.textContent = 'Original deployment error';
        const error = document.createElement('pre'); error.textContent = job.error;
        details.append(summary, error); status.append(details);
      }
      const instance = cell(row, job.deleted_at ? 'deleted' : (job.deployment ? job.deployment.state + ' (' + job.deployment.memory_mb + ' MiB)' : 'No instance reported'));
      if (job.inspection_error) { const error = document.createElement('p'); error.textContent = 'Inspection failed: ' + job.inspection_error; instance.append(error); }
      if (!job.deleted_at && job.deployment) {
        const health = document.createElement('p');
        health.textContent = job.health ? (job.health.healthy ? 'Healthy' : 'Unhealthy') + ' at ' + new Date(job.health.checked_at).toLocaleString() : 'Health not checked';
        instance.append(health);
      }
      const endpoint = cell(row, job.deployment?.url || '—');
      if (job.deployment?.url) {
        const url = new URL(job.deployment.url);
        if (url.protocol === 'https:') {
          const link = document.createElement('a'); link.href = url.href; link.textContent = url.hostname;
          link.target = '_blank'; link.rel = 'noreferrer noopener'; endpoint.replaceChildren(link);
        }
      }
      const actions = row.insertCell();
      actions.append(button('Details', () => show(job)));
      if (!['queued', 'running'].includes(job.status) && !job.deleted_at) {
        actions.append(button('Inspect', async () => { show(await request('/v1/deployments/' + job.id)); await refresh(); }));
        if (job.deployment?.url) {
          actions.append(button('Check health', async () => { show(await request('/v1/deployments/' + job.id + '/health', {method: 'POST'})); await refresh(); }));
          $('invoke-job').add(new Option(job.name, job.id));
          actions.append(button('Invoke', async () => { $('invoke-job').value = job.id; $('payload').focus(); }));
        }
        actions.append(button('Delete instance', async () => {
          if (!window.confirm('Delete instance ' + job.name + '?')) return;
          await request('/v1/deployments/' + job.id, {method: 'DELETE'});
          message('Instance deleted.'); await refresh();
        }));
      }
      if (job.deleted_at) actions.append(button('Remove record', async () => {
        await request('/v1/jobs/' + job.id, {method: 'DELETE'}); await refresh();
      }));
      $('jobs').append(row);
    }
    if ([...$('invoke-job').options].some(option => option.value === selection)) $('invoke-job').value = selection;
    const pending = jobs.filter(job => ['queued', 'running'].includes(job.status)).length;
    message(pending ? pending + ' job(s) pending.' : (jobs.length ? 'Ready.' : 'No jobs yet.'));
    if (pending && !document.hidden) timer = setTimeout(() => refresh().catch(fail), 2500);
  } finally {
    refreshInFlight = false;
    if (refreshAgain) { const inspect = inspectAgain; refreshAgain = false; inspectAgain = false; refresh(inspect).catch(fail); }
  }
}
$('file').addEventListener('change', async () => {
  try {
    const file = $('file').files[0]; if (!file) return;
    if (file.size > 1024 * 1024) throw new Error('Python source exceeds 1 MiB.');
    $('source').value = await file.text();
  } catch (error) { fail(error); }
});
$('build-form').addEventListener('submit', async event => {
  event.preventDefault(); $('build').disabled = true;
  try {
    const env = JSON.parse($('env').value || '{}');
    if (!env || Array.isArray(env) || typeof env !== 'object' || Object.values(env).some(value => typeof value !== 'string')) throw new Error('Environment must be a JSON object with string values.');
    const spec = {metro: $('metro').value, name: $('name').value, image: $('image').value, entry: $('entry').value, source: $('source').value,
      memory_mb: Number($('memory').value), env, auth: {mode: $('access').value}};
    message('Submitting…');
    latestReceipt = await request('/v1/deployments', {method: 'POST', body: JSON.stringify(spec)});
    $('receipt-section').hidden = false; $('receipt').textContent = JSON.stringify(latestReceipt, null, 2);
    await refresh();
  } catch (error) { fail(error); } finally { $('build').disabled = false; }
});
$('invoke-form').addEventListener('submit', async event => {
  event.preventDefault(); $('invoke').disabled = true;
  try {
    if (!$('invoke-job').value) throw new Error('Choose a deployed function.');
    const payload = JSON.parse($('payload').value);
    show(await request('/v1/deployments/' + $('invoke-job').value + '/invoke', {method: 'POST', body: JSON.stringify(payload)}));
    message('Invocation completed.');
  } catch (error) { fail(error); } finally { $('invoke').disabled = false; }
});
$('refresh').addEventListener('click', () => refresh(true).catch(fail));
$('download-receipt').addEventListener('click', () => {
  if (!latestReceipt) return;
  const url = URL.createObjectURL(new Blob([JSON.stringify(latestReceipt, null, 2)], {type: 'application/json'}));
  const link = document.createElement('a'); link.href = url; link.download = 'submission.json'; link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
});
document.addEventListener('visibilitychange', () => { clearTimeout(timer); if (!document.hidden) refresh().catch(fail); });
async function initialize() {
  const catalog = await request('/v1/metros');
  $('metro').replaceChildren(...catalog.metros.map(metro => new Option(metro.code + (metro.country ? ' (' + metro.country.toUpperCase() + ')' : ''), metro.code)));
  $('metro').value = catalog.default;
  $('metro').disabled = false;
  $('build').disabled = false;
  await refresh();
}
initialize().catch(fail);
