'use strict';

const app = document.getElementById('app');
const livePill = document.getElementById('live-pill');
const liveLabel = document.getElementById('live-label');
const toast = document.getElementById('toast');
const activationGate = document.getElementById('activation-gate');
const activationCurrent = document.getElementById('activation-current');
const activationLeft = document.getElementById('activation-left');
let refreshTimer;
let rendered = false;
let renderController;

const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]));
const short = (value, left = 12, right = 7) => value && value.length > left + right + 3 ? `${value.slice(0, left)}…${value.slice(-right)}` : value || '—';
const number = value => Number(value || 0).toLocaleString('en-US');
const percent = value => `${Number(value || 0).toLocaleString('en-US', {maximumFractionDigits: 2})}%`;

function hashrate(value) {
  let n = Number(value || 0);
  const units = ['H/s', 'kH/s', 'MH/s', 'GH/s', 'TH/s', 'PH/s', 'EH/s'];
  let i = 0;
  while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
  const digits = n >= 100 ? 0 : n >= 10 ? 1 : 2;
  return `${n.toLocaleString('en-US', {minimumFractionDigits: i ? digits : 0, maximumFractionDigits: digits})} ${units[i]}`;
}

function difficulty(value) {
  let n = Number(value || 0);
  const units = ['', 'K', 'M', 'G', 'T', 'P'];
  let i = 0;
  while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
  return `${n.toLocaleString('en-US', {maximumFractionDigits: 2})}${units[i]}`;
}

function decimal(value, digits = 6) {
  const raw = String(value ?? '0');
  const match = /^(\d+)(?:\.(\d+))?$/.exec(raw);
  if (!match) return escapeHTML(raw);
  const whole = match[1].replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  const fraction = (match[2] || '').slice(0, digits).replace(/0+$/, '');
  return fraction ? `${whole}.${fraction}` : whole;
}

function qday(amount) {
  if (!amount) return '0 QDAY';
  return `${decimal(amount.qday)} QDAY`;
}

function age(value) {
  if (!value) return '—';
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

async function api(path, signal) {
  const response = await fetch(path, {headers: {'Accept': 'application/json'}, cache: 'no-store', signal});
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
  return body;
}

function notify(message) {
  toast.textContent = message;
  toast.classList.add('show');
  clearTimeout(toast._timer);
  toast._timer = setTimeout(() => toast.classList.remove('show'), 1800);
}

async function copy(value) {
  try { await navigator.clipboard.writeText(value); notify('Copied.'); }
  catch { notify('Copy failed.'); }
}

function setLive(status) {
  livePill.classList.remove('connecting', 'offline');
  if (status?.ready) liveLabel.textContent = 'POOL LIVE';
  else if (status?.network && !status.network.miningActive) { livePill.classList.add('connecting'); liveLabel.textContent = 'OPENS AT 9,100'; }
  else if (status) { livePill.classList.add('offline'); liveLabel.textContent = 'POOL WAITING'; }
  else { livePill.classList.add('connecting'); liveLabel.textContent = 'CONNECTING'; }
  if (status?.network && !status.network.miningActive) {
    activationCurrent.textContent = number(status.network.height);
    activationLeft.textContent = number(status.network.blocksUntilActivation);
    activationGate.hidden = false;
  } else {
    activationGate.hidden = true;
  }
}

function setNav(name) {
  document.querySelectorAll('[data-nav]').forEach(link => link.classList.toggle('active', link.dataset.nav === name));
}

function blockRows(blocks) {
  if (!blocks?.length) return '<tr><td class="empty" colspan="9">No pool blocks yet. The hashrate has work to do.</td></tr>';
  return blocks.map(block => `<tr>
    <td class="primary">${number(block.height)}</td>
    <td><a class="mono-link" href="https://explorer.pqday.com/block/${encodeURIComponent(block.id)}" target="_blank" rel="noreferrer">${escapeHTML(short(block.id, 14, 8))}</a></td>
    <td>${escapeHTML(age(block.foundAt))}</td>
    <td><a class="mono-link route-link" href="/account/${encodeURIComponent(block.foundByAddress)}">${escapeHTML(short(block.foundByAddress, 13, 7))}</a></td>
    <td>${escapeHTML(block.foundByWorker)}</td>
    <td class="numeric">${qday(block.fees)}</td>
    <td class="numeric">${qday(block.distributed)}</td>
    <td class="numeric">${qday(block.poolFee)}</td>
    <td><span class="status ${escapeHTML(block.status)}">${escapeHTML(block.status)}</span></td>
  </tr>`).join('');
}

function minerRows(miners) {
  if (!miners?.length) return '<tr><td class="empty" colspan="6">No accepted shares in the last ten minutes.</td></tr>';
  return miners.map((miner, i) => `<tr data-href="/account/${encodeURIComponent(miner.address)}">
    <td class="primary">${i + 1}</td>
    <td><a class="mono-link route-link" href="/account/${encodeURIComponent(miner.address)}">${escapeHTML(short(miner.address, 15, 8))}</a></td>
    <td>${escapeHTML(miner.worker)}</td>
    <td class="numeric">${hashrate(miner.hashrate)}</td>
    <td class="numeric">${number(miner.shares)}</td>
    <td>${escapeHTML(age(miner.lastAt))}</td>
  </tr>`).join('');
}

function payoutRows(payouts) {
  if (!payouts?.length) return '<tr><td class="empty" colspan="6">No payouts yet.</td></tr>';
  return payouts.map(payout => `<tr>
    <td>${escapeHTML(age(payout.createdAt))}</td>
    <td><span class="status ${escapeHTML(payout.status)}">${escapeHTML(payout.status)}</span></td>
    <td class="numeric">${qday(payout.amount)}</td>
    <td class="numeric">${qday(payout.fee)}</td>
    <td class="numeric">${number(payout.outputs || 0)}</td>
    <td>${payout.transactionID ? `<a class="mono-link" href="https://explorer.pqday.com/transaction/${encodeURIComponent(payout.transactionID)}" target="_blank" rel="noreferrer">${escapeHTML(short(payout.transactionID, 14, 8))}</a>` : '<span class="mono">—</span>'}</td>
  </tr>`).join('');
}

function pagination(path, total, limit, offset) {
  const pages = Math.max(1, Math.ceil(total / limit));
  const current = Math.floor(offset / limit) + 1;
  const items = [];
  const add = page => items.push(page === current ? `<span class="active">${page}</span>` : `<a class="route-link" href="${path}?page=${page}">${page}</a>`);
  items.push(current > 1 ? `<a class="route-link" href="${path}?page=${current - 1}">←</a>` : '<span class="disabled">←</span>');
  const wanted = new Set([1, pages, current - 2, current - 1, current, current + 1, current + 2].filter(p => p >= 1 && p <= pages));
  let previous = 0;
  [...wanted].sort((a,b) => a-b).forEach(page => { if (previous && page > previous + 1) items.push('<span class="disabled">…</span>'); add(page); previous = page; });
  items.push(current < pages ? `<a class="route-link" href="${path}?page=${current + 1}">→</a>` : '<span class="disabled">→</span>');
  return `<nav class="pagination" aria-label="Pagination">${items.join('')}</nav>`;
}

function requestedPage(params) {
  const value = Number(params.get('page') || 1);
  return Number.isSafeInteger(value) && value > 0 && value <= 500000 ? value : 1;
}

function connection(status) {
  const stratum = escapeHTML(status.stratum);
  return `<section class="connection-card">
    <div class="connection-head"><div><h2>CONNECT A MINER.</h2><p>Your QDAY receive address is your account. The worker name after the dot is optional.</p></div><span class="connection-badge">NO REGISTRATION</span></div>
    <div class="endpoint-row"><span>POOL ADDRESS</span><div class="copy-line"><code>${stratum}</code><button class="copy" data-copy="${stratum}">COPY</button></div></div>
    <div class="miner-setup-grid">
      <article class="miner-setup">
        <div class="setup-title"><span>GPU</span><strong>QDAY-GOMINER</strong></div>
        <p>Install the AMD/NVIDIA OpenCL driver, download <b>qday-gominer</b>, replace <b>YOUR_QDAY_ADDRESS</b>, then run:</p>
        <div class="miner-downloads"><a href="https://github.com/petoshi/qday-gominer/releases/download/v1.0.0/qday-gominer-1.0.0-linux-amd64.tar.gz" target="_blank" rel="noreferrer">LINUX X86-64 ↓</a><a href="https://github.com/petoshi/qday-gominer/releases/download/v1.0.0/qday-gominer-1.0.0-linux-arm64.tar.gz" target="_blank" rel="noreferrer">LINUX ARM64 ↓</a><a href="https://github.com/petoshi/qday-gominer/releases/download/v1.0.0/SHA256SUMS" target="_blank" rel="noreferrer">SHA-256 ↓</a></div>
        <code class="command">qday-gominer -url ${stratum} -user YOUR_QDAY_ADDRESS.gpu1</code>
      </article>
      <article class="miner-setup">
        <div class="setup-title"><span>ASIC</span><strong>SIA HARDWARE</strong></div>
        <p>The mining software is already inside the device. Open its pool settings and enter:</p>
        <dl class="asic-fields"><div><dt>URL</dt><dd>${stratum}</dd></div><div><dt>USERNAME</dt><dd>YOUR_QDAY_ADDRESS.asic1</dd></div><div><dt>PASSWORD</dt><dd>x <small>required by some firmware; ignored by the pool</small></dd></div></dl>
      </article>
    </div>
  </section>`;
}

async function overview(signal) {
  setNav('overview');
  const [status, blocks] = await Promise.all([api('/api/status', signal), api('/api/blocks?limit=8&offset=0', signal)]);
  setLive(status);
  app.innerHTML = `<section class="overview-heading"><div><span class="kicker">PUBLIC QDAY PPLNS</span><h1>HASH. GET PAID.</h1><p>No accounts. Rewards go to your QDAY address.</p></div><div class="sync-state"><i class="sync-dot"></i> CHAIN ${status.network.synced ? 'VERIFIED' : 'SYNCING'} · BLOCK ${number(status.network.height)}</div></section>
    ${connection(status)}
    <section class="metric-grid">
      <div class="metric accent"><span>Pool hashrate</span><strong>${hashrate(status.pool.hashrate)}</strong><small>accepted work · 10 minute window</small></div>
      <div class="metric"><span>Network hashrate</span><strong>${hashrate(status.network.hashrate)}</strong><small>BLAKE2b-256 · difficulty ${difficulty(status.network.difficulty)}</small></div>
      <div class="metric"><span>Active miners</span><strong>${number(status.pool.miners)}</strong><small>${number(status.pool.workers)} active workers · ${number(status.pool.connected)} authorized sessions</small></div>
      <div class="metric"><span>Round effort</span><strong>${percent(status.pool.roundEffort)}</strong><small>expected work since last pool block</small></div>
      <div class="metric"><span>Pool blocks</span><strong>${number(status.pool.blocks)}</strong><small>${status.lastBlock ? `last at height ${number(status.lastBlock.height)}` : 'waiting for the first one'}</small></div>
      <div class="metric"><span>Next block template</span><strong>${number(status.network.templateTransactions)} TX</strong><small>${number(status.network.mempoolTransactions)} waiting in mempool · block weight limited</small></div>
    </section>
    <section class="lookup"><label for="address-search">MINER ACCOUNT</label><form class="lookup-row" id="lookup-form"><input id="address-search" autocomplete="off" spellcheck="false" placeholder="Enter a QDAY payout address"><button>LOOK UP</button></form></section>
    <section class="card policy-card"><header class="card-header"><h2>Pool policy</h2><span class="card-meta">EXACT ATOMIC ACCOUNTING</span></header><ul class="policy-list"><li><span>Method</span><strong>${escapeHTML(status.policy.method)} ${number(status.policy.window)}N</strong></li><li><span>Pool reserve</span><strong>${status.policy.feePercent}%</strong></li><li><span>Reward maturity</span><strong>${number(status.policy.maturityBlocks)} BLOCKS</strong></li><li><span>Minimum payout</span><strong>${qday(status.policy.minimumPayout)}</strong></li><li><span>Batch tx fee from reserve</span><strong>${qday(status.policy.payoutFee)}</strong></li></ul></section>
    <section class="card"><header class="card-header"><h2>Latest pool blocks</h2><a class="card-action route-link" href="/blocks">View all blocks →</a></header><div class="table-scroll"><table class="data-table blocks-table"><thead><tr><th>Height</th><th>Block</th><th>Found</th><th>Miner</th><th>Worker</th><th class="numeric">TX fees</th><th class="numeric">Miner credit</th><th class="numeric">Pool reserve</th><th>Status</th></tr></thead><tbody>${blockRows(blocks.blocks)}</tbody></table></div></section>`;
}

async function blocksPage(params, signal) {
  setNav('blocks');
  const page = requestedPage(params); const limit = 20; const offset = (page - 1) * limit;
  const [status, data] = await Promise.all([api('/api/status', signal), api(`/api/blocks?limit=${limit}&offset=${offset}`, signal)]); setLive(status);
  app.innerHTML = `<section class="page-heading"><div><span class="kicker">POOL HISTORY</span><h1>BLOCKS</h1><p>Every block found through this pool, its fees and its maturity state.</p></div><div class="sync-state"><i class="sync-dot"></i> BLOCK ${number(status.network.height)}</div></section>
    <section class="card"><div class="table-scroll"><table class="data-table blocks-table"><thead><tr><th>Height</th><th>Block</th><th>Found</th><th>Miner</th><th>Worker</th><th class="numeric">TX fees</th><th class="numeric">Miner credit</th><th class="numeric">Pool reserve</th><th>Status</th></tr></thead><tbody>${blockRows(data.blocks)}</tbody></table></div></section>${pagination('/blocks', data.total, data.limit, data.offset)}`;
}

async function minersPage(signal) {
  setNav('miners');
  const [status, data] = await Promise.all([api('/api/status', signal), api('/api/miners?limit=100', signal)]); setLive(status);
  app.innerHTML = `<section class="page-heading"><div><span class="kicker">LAST TEN MINUTES</span><h1>MINERS</h1><p>Accepted work by payout address and worker label.</p></div><div class="sync-state"><i class="sync-dot"></i> ${number(status.pool.miners)} ACTIVE</div></section>
    <section class="card"><div class="table-scroll"><table class="data-table miners-table"><thead><tr><th>#</th><th>Address</th><th>Worker</th><th class="numeric">Hashrate</th><th class="numeric">Shares</th><th>Last share</th></tr></thead><tbody>${minerRows(data.miners)}</tbody></table></div></section>`;
}

async function payoutsPage(params, signal) {
  setNav('payouts');
  const page = requestedPage(params); const limit = 20; const offset = (page - 1) * limit;
  const [status, data] = await Promise.all([api('/api/status', signal), api(`/api/payouts?limit=${limit}&offset=${offset}`, signal)]); setLive(status);
  app.innerHTML = `<section class="page-heading"><div><span class="kicker">IDEMPOTENT ON-CHAIN PAYMENTS</span><h1>PAYOUTS</h1><p>Mature PPLNS balances leave in exact atomic amounts.</p></div><div class="sync-state"><i class="sync-dot"></i> MINIMUM ${qday(status.policy.minimumPayout)}</div></section>
    <section class="card"><div class="table-scroll"><table class="data-table payouts-table"><thead><tr><th>Created</th><th>Status</th><th class="numeric">Amount</th><th class="numeric">Fee</th><th class="numeric">Outputs</th><th>Transaction</th></tr></thead><tbody>${payoutRows(data.payouts)}</tbody></table></div></section>${pagination('/payouts', data.total, data.limit, data.offset)}`;
}

async function accountPage(address, signal) {
  setNav('');
  const [status, account] = await Promise.all([api('/api/status', signal), api(`/api/accounts/${encodeURIComponent(address)}`, signal)]); setLive(status);
  app.innerHTML = `<div class="breadcrumbs"><a class="route-link" href="/">Pool</a><span>›</span><span>Miner account</span></div>
    <section class="page-heading"><div><span class="kicker">PPLNS ACCOUNT</span><h1>MINER</h1><p>Credits are attached to the payout address in your Stratum username.</p></div></section>
    <section class="account-id"><span>QDAY PAYOUT ADDRESS</span><code>${escapeHTML(account.address)}</code></section>
    <section class="account-grid"><div class="account-metric"><span>AVAILABLE</span><strong>${qday(account.balance)}</strong><small>eligible for the next payout batch</small></div><div class="account-metric"><span>MATURING</span><strong>${qday(account.pending)}</strong><small>waiting for pool blocks to reach maturity</small></div><div class="account-metric"><span>PAID</span><strong>${qday(account.paid)}</strong><small>submitted on chain</small></div><div class="account-metric"><span>ACCEPTED SHARES</span><strong>${number(account.shares)}</strong><small>${number(account.workers)} worker labels</small></div></section>`;
}

async function render() {
  clearTimeout(refreshTimer);
  renderController?.abort();
  const controller = new AbortController();
  renderController = controller;
  const focused = document.activeElement?.id || '';
  const lookupValue = document.getElementById('address-search')?.value || '';
  try {
    const url = new URL(location.href);
    if (url.pathname === '/') await overview(controller.signal);
    else if (url.pathname === '/blocks') await blocksPage(url.searchParams, controller.signal);
    else if (url.pathname === '/miners') await minersPage(controller.signal);
    else if (url.pathname === '/payouts') await payoutsPage(url.searchParams, controller.signal);
    else if (url.pathname.startsWith('/account/')) await accountPage(decodeURIComponent(url.pathname.slice(9)), controller.signal);
    else { history.replaceState({}, '', '/'); await overview(controller.signal); }
	if (lookupValue) {
	  const input = document.getElementById('address-search');
	  if (input) input.value = lookupValue;
	}
    if (focused) document.getElementById(focused)?.focus({preventScroll: true});
    rendered = true;
    refreshTimer = setTimeout(render, document.hidden ? 30000 : 10000);
  } catch (error) {
    if (error.name === 'AbortError') return;
    if (rendered) notify('Refresh delayed. Retrying…');
    else {
      setLive(null);
      app.innerHTML = `<section class="error-panel"><h2>Pool data unavailable.</h2><p>Retrying automatically.</p></section>`;
    }
    refreshTimer = setTimeout(render, document.hidden ? 30000 : 10000);
  }
}

function navigate(href) {
  history.pushState({}, '', href); window.scrollTo({top: 0, behavior: 'instant'}); render();
}

document.addEventListener('click', event => {
  const route = event.target.closest('a.route-link');
  if (route && !event.metaKey && !event.ctrlKey && !event.shiftKey && event.button === 0) {
    event.preventDefault();
    navigate(route.getAttribute('href'));
    return;
  }
  const copyButton = event.target.closest('[data-copy]');
  if (copyButton) {
    copy(copyButton.dataset.copy);
    return;
  }
  const row = event.target.closest('tr[data-href]');
  if (row && !event.target.closest('a')) navigate(row.dataset.href);
});

document.addEventListener('submit', event => {
  if (event.target.id !== 'lookup-form') return;
  event.preventDefault();
  const value = document.getElementById('address-search').value.trim();
  if (value) navigate(`/account/${encodeURIComponent(value)}`);
});

window.addEventListener('popstate', render);
render();
