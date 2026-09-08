/* revproxy 管理后台 —— 零依赖原生实现 */
'use strict';

const BASE = location.pathname.replace(/index\.html$/, '').replace(/\/$/, '');
const state = {
  user: null, tab: 'overview', routes: [], proxies: [], meta: {}, logs: [],
  upstreams: [], timer: null, logTimer: null, logsAuto: true
};

/* ---------------- 基础工具 ---------------- */
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}
function fmtBytes(n) {
  n = Number(n) || 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + ' ' + u[i];
}
function fmtDur(ms) {
  ms = Number(ms) || 0;
  if (ms < 1000) return ms + ' ms';
  if (ms < 60000) return (ms / 1000).toFixed(2) + ' s';
  return (ms / 60000).toFixed(1) + ' min';
}
function fmtTime(t) {
  if (!t) return '';
  const d = new Date(t);
  const p = n => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function toast(msg, type) {
  const el = document.createElement('div');
  el.className = 'toast ' + (type || '');
  el.textContent = msg;
  $('#toast').appendChild(el);
  setTimeout(() => el.remove(), 3200);
}
async function api(path, opts) {
  opts = opts || {};
  const o = {
    method: opts.method || 'GET',
    headers: { 'X-Revproxy-Admin': '1' },
    credentials: 'same-origin'
  };
  if (opts.body !== undefined) {
    o.headers['Content-Type'] = 'application/json';
    o.body = JSON.stringify(opts.body);
  }
  const res = await fetch(`${BASE}/api/${path}`, o);
  let data;
  try { data = await res.json(); } catch (e) { throw new Error('服务端返回了非 JSON 响应'); }
  if (res.status === 401) { showLogin(); throw new Error('未登录或会话已过期'); }
  if (!data.ok) throw new Error(data.error || '请求失败');
  return data.data !== undefined ? data.data : data;
}

/* ---------------- 登录 ---------------- */
function showLogin() {
  clearTimers();
  $('#login').style.display = '';
  $('#app').hidden = true;
}
function showApp() {
  $('#login').style.display = 'none';
  $('#app').hidden = false;
}
$('#loginForm').addEventListener('submit', async e => {
  e.preventDefault();
  const errEl = $('#loginErr');
  errEl.textContent = '';
  const user = $('#user').value.trim(), pass = $('#pass').value;
  if (!user || !pass) { errEl.textContent = '请输入用户名和密码'; return; }
  try {
    const r = await api('login', { method: 'POST', body: { user, pass } });
    state.user = r.user;
    $('#userChip').textContent = r.user;
    showApp();
    await refresh();
  } catch (err) {
    errEl.textContent = err.message;
  }
});
$('#btnLogout').addEventListener('click', async () => {
  try { await api('logout', { method: 'POST' }); } catch (e) { }
  showLogin();
});

/* ---------------- 弹窗 ---------------- */
function openModal(title, body, foot) {
  const mask = document.createElement('div');
  mask.className = 'modal-mask';
  mask.innerHTML = `<div class="modal">
    <div class="modal-head"><h3>${esc(title)}</h3><button class="x" data-act="close">&times;</button></div>
    <div class="modal-body">${body}</div>
    <div class="modal-foot">${foot || '<button class="btn" data-act="close">关闭</button>'}</div>
  </div>`;
  mask.addEventListener('click', e => {
    if (e.target === mask || e.target.dataset.act === 'close') close();
  });
  $('#modalRoot').appendChild(mask);
  function close() { mask.remove(); }
  return { root: mask, close };
}

/* ---------------- 数据加载 ---------------- */
function clearTimers() {
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
  if (state.logTimer) { clearInterval(state.logTimer); state.logTimer = null; }
}
async function refresh() {
  const d = await api('overview');
  state.routes = d.routes || [];
  state.proxies = (d.meta && d.meta.proxyTotal >= 0) ? state.proxies : state.proxies;
  state.meta = d.meta || {};
  state.upstreams = d.upstreams || [];
  const snap = d.stats || {};
  $('#metaLine').textContent = `v${state.meta.version || ''} · ${state.meta.listen || ''} · 运行 ${state.meta.uptime || '-'}`;
  window.__snapshot = snap;
  render();
  await loadProxies();
}
async function loadProxies() {
  state.proxies = await api('proxies');
  if (state.tab === 'proxies') renderProxies();
}

/* ---------------- 渲染分发 ---------------- */
function render() {
  const t = state.tab;
  if (t === 'overview') renderOverview();
  else if (t === 'routes') renderRoutes();
  else if (t === 'proxies') renderProxies();
  else if (t === 'logs') { renderLogs(); startLogTimer(); }
  else if (t === 'settings') renderSettings();
}
$('#tabs').addEventListener('click', e => {
  const b = e.target.closest('.tab');
  if (!b) return;
  $$('.tab').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  state.tab = b.dataset.tab;
  if (state.tab !== 'logs' && state.logTimer) { clearInterval(state.logTimer); state.logTimer = null; }
  render();
});
$('#btnRefresh').addEventListener('click', () => refresh().then(() => toast('已刷新', 'ok')).catch(e => toast(e.message, 'err')));
$('#btnReloadCfg').addEventListener('click', async () => {
  try { await api('reload', { method: 'POST' }); toast('配置已热重载', 'ok'); await refresh(); }
  catch (e) { toast(e.message, 'err'); }
});

/* ---------------- 概览 ---------------- */
function renderOverview() {
  const s = window.__snapshot || {};
  const qps = (s.qps || 0).toFixed(1);
  const avg = (s.avgDur || 0).toFixed(1);
  const series = s.qps1m || [];
  const max = Math.max(1, ...series);
  const spark = series.map(v => `<i style="height:${Math.max(1, Math.round(v / max * 44))}px" title="${v} req/s"></i>`).join('');

  const routes = state.routes.map(r => {
    const up = (state.upstreams.find(u => u.id === r.id) || {}).backends || [];
    const alive = up.filter(b => b.alive).length;
    return `<tr>
      <td><b>${esc(r.name || r.id)}</b><div class="dim small mono">${esc(r.host || '*')}${esc(r.path)}</div></td>
      <td>${r.enabled ? '<span class="badge on">启用</span>' : '<span class="badge off">停用</span>'}</td>
      <td class="mono small">${up.length ? `${alive}/${up.length}` : '-'}</td>
      <td class="mono">${r.reqs || 0}</td>
      <td class="mono">${(r.avgDur || 0).toFixed(0)} ms</td>
      <td class="mono">${r.err5xx ? `<span class="s-5">${r.err5xx}</span>` : '0'}</td>
      <td class="mono small dim">${esc(r.lastError || '')}</td>
    </tr>`;
  }).join('');

  const upstreams = state.upstreams.map(u => {
    const bs = (u.backends || []).map(b => `<span class="badge ${b.alive ? 'on' : 'err'}" title="${esc(b.lastErr || '')}">
      <span class="dot ${b.alive ? 'ok' : 'err'}"></span>${esc(b.url)} <span class="dim">${esc(b.proxy || '直连')}</span></span>`).join(' ');
    return `<tr><td><b>${esc(u.name || u.id)}</b></td><td>${bs}</td></tr>`;
  }).join('');

  $('#view').innerHTML = `
    <div class="grid">
      <div class="card"><div class="k">总请求数</div><div class="v">${s.totalReq || 0}</div><div class="sub">路由 ${state.meta.routeOn || 0}/${state.meta.routeTotal || 0} 启用</div></div>
      <div class="card"><div class="k">当前 QPS</div><div class="v">${qps}</div><div class="sub">近 5 秒平均</div></div>
      <div class="card"><div class="k">活跃连接</div><div class="v">${s.active || 0}</div><div class="sub">正在处理中</div></div>
      <div class="card"><div class="k">平均延迟</div><div class="v">${avg}<span class="small dim"> ms</span></div><div class="sub">出流量 ${fmtBytes(s.bytesOut)}</div></div>
      <div class="card"><div class="k">运行时长</div><div class="v" style="font-size:18px">${esc(s.uptime || '-')}</div><div class="sub">Go ${esc(state.meta.goVersion || '')}</div></div>
    </div>

    ${state.proxies.length ? '' : `<div class="panel panel-tip"><div class="panel-body">
      <b>快速开始：</b>如果你的上游需要通过 SOCKS5 才能访问，请先到「上游代理（添加代理）」页添加代理，再回到「路由」页配置反代规则。
    </div></div>`}
    <div class="section">
      <div class="section-head"><h3>近 60 秒请求量</h3><span class="dim small">峰值 ${max} req/s</span></div>
      <div class="panel"><div class="panel-body"><div class="spark">${spark || '<span class="dim small">暂无数据</span>'}</div></div></div>
    </div>

    <div class="section">
      <div class="section-head"><h3>路由实时指标</h3>
        <button class="btn sm" data-act="goto-routes">管理路由</button></div>
      <div class="panel"><div class="tbl-wrap"><table>
        <thead><tr><th>路由</th><th>状态</th><th>健康节点</th><th>请求数</th><th>平均延迟</th><th>5xx</th><th>最近错误</th></tr></thead>
        <tbody>${routes || '<tr><td colspan="7" class="empty">还没有路由，去「路由」页新建一条吧</td></tr>'}</tbody>
      </table></div></div>
    </div>

    <div class="section">
      <div class="section-head"><h3>上游节点健康状态</h3></div>
      <div class="panel"><div class="tbl-wrap"><table>
        <thead><tr><th style="width:180px">路由</th><th>节点</th></tr></thead>
        <tbody>${upstreams || '<tr><td colspan="2" class="empty">暂无节点</td></tr>'}</tbody>
      </table></div></div>
    </div>`;
}

$('#view').addEventListener('click', e => {
  const b = e.target.closest('[data-act]');
  if (!b) return;
  const act = b.dataset.act;
  if (act === 'goto-routes') {
    $$('.tab').forEach(x => x.classList.remove('active'));
    $('.tab[data-tab="routes"]').classList.add('active');
    state.tab = 'routes'; render();
  }
});

/* ---------------- 路由管理 ---------------- */
const LB_OPTIONS = [
  ['round_robin', '轮询 (round_robin)'], ['weighted', '加权轮询 (weighted)'],
  ['least_conn', '最少连接 (least_conn)'], ['ip_hash', 'IP 哈希 (ip_hash)'], ['random', '随机 (random)']
];
const MATCH_OPTIONS = [['prefix', '前缀匹配'], ['exact', '精确匹配'], ['regex', '正则匹配']];

function proxyOptions(sel, withDefault) {
  let html = '';
  if (withDefault) html += `<option value=""${!sel ? ' selected' : ''}>跟随全局默认</option>`;
  html += `<option value="__direct__"${sel === '__direct__' ? ' selected' : ''}>直连（不走代理）</option>`;
  state.proxies.forEach(p => {
    html += `<option value="${esc(p.id)}"${sel === p.id ? ' selected' : ''}>${esc(p.name || p.id)} — ${esc(p.type)}://${esc(p.addr)}</option>`;
  });
  return html;
}

function schemeOptions(sel) {
  const opts = [
    ['', '继承路由默认'],
    ['http', 'http'],
    ['https', 'https（如 raw.githubusercontent.com）'],
  ];
  return opts.map(([v, t]) => `<option value="${v}"${sel === v ? ' selected' : ''}>${t}</option>`).join('');
}

function renderRoutes() {
  const rows = state.routes.map(r => {
    const tags = [];
    if (r.host) tags.push(`<span class="badge acc">${esc(r.host)}</span>`);
    tags.push(`<span class="badge">${esc(r.match)}</span>`);
    if (r.mode === 'domain_in_path') {
      tags.push(`<span class="badge pp">万能反代 ${esc(r.scheme || 'http')}</span>`);
      if (r.domainRules && r.domainRules.length) tags.push(`<span class="badge warn">域名规则×${r.domainRules.length}</span>`);
    }
    if (r.proxyId) tags.push(`<span class="badge pp">${esc(proxyName(r.proxyId))}</span>`);
    else if (r.proxyId === '__direct__') tags.push(`<span class="badge">直连</span>`);
    if (r.access && r.access.basicAuth && r.access.basicAuth.enabled) tags.push('<span class="badge warn">Basic 认证</span>');
    const beArr = Array.isArray(r.backends) ? r.backends : [];
    const drArr = Array.isArray(r.domainRules) ? r.domainRules : [];
    const backendCell = r.mode === 'domain_in_path'
      ? `<span class="dim">路径内嵌域名<br><small>${esc(drArr.map(d => d.match + '→' + proxyName(d.proxyId)).join('，') || '默认：' + proxyName(r.proxyId))}</small></span>`
      : beArr.map(b => esc(b.url)).join('<br>');
    return `<tr>
      <td>
        <b>${esc(r.name || '(未命名)')}</b>
        <div class="dim small mono">${esc(r.host || '*')}${esc(r.path)}${r.rewrite ? ' → ' + esc(r.rewrite) : ''}</div>
        <div style="margin-top:4px">${tags.join(' ')}</div>
      </td>
      <td class="mono small">${backendCell}</td>
      <td class="small">${esc(r.lb)}</td>
      <td>${r.enabled ? '<span class="badge on">启用</span>' : '<span class="badge off">停用</span>'}</td>
      <td class="small dim">${r.updatedAt ? esc(String(r.updatedAt).slice(0, 19).replace('T', ' ')) : ''}</td>
      <td style="text-align:right;white-space:nowrap">
        <button class="btn sm" data-act="edit" data-id="${esc(r.id)}">编辑</button>
        <button class="btn sm" data-act="toggle" data-id="${esc(r.id)}">${r.enabled ? '停用' : '启用'}</button>
        <button class="btn sm" data-act="dup" data-id="${esc(r.id)}">复制</button>
        <button class="btn sm danger" data-act="del" data-id="${esc(r.id)}">删除</button>
      </td>
    </tr>`;
  }).join('');
  $('#view').innerHTML = `
    <div class="section">
      <div class="section-head">
        <h3>路由规则 <span class="dim small">（优先级高的先匹配；同优先级下路径更长的先匹配）</span></h3>
        <div style="display:flex;gap:8px">
          <button class="btn" data-act="quick">快速添加</button>
          <button class="btn primary" data-act="new"><b>+</b> 添加路由</button>
        </div>
      </div>
      <div class="panel"><div class="tbl-wrap"><table>
        <thead><tr><th>名称 / 匹配</th><th>上游节点</th><th>负载均衡</th><th>状态</th><th>更新时间</th><th></th></tr></thead>
        <tbody>${rows || '<tr><td colspan="6" class="empty">还没有任何路由</td></tr>'}</tbody>
      </table></div></div>
    </div>`;
}

function proxyName(id) {
  if (!id || id === '__direct__') return '直连';
  const p = state.proxies.find(x => x.id === id);
  return p ? (p.name || p.addr) : '未知代理';
}

$('#view').addEventListener('click', async e => {
  const b = e.target.closest('[data-act]');
  if (!b) return;
  const act = b.dataset.act, id = b.dataset.id;
  switch (act) {
    case 'new': openRouteEditor(null); break;
    case 'quick': openQuickAdd(); break;
    case 'edit': {
      const r = await api('routes/' + id);
      openRouteEditor(r);
      break;
    }
    case 'toggle': await api('routes/' + id + '/toggle', { method: 'POST' }); toast('已切换状态', 'ok'); await refresh(); break;
    case 'dup': await api('routes/' + id + '/duplicate', { method: 'POST' }); toast('已复制', 'ok'); await refresh(); break;
    case 'del': {
      const r = state.routes.find(x => x.id === id);
      if (!confirm(`确认删除路由「${(r && r.name) || id}」？`)) return;
      await api('routes/' + id, { method: 'DELETE' }); toast('已删除', 'ok'); await refresh();
      break;
    }
    case 'new-proxy': openProxyEditor(null); break;
    case 'edit-proxy': openProxyEditor(state.proxies.find(p => p.id === id)); break;
    case 'del-proxy': {
      if (!confirm('确认删除该上游代理？引用它的路由会退化为直连。')) return;
      await api('proxies/' + id, { method: 'DELETE' }); toast('已删除', 'ok'); await refresh();
      break;
    }
    case 'test-proxy': await testProxy(id); break;
    case 'clear-logs': await api('logs/clear', { method: 'POST' }); toast('已清空', 'ok'); renderLogs(); break;
    case 'toggle-logs-auto': {
      state.logsAuto = !state.logsAuto;
      b.textContent = state.logsAuto ? '自动刷新：开' : '自动刷新：关';
      b.classList.toggle('primary', state.logsAuto);
      startLogTimer();
      break;
    }
    case 'save-settings': await saveSettings(); break;
    case 'change-pwd': await changePassword(); break;
    case 'export-cfg': window.open(`${BASE}/api/export`, '_blank'); break;
    case 'import-cfg': $('#importFile').click(); break;
  }
});

/* ---------------- 快速添加 ---------------- */
function openQuickAdd() {
  const body = `
    <div class="frow">
      <label>名称<input id="q_name" placeholder="例如：本地服务"></label>
      <label>监听域名（可留空）<input id="q_host" placeholder="a.example.com 或 *.example.com"></label>
    </div>
    <div class="frow">
      <label>路径前缀<input id="q_path" value="/" placeholder="/api"></label>
      <label>上游地址<input id="q_url" placeholder="http://127.0.0.1:9000"></label>
    </div>
    <div class="frow">
      <label>走哪条代理<select id="q_proxy" style="width:100%">${proxyOptions('', true)}</select></label>
      <label>路径重写（留空=原样转发）<input id="q_rewrite" placeholder="/ 表示去掉前缀"></label>
    </div>
    <div class="hint">保存后可在「编辑」里配置负载均衡、健康检查、访问控制等高级选项。</div>`;
  const m = openModal('快速添加路由', body,
    `<button class="btn" data-act="close">取消</button><button class="btn primary" id="q_save">保存</button>`);
  $('#q_save', m.root).addEventListener('click', async () => {
    const url = $('#q_url', m.root).value.trim();
    if (!url) { toast('请填写上游地址', 'err'); return; }
    const route = {
      name: $('#q_name', m.root).value.trim() || '新路由',
      enabled: true,
      host: $('#q_host', m.root).value.trim(),
      path: $('#q_path', m.root).value.trim() || '/',
      match: 'prefix',
      priority: 0,
      rewrite: $('#q_rewrite', m.root).value.trim(),
      stripPath: false,
      backends: [{ url, weight: 1, proxyId: $('#q_proxy', m.root).value, backup: false }],
      lb: 'round_robin',
      proxyId: $('#q_proxy', m.root).value,
      insecureTLS: false, preserveHost: false, retry: 1, timeout: 0,
      headers: { request: [], response: [], remove: [] },
      health: { enabled: false, path: '/', interval: 10, timeout: 3, fails: 3, passes: 2, codes: [] },
      access: { basicAuth: { enabled: false, username: '', password: '', realm: '' }, ipAllow: [], ipDeny: [], rate: { enabled: false, rps: 10, burst: 20, perIp: true } },
      note: ''
    };
    try {
      await api('routes', { method: 'POST', body: route });
      toast('路由已创建', 'ok'); m.close(); await refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
}

/* ---------------- 路由编辑器 ---------------- */
function openRouteEditor(r) {
  const isNew = !r;
  r = r || {
    id: '', name: '', enabled: true, host: '', path: '/', match: 'prefix', priority: 0,
    rewrite: '', stripPath: false, backends: [{ url: '', weight: 1, proxyId: '', backup: false }],
    lb: 'round_robin', proxyId: '', insecureTLS: false, preserveHost: false, retry: 1, timeout: 0,
    headers: { request: [], response: [], remove: [] },
    health: { enabled: false, path: '/', interval: 10, timeout: 3, fails: 3, passes: 2, codes: [] },
    access: { basicAuth: { enabled: false, username: '', password: '', realm: '' }, ipAllow: [], ipDeny: [], rate: { enabled: false, rps: 10, burst: 20, perIp: true } },
    note: ''
  };
  r.headers = r.headers || { request: [], response: [], remove: [] };
  r.health = r.health || {};
  r.access = r.access || { basicAuth: {}, rate: {} };
  r.access.basicAuth = r.access.basicAuth || {};
  r.access.rate = r.access.rate || {};

  r.backends = Array.isArray(r.backends) ? r.backends : [{ url: '', weight: 1, proxyId: '', backup: false }];
  r.domainRules = Array.isArray(r.domainRules) ? r.domainRules : [];
  const beRows = r.backends.map((b, i) => beRowHTML(b, i)).join('');
  const kvToText = arr => (arr || []).map(kv => `${kv.name}: ${kv.value}`).join('\n');

  const body = `
  <fieldset><legend>基本</legend>
    <div class="frow">
      <label>路由名称<input id="f_name" value="${esc(r.name)}" placeholder="给自己看的备注名"></label>
      <label>路由模式<select id="f_mode">
        <option value=""${r.mode !== 'domain_in_path' ? ' selected' : ''}>普通模式（转发到固定上游）</option>
        <option value="domain_in_path"${r.mode === 'domain_in_path' ? ' selected' : ''}>万能反代（域名写在路径里）</option>
      </select><div class="hint">万能反代：访问 /域名/路径 即代理 协议://域名/路径，无需为每个站点建规则</div></label>
    </div>
    <div class="frow">
      <label>优先级<input id="f_priority" type="number" value="${Number(r.priority) || 0}"><div class="hint">数字越大越先匹配</div></label>
      <label style="display:flex;align-items:flex-end;height:100%;padding-bottom:8px">
        <label class="switch" style="margin:0"><input id="f_enabled" type="checkbox"${r.enabled !== false ? ' checked' : ''}> 启用该路由</label>
      </label>
    </div>
    <div class="frow">
      <label>域名 Host<input id="f_host" value="${esc(r.host)}" placeholder="留空=任意，支持 *.example.com"></label>
      <label>匹配方式<select id="f_match">${MATCH_OPTIONS.map(([v, t]) => `<option value="${v}"${r.match === v ? ' selected' : ''}>${t}</option>`).join('')}</select></label>
    </div>
    <div class="frow three">
      <label>路径前缀<input id="f_path" value="${esc(r.path)}" placeholder="/ 或 /proxy"><div class="hint">万能反代下这是入口前缀，如 /proxy 则访问 /proxy/a.com/x</div></label>
      <label>重写为<input id="f_rewrite" value="${esc(r.rewrite || '')}" placeholder="留空=原样转发"><div class="hint">仅普通模式生效</div></label>
      <label></label>
    </div>
  </fieldset>

  <fieldset><legend>代理出口（默认走哪个代理）</legend>
    <div class="frow">
      <label>路由级默认代理<select id="f_proxyId">${proxyOptions(r.proxyId, true)}</select><div class="hint">普通模式：节点未单独指定时使用；万能反代：未命中域名规则时的出口</div></label>
    </div>
  </fieldset>

  <fieldset id="beFieldset"><legend>上游节点（普通模式）</legend>
    <div id="beList">${beRows}</div>
    <button class="btn sm" id="addBe">+ 添加节点</button>
    <div class="frow" style="margin-top:12px">
      <label>负载均衡<select id="f_lb">${LB_OPTIONS.map(([v, t]) => `<option value="${v}"${r.lb === v ? ' selected' : ''}>${t}</option>`).join('')}</select></label>
    </div>
  </fieldset>

  <fieldset id="dynBox" ${r.mode === 'domain_in_path' ? '' : 'hidden'}><legend>万能反代设置</legend>
    <div class="frow">
      <label>目标协议<select id="f_scheme">
        <option value="http"${r.scheme !== 'https' ? ' selected' : ''}>http</option>
        <option value="https"${r.scheme === 'https' ? ' selected' : ''}>https</option>
      </select><div class="hint">访问 /a.com/x 时拼成 协议://a.com/x</div></label>
    </div>
    <div class="hint">域名规则：指定某些域名走特定代理（如 b.com 走 SOCKS5）。未列出的域名用上面的「路由级默认代理」。</div>
    <div id="drList"></div>
    <button class="btn sm" id="addDr">+ 添加域名规则</button>
  </fieldset>

  <fieldset><legend>超时与重试</legend>
    <div class="frow three">
      <label>请求超时（秒）<input id="f_timeout" type="number" value="${Number(r.timeout) || 0}"><div class="hint">0=不限，WebSocket 自动忽略</div></label>
      <label>失败重试次数<input id="f_retry" type="number" value="${Number(r.retry) || 0}"><div class="hint">仅对无请求体的请求生效</div></label>
      <label style="padding-bottom:6px">
        <label class="switch"><input id="f_insecure" type="checkbox"${r.insecureTLS ? ' checked' : ''}> 跳过上游 TLS 证书校验</label>
        <label class="switch"><input id="f_preserve" type="checkbox"${r.preserveHost ? ' checked' : ''}> 保留客户端 Host 头</label>
      </label>
    </div>
  </fieldset>

  <fieldset><legend>自定义请求头（每行一条 <code>Name: Value</code>）</legend>
    <textarea id="f_hdrReq" placeholder="X-Api-Key: abc123&#10;X-Source: revproxy">${esc(kvToText(r.headers.request))}</textarea>
    <div class="frow" style="margin-top:10px">
      <label>自定义响应头<textarea id="f_hdrRes" placeholder="X-Powered-By: revproxy">${esc(kvToText(r.headers.response))}</textarea></label>
      <label>删除请求头（逗号分隔）<input id="f_hdrDel" value="${esc((r.headers.remove || []).join(','))}" placeholder="Cookie,Authorization"></label>
    </div>
  </fieldset>

  <fieldset><legend>健康检查</legend>
    <label class="switch"><input id="f_hcOn" type="checkbox"${r.health.enabled ? ' checked' : ''}> 启用主动健康检查</label>
    <div class="frow three">
      <label>探测路径<input id="f_hcPath" value="${esc(r.health.path || '/')}" placeholder="/healthz（留空则只做 TCP 探测）"></label>
      <label>间隔（秒）<input id="f_hcInterval" type="number" value="${Number(r.health.interval) || 10}"></label>
      <label>超时（秒）<input id="f_hcTimeout" type="number" value="${Number(r.health.timeout) || 3}"></label>
    </div>
    <div class="frow">
      <label>连续失败 N 次下线<input id="f_hcFails" type="number" value="${Number(r.health.fails) || 3}"></label>
      <label>连续成功 N 次上线<input id="f_hcPasses" type="number" value="${Number(r.health.passes) || 2}"></label>
    </div>
  </fieldset>

  <fieldset><legend>访问控制</legend>
    <label class="switch"><input id="f_baOn" type="checkbox"${r.access.basicAuth.enabled ? ' checked' : ''}> 启用 Basic 认证（访问该路由需输入账号密码）</label>
    <div class="frow three">
      <label>账号<input id="f_baUser" value="${esc(r.access.basicAuth.username || '')}"></label>
      <label>密码<input id="f_baPass" value="${esc(r.access.basicAuth.password || '')}"></label>
      <label>提示文本 Realm<input id="f_baRealm" value="${esc(r.access.basicAuth.realm || '')}" placeholder="Restricted"></label>
    </div>
    <div class="frow">
      <label>IP 白名单（逗号分隔，支持 CIDR）<input id="f_ipAllow" value="${esc((r.access.ipAllow || []).join(','))}" placeholder="192.168.1.0/24, 10.0.0.1"></label>
      <label>IP 黑名单<input id="f_ipDeny" value="${esc((r.access.ipDeny || []).join(','))}"></label>
    </div>
    <label class="switch"><input id="f_rateOn" type="checkbox"${r.access.rate.enabled ? ' checked' : ''}> 启用限流</label>
    <div class="frow three">
      <label>每秒请求数<input id="f_rateRps" type="number" step="0.1" value="${Number(r.access.rate.rps) || 10}"></label>
      <label>突发容量<input id="f_rateBurst" type="number" value="${Number(r.access.rate.burst) || 20}"></label>
      <label style="padding-bottom:6px"><label class="switch"><input id="f_ratePerIp" type="checkbox"${r.access.rate.perIp !== false ? ' checked' : ''}> 按 IP 分别限流</label></label>
    </div>
  </fieldset>

  <fieldset><legend>备注</legend>
    <textarea id="f_note" placeholder="可选，写点说明">${esc(r.note || '')}</textarea>
  </fieldset>`;

  const m = openModal(isNew ? '添加路由' : `编辑路由：${r.name || r.id}`, body,
    `<button class="btn" data-act="close">取消</button>
     <button class="btn" id="btnTestUp">测试上游连通性</button>
     <button class="btn primary" id="btnSaveRoute">保存</button>`);

  // 万能反代：渲染已有的域名规则
  const drList = $('#drList', m.root);
  (r.domainRules || []).forEach((d, i) => drList.insertAdjacentHTML('beforeend', drRowHTML(d, i)));
  const syncDynVisibility = () => {
    const dyn = $('#f_mode', m.root).value === 'domain_in_path';
    $('#beFieldset', m.root).hidden = dyn;
    $('#dynBox', m.root).hidden = !dyn;
  };
  $('#f_mode', m.root).addEventListener('change', syncDynVisibility);
  $('#addDr', m.root).addEventListener('click', () => {
    drList.insertAdjacentHTML('beforeend', drRowHTML({ match: '', proxyId: '' }, drList.children.length));
  });
  drList.addEventListener('click', e => {
    const t = e.target.closest('[data-dract]');
    if (t && t.dataset.dract === 'del') t.closest('.dr-row').remove();
  });

  $('#addBe', m.root).addEventListener('click', () => {
    const list = $('#beList', m.root);
    const idx = list.children.length;
    list.insertAdjacentHTML('beforeend', beRowHTML({ url: '', weight: 1, proxyId: '', backup: false }, idx));
  });
  $('#beList', m.root).addEventListener('click', async e => {
    const t = e.target.closest('[data-bact]');
    if (!t) return;
    if (t.dataset.bact === 'del') {
      if ($('#beList', m.root).children.length > 1) t.closest('.be-row').remove();
      else toast('至少保留一个上游节点', 'warn');
    } else if (t.dataset.bact === 'test') {
      const row = t.closest('.be-row');
      const url = $('input[data-f=url]', row).value.trim();
      const pid = $('select[data-f=proxyId]', row).value;
      if (!url) { toast('请先填写上游地址', 'warn'); return; }
      t.disabled = true; const old = t.textContent; t.textContent = '测试中…';
      try {
        const res = await api('upstream-test', { method: 'POST', body: { url, proxyId: pid, path: '', method: 'GET', insecure: $('#f_insecure', m.root).checked } });
        toast(`可达：HTTP ${res.status}，耗时 ${res.elapsed}，经 ${res.via}`, 'ok');
      } catch (err) { toast('不可达：' + err.message, 'err'); }
      t.textContent = old; t.disabled = false;
    }
  });

  $('#btnTestUp', m.root).addEventListener('click', async () => {
    for (const row of $$('#beList .be-row', m.root)) {
      const url = $('input[data-f=url]', row).value.trim();
      if (url) { $('[data-bact=test]', row).click(); break; }
    }
  });

  $('#btnSaveRoute', m.root).addEventListener('click', async () => {
    const route = collectRoute(m.root, r);
    if (!route) return;
    try {
      if (isNew) await api('routes', { method: 'POST', body: route });
      else await api('routes/' + r.id, { method: 'PUT', body: route });
      toast('已保存并热重载', 'ok'); m.close(); await refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
}

function beRowHTML(b, i) {
  return `<div class="be-row">
    <label>地址<input data-f="url" value="${esc(b.url)}" placeholder="http://127.0.0.1:9000"></label>
    <label>权重<input data-f="weight" type="number" min="1" value="${Number(b.weight) || 1}"></label>
    <label>代理<select data-f="proxyId">${proxyOptions(b.proxyId, true)}</select></label>
    <label class="switch" style="padding-bottom:8px"><input data-f="backup" type="checkbox"${b.backup ? ' checked' : ''}> 备用节点</label>
    <div style="display:flex;gap:6px;padding-bottom:6px">
      <button class="btn sm" type="button" data-bact="test">测试</button>
      <button class="btn sm danger" type="button" data-bact="del">删</button>
    </div>
  </div>`;
}

function drRowHTML(d, i) {
  d = d || { match: '', proxyId: '', scheme: '' };
  return `<div class="dr-row">
    <label>域名（支持 *.b.com）<input data-f="match" value="${esc(d.match)}" placeholder="b.com"></label>
    <label>走哪个代理<select data-f="proxyId">${proxyOptions(d.proxyId, true)}</select></label>
    <label>协议<select data-f="scheme">${schemeOptions(d.scheme)}</select></label>
    <button class="btn sm danger" type="button" data-dract="del">删</button>
  </div>`;
}

function collectRoute(root, old) {
  const name = $('#f_name', root).value.trim();
  if (!name) { toast('请填写路由名称', 'err'); return null; }
  const mode = $('#f_mode', root).value;
  const backends = $$('#beList .be-row', root).map(row => ({
    url: $('input[data-f=url]', row).value.trim(),
    weight: Number($('input[data-f=weight]', row).value) || 1,
    proxyId: $('select[data-f=proxyId]', row).value,
    backup: $('input[data-f=backup]', row).checked
  })).filter(b => b.url);
  if (mode !== 'domain_in_path' && !backends.length) { toast('至少填写一个上游地址', 'err'); return null; }
  const parseKV = txt => String(txt || '').split('\n').map(l => l.trim()).filter(Boolean).map(l => {
    const i = l.indexOf(':');
    return i > 0 ? { name: l.slice(0, i).trim(), value: l.slice(i + 1).trim() } : null;
  }).filter(Boolean);
  const splitList = txt => String(txt || '').split(',').map(s => s.trim()).filter(Boolean);
  const path = $('#f_path', root).value.trim() || '/';
  const domainRules = $$('#drList .dr-row', root).map(row => ({
    match: $('input[data-f=match]', row).value.trim(),
    proxyId: $('select[data-f=proxyId]', row).value,
    scheme: $('select[data-f=scheme]', row).value
  })).filter(d => d.match);
  return {
    name,
    enabled: $('#f_enabled', root).checked,
    host: $('#f_host', root).value.trim(),
    path,
    match: $('#f_match', root).value,
    priority: Number($('#f_priority', root).value) || 0,
    rewrite: $('#f_rewrite', root).value.trim(),
    stripPath: false,
    mode,
    scheme: mode === 'domain_in_path' ? ($('#f_scheme', root).value || 'http') : '',
    domainRules,
    backends,
    lb: $('#f_lb', root).value,
    proxyId: $('#f_proxyId', root).value,
    insecureTLS: $('#f_insecure', root).checked,
    preserveHost: $('#f_preserve', root).checked,
    retry: Number($('#f_retry', root).value) || 0,
    timeout: Number($('#f_timeout', root).value) || 0,
    headers: {
      request: parseKV($('#f_hdrReq', root).value),
      response: parseKV($('#f_hdrRes', root).value),
      remove: splitList($('#f_hdrDel', root).value)
    },
    health: {
      enabled: $('#f_hcOn', root).checked,
      path: $('#f_hcPath', root).value.trim(),
      interval: Number($('#f_hcInterval', root).value) || 10,
      timeout: Number($('#f_hcTimeout', root).value) || 3,
      fails: Number($('#f_hcFails', root).value) || 3,
      passes: Number($('#f_hcPasses', root).value) || 2,
      codes: []
    },
    access: {
      basicAuth: {
        enabled: $('#f_baOn', root).checked,
        username: $('#f_baUser', root).value.trim(),
        password: $('#f_baPass', root).value,
        realm: $('#f_baRealm', root).value.trim()
      },
      ipAllow: splitList($('#f_ipAllow', root).value),
      ipDeny: splitList($('#f_ipDeny', root).value),
      rate: {
        enabled: $('#f_rateOn', root).checked,
        rps: Number($('#f_rateRps', root).value) || 10,
        burst: Number($('#f_rateBurst', root).value) || 20,
        perIp: $('#f_ratePerIp', root).checked
      }
    },
    note: $('#f_note', root).value,
    createdAt: old && old.createdAt
  };
}

/* ---------------- 上游代理 ---------------- */
function renderProxies() {
  const rows = state.proxies.map(p => `<tr>
    <td><b>${esc(p.name || p.id)}</b><div class="dim small">${esc(p.note || '')}</div></td>
    <td><span class="badge ${p.type === 'socks5' ? 'pp' : 'acc'}">${esc(p.type)}</span></td>
    <td class="mono">${esc(p.addr)}</td>
    <td class="mono small">${esc(p.username || '-')}</td>
    <td class="mono small">${p.timeout || 10}s</td>
    <td style="text-align:right;white-space:nowrap">
      <button class="btn sm" data-act="test-proxy" data-id="${esc(p.id)}">测试</button>
      <button class="btn sm" data-act="edit-proxy" data-id="${esc(p.id)}">编辑</button>
      <button class="btn sm danger" data-act="del-proxy" data-id="${esc(p.id)}">删除</button>
    </td></tr>`).join('');
  const def = state.meta.defaultProxy || '';
  $('#view').innerHTML = `
    <div class="section">
      <div class="section-head">
        <h3>SOCKS5 / HTTP 代理池</h3>
        <button class="btn primary" data-act="new-proxy"><b>+</b> 添加代理</button>
      </div>
      <div class="panel"><div class="panel-body">
        <div class="dim small">这里的代理用于访问「本地直连不通、必须走代理」的上游。全局默认代理：
          <b>${esc(proxyName(def))}</b>（可在设置里修改）</div>
      </div></div>
      <div class="panel" style="margin-top:12px"><div class="tbl-wrap"><table>
        <thead><tr><th>名称</th><th>类型</th><th>地址</th><th>用户名</th><th>超时</th><th></th></tr></thead>
        <tbody>${rows || '<tr><td colspan="6" class="empty">还没有配置上游代理，请点击右上角「添加代理」创建第一条 SOCKS5 / HTTP 代理</td></tr>'}</tbody>
      </table></div></div>
    </div>
    <div class="section">
      <div class="section-head"><h3>说明</h3></div>
      <div class="panel"><div class="panel-body dim small" style="line-height:1.9">
        · <b>socks5</b>：RFC 1928，支持无认证与用户名密码认证（RFC 1929），域名由代理端解析<br>
        · <b>http</b>：HTTP CONNECT 隧道代理<br>
        · 路由或节点选择「直连」即不经过任何代理<br>
        · 测试按钮会用该代理去连 <code>www.cloudflare.com:443</code>，验证代理本身是否可用
      </div></div>
    </div>`;
}

function openProxyEditor(p) {
  const isNew = !p;
  p = p || { id: '', name: '', type: 'socks5', addr: '', username: '', password: '', timeout: 10, note: '' };
  const body = `
    <div class="frow">
      <label>名称<input id="p_name" value="${esc(p.name)}" placeholder="家里 socks5"></label>
      <label>类型<select id="p_type">
        <option value="socks5"${p.type === 'socks5' ? ' selected' : ''}>SOCKS5</option>
        <option value="http"${p.type === 'http' ? ' selected' : ''}>HTTP CONNECT</option>
        <option value="direct"${p.type === 'direct' ? ' selected' : ''}>直连（占位）</option>
      </select></label>
    </div>
    <div class="frow">
      <label>代理地址<input id="p_addr" value="${esc(p.addr)}" placeholder="127.0.0.1:1080"></label>
      <label>超时（秒）<input id="p_timeout" type="number" value="${Number(p.timeout) || 10}"></label>
    </div>
    <div class="frow">
      <label>用户名（可留空）<input id="p_user" value="${esc(p.username)}"></label>
      <label>密码（可留空）<input id="p_pass" value="${esc(p.password)}"></label>
    </div>
    <label>备注<input id="p_note" value="${esc(p.note)}"></label>`;
  const m = openModal(isNew ? '添加上游代理' : '编辑上游代理', body,
    `<button class="btn" data-act="close">取消</button>
     <button class="btn" id="p_test">测试连通性</button>
     <button class="btn primary" id="p_save">保存</button>`);
  const gather = () => ({
    name: $('#p_name', m.root).value.trim(),
    type: $('#p_type', m.root).value,
    addr: $('#p_addr', m.root).value.trim(),
    username: $('#p_user', m.root).value.trim(),
    password: $('#p_pass', m.root).value,
    timeout: Number($('#p_timeout', m.root).value) || 10,
    note: $('#p_note', m.root).value.trim()
  });
  $('#p_test', m.root).addEventListener('click', async () => {
    const cfg = gather();
    if (!cfg.addr) { toast('请填写代理地址', 'err'); return; }
    const btn = $('#p_test', m.root); btn.disabled = true; btn.textContent = '测试中…';
    try {
      const res = await api('proxy-test', { method: 'POST', body: Object.assign({ target: 'www.cloudflare.com:443' }, cfg) });
      if (res.ok) toast(`代理可用，耗时 ${res.elapsed}`, 'ok');
      else toast('代理不可用：' + res.error, 'err');
    } catch (err) { toast(err.message, 'err'); }
    btn.disabled = false; btn.textContent = '测试连通性';
  });
  $('#p_save', m.root).addEventListener('click', async () => {
    const cfg = gather();
    if (!cfg.name) { toast('请填写名称', 'err'); return; }
    if (!cfg.addr) { toast('请填写代理地址', 'err'); return; }
    try {
      if (isNew) await api('proxies', { method: 'POST', body: cfg });
      else await api('proxies/' + p.id, { method: 'PUT', body: cfg });
      toast('已保存', 'ok'); m.close(); await refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
}

async function testProxy(id) {
  toast('正在测试…');
  try {
    const res = await api('proxy-test', { method: 'POST', body: { id, target: 'www.cloudflare.com:443' } });
    if (res.ok) toast(`代理可用，耗时 ${res.elapsed}`, 'ok');
    else toast('不可用：' + res.error, 'err');
  } catch (err) { toast(err.message, 'err'); }
}

/* ---------------- 访问日志 ---------------- */
async function renderLogs() {
  const logs = await api('logs?limit=200');
  state.logs = logs || [];
  const rows = state.logs.map(l => {
    const cls = 's-' + String(l.status || 0)[0];
    return `<tr>
      <td class="mono small dim">${fmtTime(l.time)}</td>
      <td class="mono">${esc(l.method)}</td>
      <td class="log-line mono" title="${esc(l.host + l.path)}">${esc(l.host + l.path)}</td>
      <td class="mono ${cls}">${l.status || '-'}</td>
      <td class="mono">${l.duration} ms</td>
      <td class="mono small dim">${esc(l.upstream || '-')}</td>
      <td class="mono small">${esc(l.viaProxy || '-')}</td>
      <td class="mono small dim">${esc(l.clientIp)}</td>
    </tr>`;
  }).join('');
  $('#view').innerHTML = `
    <div class="section">
      <div class="section-head">
        <h3>访问日志 <span class="dim small">（内存保留最近 ${state.meta && state.meta.logCount || 500} 条）</span></h3>
        <div style="display:flex;gap:8px">
          <button class="btn ${state.logsAuto ? 'primary' : ''}" data-act="toggle-logs-auto">自动刷新：${state.logsAuto ? '开' : '关'}</button>
          <button class="btn" data-act="clear-logs">清空</button>
        </div>
      </div>
      <div class="panel"><div class="tbl-wrap"><table>
        <thead><tr><th>时间</th><th>方法</th><th>请求</th><th>状态</th><th>耗时</th><th>上游</th><th>经代理</th><th>客户端</th></tr></thead>
        <tbody>${rows || '<tr><td colspan="8" class="empty">暂无访问记录</td></tr>'}</tbody>
      </table></div></div>
    </div>`;
}
function startLogTimer() {
  if (state.logTimer) clearInterval(state.logTimer);
  if (state.tab === 'logs' && state.logsAuto) state.logTimer = setInterval(renderLogs, 3000);
}

/* ---------------- 设置 ---------------- */
async function renderSettings() {
  const cfg = await api('config');
  window.__cfg = cfg;
  const g = cfg.global || {}, l = cfg.listen || {}, log = cfg.log || {}, t = cfg.tls || {};
  $('#view').innerHTML = `
    <div class="section">
      <div class="section-head"><h3>监听与端口</h3></div>
      <div class="panel"><div class="panel-body">
        <div class="frow three">
          <label>代理监听地址<input id="s_addr" value="${esc(l.addr)}" placeholder=":8080"><div class="hint">改后需重启进程</div></label>
          <label>管理后台独立端口<input id="s_adminAddr" value="${esc(l.adminAddr || '')}" placeholder="留空=与代理共用端口"><div class="hint">如 :8081，改后需重启</div></label>
          <label>管理路径前缀<input id="s_adminPath" value="${esc(l.adminPath || '/__admin')}"></label>
        </div>
        <div class="frow three">
          <label>读超时（秒）<input id="s_readTO" type="number" value="${Number(l.readTimeout) || 0}"><div class="hint">0=不限（WebSocket 需要）</div></label>
          <label>写超时（秒）<input id="s_writeTO" type="number" value="${Number(l.writeTimeout) || 0}"></label>
          <label>空闲超时（秒）<input id="s_idleTO" type="number" value="${Number(l.idleTimeout) || 120}"></label>
        </div>
      </div></div>
    </div>

    <div class="section">
      <div class="section-head"><h3>全局代理与超时</h3></div>
      <div class="panel"><div class="panel-body">
        <div class="frow">
          <label>全局默认代理<select id="s_defProxy">${proxyOptions(g.defaultProxy, true)}</select>
            <div class="hint">路由未指定代理时默认走它</div></label>
          <label>内置 /__echo 自测端点
            <select id="s_echo"><option value="1"${g.enableEcho ? ' selected' : ''}>开启</option><option value="0"${!g.enableEcho ? ' selected' : ''}>关闭</option></select></label>
        </div>
        <div class="frow three">
          <label>建连超时（秒）<input id="s_dialTO" type="number" value="${Number(g.dialTimeout) || 10}"></label>
          <label>响应超时（秒）<input id="s_respTO" type="number" value="${Number(g.responseTimeout) || 0}"><div class="hint">0=不限</div></label>
          <label>TCP 保活（秒）<input id="s_keepAlive" type="number" value="${Number(g.keepAlive) || 30}"></label>
        </div>
        <div class="frow three">
          <label>最大空闲连接<input id="s_maxIdle" type="number" value="${Number(g.maxIdleConns) || 200}"></label>
          <label>单主机最大连接<input id="s_maxHost" type="number" value="${Number(g.maxConnsPerHost) || 0}"><div class="hint">0=不限</div></label>
          <label>信任 X-Forwarded-For
            <select id="s_trust"><option value="0"${!g.trustProxy ? ' selected' : ''}>否（更安全）</option><option value="1"${g.trustProxy ? ' selected' : ''}>是（前面还有 CDN 时开启）</option></select></label>
        </div>
      </div></div>
    </div>

    <div class="section">
      <div class="section-head"><h3>日志</h3></div>
      <div class="panel"><div class="panel-body">
        <div class="frow three">
          <label>日志级别<select id="s_level">
            ${['debug', 'info', 'warn', 'error'].map(x => `<option value="${x}"${log.level === x ? ' selected' : ''}>${x}</option>`).join('')}
          </select></label>
          <label>记录访问日志<select id="s_access"><option value="1"${log.access !== false ? ' selected' : ''}>是</option><option value="0"${log.access === false ? ' selected' : ''}>否</option></select></label>
          <label>内存保留条数<input id="s_buffer" type="number" value="${Number(log.buffer) || 500}"></label>
        </div>
        <label>访问日志文件（留空=仅内存）<input id="s_logFile" value="${esc(log.file || '')}" placeholder="/var/log/revproxy/access.log"></label>
      </div></div>
    </div>

    <div class="section">
      <div class="section-head"><h3>HTTPS（对外提供 TLS）</h3></div>
      <div class="panel"><div class="panel-body">
        <label class="switch"><input id="s_tlsOn" type="checkbox"${t.enabled ? ' checked' : ''}> 启用 HTTPS（需重启生效）</label>
        <div class="frow">
          <label>证书文件 cert.pem<input id="s_cert" value="${esc(t.certFile || '')}" placeholder="/etc/revproxy/cert.pem"></label>
          <label>私钥文件 key.pem<input id="s_key" value="${esc(t.keyFile || '')}" placeholder="/etc/revproxy/key.pem"></label>
        </div>
        <div class="frow">
          <label>HTTP 自动跳转 HTTPS<select id="s_redirect"><option value="0"${!t.redirect ? ' selected' : ''}>否</option><option value="1"${t.redirect ? ' selected' : ''}>是</option></select></label>
          <label>HTTP 端口<input id="s_httpPort" type="number" value="${Number(t.httpPort) || 80}"></label>
        </div>
        <div class="hint">自动申请证书（Let's Encrypt）建议使用 Caddy / acme.sh 前置或反代；本程序支持直接加载已有证书文件。</div>
      </div></div>
    </div>

    <div class="section">
      <div class="section-head"><h3>账号与配置</h3></div>
      <div class="panel"><div class="panel-body">
        <div class="frow three">
          <label>当前用户名<input id="s_adminUser" value="${esc((cfg.admin && cfg.admin.username) || 'admin')}" disabled></label>
          <label>新密码（留空不改）<input id="s_newPass" type="password" placeholder="至少 6 位"></label>
          <label>原密码<input id="s_oldPass" type="password" placeholder="修改密码时填写"></label>
        </div>
        <div style="display:flex;gap:8px;flex-wrap:wrap;margin-top:10px">
          <button class="btn" data-act="change-pwd">修改密码</button>
          <button class="btn" data-act="export-cfg">导出配置</button>
          <button class="btn" data-act="import-cfg">导入配置</button>
          <input type="file" id="importFile" accept="application/json,.json" hidden>
          <button class="btn primary" data-act="save-settings">保存设置</button>
        </div>
        <div class="hint" style="margin-top:10px">配置文件路径：<code>${esc(state.meta.configPath || '')}</code>，也可直接编辑该文件，3 秒内自动热重载。</div>
      </div></div>
    </div>`;

  $('#importFile').addEventListener('change', async e => {
    const f = e.target.files[0];
    if (!f) return;
    try {
      const text = await f.text();
      const obj = JSON.parse(text);
      if (!confirm('导入会覆盖当前全部配置，确定继续？')) return;
      await api('import', { method: 'POST', body: { config: obj } });
      toast('配置已导入', 'ok'); await refresh(); renderSettings();
    } catch (err) { toast('导入失败：' + err.message, 'err'); }
    e.target.value = '';
  });
}

async function saveSettings() {
  const c = window.__cfg || {};
  const num = id => Number($(id).value) || 0;
  const body = {
    listen: {
      addr: $('#s_addr').value.trim() || ':8080',
      adminAddr: $('#s_adminAddr').value.trim(),
      adminPath: $('#s_adminPath').value.trim() || '/__admin',
      readTimeout: num('#s_readTO'), writeTimeout: num('#s_writeTO'), idleTimeout: num('#s_idleTO'),
      maxHeaderKB: (c.listen && c.listen.maxHeaderKB) || 64
    },
    log: {
      level: $('#s_level').value,
      access: $('#s_access').value === '1',
      file: $('#s_logFile').value.trim(),
      buffer: num('#s_buffer') || 500
    },
    global: {
      defaultProxy: $('#s_defProxy').value,
      enableEcho: $('#s_echo').value === '1',
      dialTimeout: num('#s_dialTO') || 10,
      responseTimeout: num('#s_respTO'),
      keepAlive: num('#s_keepAlive') || 30,
      maxIdleConns: num('#s_maxIdle') || 200,
      maxConnsPerHost: num('#s_maxHost'),
      trustProxy: $('#s_trust').value === '1',
      hideVersion: c.global && c.global.hideVersion
    },
    tls: {
      enabled: $('#s_tlsOn').checked,
      certFile: $('#s_cert').value.trim(),
      keyFile: $('#s_key').value.trim(),
      redirect: $('#s_redirect').value === '1',
      httpPort: num('#s_httpPort') || 80
    }
  };
  try {
    const res = await api('settings', { method: 'PUT', body });
    toast(res.message + (res.needRestart ? '（监听地址已变，重启后生效）' : ''), res.needRestart ? 'warn' : 'ok');
    await refresh();
  } catch (err) { toast(err.message, 'err'); }
}

async function changePassword() {
  const oldPass = $('#s_oldPass').value;
  const newPass = $('#s_newPass').value;
  if (!newPass) { toast('请填写新密码', 'warn'); return; }
  if (!oldPass) { toast('请填写原密码', 'warn'); return; }
  try {
    await api('password', { method: 'POST', body: { old: oldPass, new: newPass } });
    toast('密码已修改，请牢记新密码', 'ok');
    $('#s_oldPass').value = ''; $('#s_newPass').value = '';
  } catch (err) { toast(err.message, 'err'); }
}

/* ---------------- 启动 ---------------- */
(async function boot() {
  try {
    const me = await api('me');
    state.user = me.user;
    $('#userChip').textContent = me.user;
    showApp();
    await refresh();
    state.timer = setInterval(() => {
      if (state.tab === 'overview' && document.visibilityState === 'visible') {
        refresh().catch(() => { });
      }
    }, 5000);
  } catch (e) {
    showLogin();
    if (e.message && !/未登录/.test(e.message)) toast(e.message, 'err');
  }
})();
