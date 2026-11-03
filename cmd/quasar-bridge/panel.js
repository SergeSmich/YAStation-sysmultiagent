'use strict';

const STATE_URL = '/api/panel/state';
const SESSION_URL = '/api/panel/session';
const TURNS_URL = '/api/panel/turns?limit=50';
const LOGS_URL = '/api/panel/logs?lines=200';
const VOICE_URL = '/api/panel/voice';
const VOICE_TEST_URL = '/api/panel/voice/test';
const SETTINGS_URL = '/api/panel/settings';
const WAKE_DICTIONARY_URL = '/api/panel/wake/dictionary';
const LLM_TEST_URL = '/api/panel/llm/test';
const LLM_MODELS_URL = '/api/panel/llm/models';
const LLM_DETECT_URL = '/api/panel/llm/detect';
const LLM_CHECK_URL = '/api/panel/llm/check';
const PASSWORD_URL = '/api/panel/password';
const ROUTING_TEST_URL = '/api/panel/routing/test';
const UPDATE_URL = '/api/panel/update';
const STATION_VOLUME_URL = '/api/panel/station/volume';
const METRICS_URL = '/api/panel/metrics?range=';
const REFRESH_STATE_MS = 5000;
const REFRESH_LOGS_MS = 15000;
const REFRESH_METRICS_MS = 10000;

let csrfToken = '';
let voiceFormDirty = false;
let metricsRange = 3600;
let agentsState = [];
let savedAgentNames = [];
let providersState = [];
let providerKeysState = {};
// savedProviders maps a provider name to the payload the file holds right now.
// The probe endpoints read the file, so an unsaved row — or a row with unsaved
// edits — cannot be tested and the page must say why instead of forwarding a
// name the bridge does not know or silently testing the old values.
let savedProviders = new Map();
let providerModels = {};
let llmPresetsState = [];
let wakeDictionary = [];
let wakeDictionaryNote = '';
let wakePage = '';
let wordsState = [];
let settingsActions = ['listen', 'ask', 'command'];
let settingsVoices = [];
// routingState is the working copy of the routing section; routingPresets holds
// the ready-made policies the bridge defines, so the page never invents one.
let routingState = null;
let routingPresets = [];

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

async function getJSON(url) {
  const response = await fetch(url, {
    credentials: 'same-origin',
    headers: { 'Accept': 'application/json' },
    cache: 'no-store'
  });
  if (response.status === 401) {
    window.location.replace('/login');
    throw new Error('unauthorized');
  }
  if (!response.ok) {
    throw new Error(url + ' -> ' + response.status);
  }
  return response.json();
}

async function postJSON(url, body) {
  const response = await fetch(url, {
    method: 'POST',
    credentials: 'same-origin',
    headers: {
      'Accept': 'application/json',
      'Content-Type': 'application/json',
      'X-Panel-CSRF': csrfToken
    },
    cache: 'no-store',
    body: JSON.stringify(body)
  });
  if (response.status === 401) {
    window.location.replace('/login');
    throw new Error('unauthorized');
  }
  let payload = null;
  try {
    payload = await response.json();
  } catch (error) {
    payload = null;
  }
  if (!response.ok) {
    const message = (payload && (payload.error || payload.message)) || ('ошибка ' + response.status);
    throw new Error(message);
  }
  return payload;
}

function card(label, value, note, tone) {
  const node = el('div', 'card');
  node.appendChild(el('div', 'label', label));
  const val = el('div', 'value' + (tone ? ' ' + tone : ''), value);
  node.appendChild(val);
  if (note) node.appendChild(el('div', 'note', note));
  return node;
}

function yesNo(value) {
  return value ? 'да' : 'нет';
}

function shortTime(iso) {
  if (!iso) return '—';
  const date = new Date(iso);
  if (isNaN(date.getTime())) return '—';
  return date.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

function age(ms) {
  if (!ms || ms <= 0) return '—';
  const seconds = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (seconds < 60) return seconds + ' с назад';
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return minutes + ' мин назад';
  return Math.round(minutes / 60) + ' ч назад';
}

function renderState(payload) {
  const bridge = payload.bridge || {};
  const voice = payload.voice || {};
  const local = voice.local_voice || {};
  const cards = document.getElementById('cards');
  cards.replaceChildren();

  cards.appendChild(card('Мост', bridge.status === 'ok' ? 'работает' : String(bridge.status || '—'),
    'режим: ' + (bridge.mode || '—'), bridge.status === 'ok' ? 'ok' : 'err'));
  cards.appendChild(card('Станция', bridge.client_frames + ' / ' + bridge.server_frames, 'кадров клиент / сервер'));
  cards.appendChild(card('Маршруты', String(bridge.routing_rules || 0), 'ревизия ' + String(bridge.routing_revision || '—').slice(0, 12)));
  cards.appendChild(card('Голос', local.voice || 'Алиса', 'ревизия ' + (voice.revision ?? '—'), local.voice ? 'ok' : ''));
  cards.appendChild(card('Зависания upstream', String(bridge.upstream_stalls_total ?? 0),
    'последний кадр: ' + age(bridge.upstream_last_frame_ms), bridge.upstream_stalls_total ? 'err' : 'ok'));
  cards.appendChild(card('Сбои ping', String(bridge.upstream_ping_failures_total ?? 0),
    (bridge.upstream_ping_failures_total ? 'проверьте соединение' : 'соединение живое'),
    bridge.upstream_ping_failures_total ? 'warn' : 'ok'));
  cards.appendChild(card('Журнал диалогов', yesNo(bridge.conversation_log_enabled),
    'сбоев записи: ' + (bridge.conversation_log_failures ?? 0),
    bridge.conversation_log_failures ? 'warn' : ''));
  cards.appendChild(card('Инструменты ассистента', yesNo(bridge.assistant_tools_enabled),
    'контекст диалога: ' + yesNo(bridge.dialogue_context_enabled)));
  cards.appendChild(card('Дом', yesNo(bridge.home_assistant_enabled), 'MCP серверов: ' + (bridge.mcp_servers_configured ?? 0)));
  cards.appendChild(card('Музыка', yesNo(bridge.music_playback_enabled), 'поток /music-stream/'));

  const voiceBox = document.getElementById('voice');
  voiceBox.replaceChildren();
  const kv = el('dl', 'kv');
  const rows = [
    ['Голос', local.voice || 'штатный голос Алисы'],
    ['Кандидаты', (local.candidates || []).join(', ') || '—'],
    ['Эмоция', local.emotion || '—'],
    ['Скорость', local.speed ? String(local.speed) : '—'],
    ['Громкость', local.volume ? String(local.volume) : '—'],
    ['Маршруты', Object.keys(local.routes || {}).length ? JSON.stringify(local.routes) : '—'],
    ['Ревизия', String(voice.revision ?? '—')],
    ['Доступные', (voice.supported || []).join(', ') || '—']
  ];
  for (const [name, value] of rows) {
    kv.appendChild(el('dt', null, name));
    kv.appendChild(el('dd', null, value));
  }
  voiceBox.appendChild(kv);
  syncVoiceForm(local, voice.supported);
}

// syncVoiceForm mirrors the live settings into the edit form, unless the
// operator is typing in it: a refresh must never overwrite a half-typed value.
function syncVoiceForm(local, supported) {
  const select = document.getElementById('voice-name');
  if (!select.dataset.filled) {
    const fallback = el('option', null, 'штатный голос Алисы');
    fallback.value = '';
    select.appendChild(fallback);
    for (const name of supported || []) {
      const option = el('option', null, name);
      option.value = name;
      select.appendChild(option);
    }
    select.dataset.filled = '1';
  }
  const form = document.getElementById('voice-form');
  if (voiceFormDirty || form.contains(document.activeElement)) return;
  select.value = local.voice || '';
  document.getElementById('voice-candidates').value = (local.candidates || []).join(', ');
  document.getElementById('voice-speed').value = local.speed ? String(local.speed) : '';
  document.getElementById('voice-volume').value = local.volume ? String(local.volume) : '';
  syncVoiceSliders();
}

// The speed and volume fields have a slider twin. The number stays the source
// of truth (an empty number means "do not change"); the slider only edits it.
function paintSlider(range) {
  const min = Number(range.min) || 0;
  const max = Number(range.max) || 100;
  const fill = ((Number(range.value) - min) / (max - min)) * 100;
  range.style.setProperty('--fill', Math.max(0, Math.min(100, fill)) + '%');
}

function syncVoiceSliders() {
  for (const id of ['voice-speed', 'voice-volume']) {
    const number = document.getElementById(id);
    const range = document.getElementById(id + '-range');
    if (!number || !range) continue;
    range.value = number.value === '' ? '1' : number.value;
    paintSlider(range);
  }
}

function initVoiceSliders() {
  for (const id of ['voice-speed', 'voice-volume']) {
    const number = document.getElementById(id);
    const range = document.getElementById(id + '-range');
    if (!number || !range) continue;
    range.addEventListener('input', () => { number.value = range.value; paintSlider(range); });
    number.addEventListener('input', () => {
      if (number.value !== '') range.value = number.value;
      paintSlider(range);
    });
  }
  syncVoiceSliders();
}

function renderTurns(payload) {
  const box = document.getElementById('turns');
  box.replaceChildren();
  const turns = payload.turns || [];
  if (!payload.enabled) {
    box.textContent = 'Журнал диалогов выключен в конфиге.';
    return;
  }
  if (!turns.length) {
    box.textContent = payload.error ? payload.error : 'Пока нет записанных ходов.';
    return;
  }
  const table = el('table');
  const head = el('thead');
  const headRow = el('tr');
  for (const title of ['Время', 'Маршрут', 'Вопрос', 'Ответ', 'мс', 'Доставлено']) {
    headRow.appendChild(el('th', null, title));
  }
  head.appendChild(headRow);
  table.appendChild(head);
  const body = el('tbody');
  for (const turn of turns) {
    const row = el('tr');
    row.appendChild(el('td', 'time', shortTime(turn.time)));
    const routeCell = el('td');
    routeCell.appendChild(el('span', 'badge ' + (turn.route || ''), turn.route || '—'));
    row.appendChild(routeCell);
    row.appendChild(el('td', 'q', turn.speech || '—'));
    row.appendChild(el('td', 'a', turn.answer || '—'));
    row.appendChild(el('td', 'meta', turn.elapsed_ms ? String(turn.elapsed_ms) : '—'));
    row.appendChild(el('td', 'meta', turn.delivered ? 'да' : '—'));
    body.appendChild(row);
  }
  table.appendChild(body);
  box.appendChild(table);
  if (payload.truncated) {
    box.appendChild(el('p', 'muted small', 'Показаны только последние записи файла журнала.'));
  }
}

function renderLogs(payload) {
  const box = document.getElementById('logs');
  document.getElementById('log-path').textContent = payload.path || '';
  const lines = payload.lines || [];
  box.replaceChildren();
  if (payload.error) {
    box.textContent = payload.error;
    return;
  }
  if (!lines.length) {
    box.textContent = document.getElementById('warn-only').checked ? 'Записей WARN/ERROR нет.' : 'Лог пуст.';
    return;
  }
  for (const line of lines) {
    let cls = '';
    const upper = line.toUpperCase();
    if (upper.includes('"LEVEL":"ERROR"') || upper.includes('PANIC')) cls = 'line-err';
    else if (upper.includes('"LEVEL":"WARN"')) cls = 'line-warn';
    const row = el('span', cls, line);
    box.appendChild(row);
    box.appendChild(document.createTextNode('\n'));
  }
}

const SVG_NS = 'http://www.w3.org/2000/svg';

// CHART_SPECS is the whole station view: one small SVG per metric, so a broken
// series only affects its own chart.
const CHART_SPECS = [
  { key: 'cpu', title: 'Процессор', unit: '%', min: 0, max: 100, digits: 0, color: '#5aa2e8' },
  { key: 'temp', title: 'Температура', unit: '°C', digits: 0, color: '#e8734a' },
  { key: 'mem', title: 'Память', unit: 'МБ', digits: 0, color: '#a78bfa', total: 'mem' },
  { key: 'disk', title: 'Диск', unit: 'МБ', digits: 0, color: '#4dd4ac', total: 'disk' },
  { key: 'load', title: 'Load average', unit: '', min: 0, digits: 2, color: '#e0b341' },
  { key: 'rssi', title: 'Wi-Fi', unit: 'dBm', digits: 0, color: '#4bc0d9' },
  { key: 'rx', title: 'Приём', unit: 'кбит/с', min: 0, digits: 0, color: '#5aa2e8' },
  { key: 'tx', title: 'Передача', unit: 'кбит/с', min: 0, digits: 0, color: '#e2665c' }
];

function svgEl(tag, attrs) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [name, value] of Object.entries(attrs || {})) node.setAttribute(name, String(value));
  return node;
}

function formatValue(value, digits) {
  if (typeof value !== 'number' || !isFinite(value)) return '—';
  return value.toLocaleString('ru-RU', { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

function formatDuration(seconds) {
  if (typeof seconds !== 'number' || !isFinite(seconds) || seconds < 0) return '—';
  const total = Math.round(seconds);
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (days > 0) return days + ' сут ' + hours + ' ч';
  if (hours > 0) return hours + ' ч ' + minutes + ' мин';
  return minutes + ' мин';
}

function clockLabel(unix, rangeSeconds) {
  if (typeof unix !== 'number' || !isFinite(unix)) return '';
  const date = new Date(unix * 1000);
  if (isNaN(date.getTime())) return '';
  if (rangeSeconds > 21600) {
    return date.toLocaleString('ru-RU', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
  }
  return date.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
}

// niceStep picks a round grid step for the axis, so the labels read 0, 25, 50
// rather than 0, 23.7, 47.4.
function niceStep(raw) {
  if (!(raw > 0)) return 1;
  const power = Math.pow(10, Math.floor(Math.log10(raw)));
  for (const factor of [1, 2, 2.5, 5, 10]) {
    if (raw <= factor * power) return factor * power;
  }
  return 10 * power;
}

// drawChart renders one series as an SVG line. Missing values break the line:
// the panel never draws a straight segment over a gap in the data.
function drawChart(spec, values, times, rangeSeconds, totals) {
  const width = 640;
  const height = 148;
  const pad = { left: 56, right: 12, top: 26, bottom: 18 };
  const plotWidth = width - pad.left - pad.right;
  const plotHeight = height - pad.top - pad.bottom;
  const svg = svgEl('svg', { viewBox: '0 0 ' + width + ' ' + height, class: 'chart', role: 'img' });

  const caption = svgEl('text', { x: pad.left, y: 12, fill: '#8593a3', 'font-size': 11, 'font-weight': 600 });
  caption.textContent = spec.title + (spec.unit ? ', ' + spec.unit : '');
  svg.appendChild(caption);

  const numbers = values.map((value) => (typeof value === 'number' && isFinite(value) ? value : null));
  const present = numbers.filter((value) => value !== null);
  const xOf = (index) => {
    if (numbers.length <= 1) return pad.left + plotWidth / 2;
    return pad.left + (index * plotWidth) / (numbers.length - 1);
  };

  if (!present.length) {
    const note = svgEl('text', { x: pad.left + plotWidth / 2, y: pad.top + plotHeight / 2, fill: '#8593a3', 'font-size': 12, 'text-anchor': 'middle' });
    note.textContent = 'нет данных';
    svg.appendChild(note);
    return svg;
  }

  let low = Math.min(...present);
  let high = Math.max(...present);
  if (spec.min !== undefined) low = Math.min(low, spec.min);
  if (spec.max !== undefined) high = Math.max(high, spec.max);
  if (high - low < 1e-6) high = low + 1;
  const tick = niceStep((high - low) / 3);
  low = Math.floor(low / tick) * tick;
  high = Math.ceil(high / tick) * tick;
  if (high <= low) high = low + tick;
  const yOf = (value) => pad.top + plotHeight - ((value - low) / (high - low)) * plotHeight;

  for (let value = low; value <= high + tick / 2; value += tick) {
    const y = yOf(value);
    svg.appendChild(svgEl('line', { x1: pad.left, x2: pad.left + plotWidth, y1: y, y2: y, stroke: '#2a333f', 'stroke-width': 1 }));
    const label = svgEl('text', { x: pad.left - 8, y: y + 3, fill: '#8593a3', 'font-size': 10, 'text-anchor': 'end' });
    label.textContent = formatValue(value, tick < 1 ? 2 : 0);
    svg.appendChild(label);
  }

  let path = '';
  let open = false;
  numbers.forEach((value, index) => {
    if (value === null) {
      open = false;
      return;
    }
    path += (open ? ' L' : ' M') + xOf(index).toFixed(1) + ' ' + yOf(value).toFixed(1);
    open = true;
  });
  if (path) {
    svg.appendChild(svgEl('path', {
      d: path.trim(), fill: 'none', stroke: spec.color, 'stroke-width': 1.6,
      'stroke-linejoin': 'round', 'stroke-linecap': 'round'
    }));
  }

  let lastIndex = -1;
  for (let i = numbers.length - 1; i >= 0; i--) {
    if (numbers[i] !== null) {
      lastIndex = i;
      break;
    }
  }
  if (lastIndex >= 0) {
    const last = numbers[lastIndex];
    svg.appendChild(svgEl('circle', { cx: xOf(lastIndex), cy: yOf(last), r: 2.6, fill: spec.color }));
    let text = formatValue(last, spec.digits) + (spec.unit ? ' ' + spec.unit : '');
    const total = spec.total && totals ? totals[spec.total] : 0;
    if (total > 0) text += ' · ' + formatValue((last / total) * 100, 0) + '%';
    const now = svgEl('text', { x: pad.left + plotWidth, y: 12, fill: spec.color, 'font-size': 11, 'font-weight': 600, 'text-anchor': 'end' });
    now.textContent = text;
    svg.appendChild(now);
  }

  const marks = Array.from(new Set([0, Math.floor((numbers.length - 1) / 2), numbers.length - 1]));
  for (const index of marks) {
    const anchor = index === 0 ? 'start' : (index === numbers.length - 1 ? 'end' : 'middle');
    const label = svgEl('text', { x: xOf(index), y: height - 5, fill: '#8593a3', 'font-size': 10, 'text-anchor': anchor });
    label.textContent = clockLabel(times[index], rangeSeconds);
    svg.appendChild(label);
  }
  return svg;
}

function renderMetrics(doc) {
  const alerts = (doc.alerts || []).concat(doc.bridge_alerts || []);
  const alertBox = document.getElementById('station-alerts');
  alertBox.replaceChildren();
  for (const alert of alerts) {
    alertBox.appendChild(el('span', 'alert ' + (alert.level === 'critical' ? 'critical' : 'warn'), alert.text));
  }
  if (!alerts.length) alertBox.appendChild(el('span', 'muted small', 'порогов нет'));
  document.getElementById('station-uptime').textContent =
    'uptime ' + formatDuration(doc.uptime) + ' · точек: ' + (doc.points ?? 0);

  const times = doc.t || [];
  const totals = doc.totals || {};
  const range = doc.range || metricsRange;
  const charts = document.getElementById('charts');
  charts.replaceChildren();
  for (const spec of CHART_SPECS) {
    const box = el('figure', 'chart-box');
    box.appendChild(drawChart(spec, doc[spec.key] || [], times, range, totals));
    charts.appendChild(box);
  }

  const events = (doc.events || []).slice(-6).reverse();
  const eventBox = document.getElementById('station-events');
  if (!events.length) {
    eventBox.textContent = 'Событий пока нет.';
    return;
  }
  eventBox.textContent = 'События: ' + events
    .map((event) => clockLabel(event.t, range) + ' — ' + event.text)
    .join(' · ');
}

async function refreshState() {
  const payload = await getJSON(STATE_URL);
  renderState(payload);
  document.getElementById('stamp').textContent = new Date().toLocaleTimeString('ru-RU');
}

async function refreshTurns() {
  renderTurns(await getJSON(TURNS_URL));
}

async function refreshMetrics() {
  renderMetrics(await getJSON(METRICS_URL + metricsRange));
}

async function refreshMetricsSafe() {
  try {
    await refreshMetrics();
  } catch (error) {
    console.error(error);
  }
}

async function refreshLogs() {
  const warnOnly = document.getElementById('warn-only').checked;
  renderLogs(await getJSON(LOGS_URL + (warnOnly ? '&level=WARN' : '')));
}

async function refreshAll() {
  try {
    await Promise.all([refreshState(), refreshTurns()]);
    document.getElementById('turns').classList.remove('stale');
  } catch (error) {
    console.error(error);
    document.getElementById('stamp').textContent = 'нет связи';
  }
}

async function refreshLogsSafe() {
  try {
    await refreshLogs();
  } catch (error) {
    console.error(error);
  }
}

function parseCandidates(text) {
  return text.split(',').map((part) => part.trim()).filter((part) => part.length > 0);
}

const ACTION_LABELS = {
  listen: 'открыть микрофон',
  ask: 'вопрос роли',
  command: 'команда'
};

function settingsField(labelText, control, extraClass) {
  const field = el('label', 'field' + (extraClass ? ' ' + extraClass : ''));
  field.appendChild(el('span', null, labelText));
  field.appendChild(control);
  return field;
}

function option(value, label) {
  const item = el('option', null, label);
  item.value = value;
  return item;
}

function settingsControl(tag, field, value, placeholder) {
  const control = el(tag);
  control.dataset.field = field;
  control.value = value == null ? '' : String(value);
  if (placeholder) control.placeholder = placeholder;
  return control;
}

// PROVIDER_APIS are the two wire formats the bridge speaks. The value goes into
// llm_providers[].api, so it is shown verbatim instead of a translated label:
// the operator copies it from the provider's documentation.
const PROVIDER_APIS = ['chat_completions', 'responses', 'gigachat'];

// providerPayload builds the object the write path expects. It is an explicit
// whitelist for two reasons: the transient fields the page keeps on the object
// (typed key, forget flag, fetched model list) must never reach config.json, and
// the decoder there rejects unknown members.
function providerPayload(provider) {
  const payload = {
    name: provider.name || '',
    api: provider.api || 'chat_completions',
    base_url: provider.base_url || '',
    model: provider.model || ''
  };
  if (provider.reasoning_effort) payload.reasoning_effort = provider.reasoning_effort;
  if (provider.max_output_tokens) payload.max_output_tokens = Number(provider.max_output_tokens) || 0;
  if (Array.isArray(provider.hosted_tools) && provider.hosted_tools.length) {
    payload.hosted_tools = provider.hosted_tools;
  }
  // An empty field keeps the stored key: the page never receives it back, so it
  // cannot send it again.
  if (provider._key) payload.api_key = provider._key;
  if (provider._clearKey) payload.clear_key = true;
  return payload;
}

// modelIdsFor returns the model list the page knows for a provider name. The
// list comes from the file the bridge keeps (llm_models.json), so it survives a
// reload; «Применить» and «Проверить» refresh it.
function modelIdsFor(name) {
  return (name && providerModels[name]) || [];
}

const COMBO_LIMIT = 200;
let comboOutsideBound = false;

function closeCombos() {
  for (const menu of document.querySelectorAll('.combo-menu:not([hidden])')) menu.hidden = true;
}

// modelCombo is one control: a text field the operator can type a model into
// and an arrow that opens the list with a filter box on top. A datalist was not
// enough, it only shows the entries that match what is already typed.
// getIds is read on every open, so a list refreshed meanwhile is picked up.
function modelCombo(field, value, placeholder, getIds) {
  const wrap = el('div', 'combo model-combo');
  const input = settingsControl('input', field, value, placeholder);
  input.type = 'text';
  input.autocomplete = 'off';
  const toggle = el('button', 'combo-toggle', '▾');
  toggle.type = 'button';
  toggle.title = 'Список моделей';
  toggle.setAttribute('aria-label', 'Открыть список моделей');
  const menu = el('div', 'combo-menu');
  menu.hidden = true;
  const filter = el('input', 'combo-filter');
  filter.type = 'text';
  filter.autocomplete = 'off';
  const items = el('div', 'combo-items');
  menu.append(filter, items);
  wrap.append(input, toggle, menu);

  const visible = () => {
    const query = filter.value.trim().toLowerCase();
    const ids = getIds() || [];
    return { ids, found: query ? ids.filter((id) => id.toLowerCase().includes(query)) : ids };
  };
  const pick = (id) => {
    input.value = id;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new Event('change', { bubbles: true }));
    menu.hidden = true;
  };
  const render = () => {
    const { ids, found } = visible();
    filter.placeholder = ids.length ? 'Фильтр, например free (' + ids.length + ')' : 'Фильтр';
    items.replaceChildren();
    if (!ids.length) {
      items.appendChild(el('div', 'combo-empty', 'Список пуст — введите ключ и нажмите «Применить» или «Проверить»'));
      return;
    }
    if (!found.length) {
      items.appendChild(el('div', 'combo-empty', 'Ничего не найдено'));
      return;
    }
    for (const id of found.slice(0, COMBO_LIMIT)) {
      const item = el('button', 'combo-item', id);
      item.type = 'button';
      if (id === input.value) item.classList.add('is-current');
      item.addEventListener('click', () => pick(id));
      items.appendChild(item);
    }
    if (found.length > COMBO_LIMIT) {
      items.appendChild(el('div', 'combo-empty', 'ещё ' + (found.length - COMBO_LIMIT) + ' — уточните фильтр'));
    }
  };
  toggle.addEventListener('click', () => {
    if (!menu.hidden) {
      menu.hidden = true;
      return;
    }
    closeCombos();
    filter.value = '';
    render();
    menu.hidden = false;
    filter.focus();
  });
  filter.addEventListener('input', render);
  filter.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      // Enter in a field saves its section; here it picks the first match.
      event.preventDefault();
      event.stopPropagation();
      const { found } = visible();
      if (found.length) pick(found[0]);
    } else if (event.key === 'Escape') {
      menu.hidden = true;
      input.focus();
    }
  });
  if (!comboOutsideBound) {
    comboOutsideBound = true;
    document.addEventListener('click', (event) => {
      if (!event.target.closest('.combo')) closeCombos();
    });
  }
  return { wrap, input };
}

function comboField(labelText, wrap) {
  const field = el('div', 'field');
  field.appendChild(el('span', null, labelText));
  field.appendChild(wrap);
  return field;
}

// firstUsableModel picks the model to fill an empty field with: the first one
// that is not an embedding, speech or moderation model.
function firstUsableModel(ids) {
  const skip = /embed|whisper|tts|guard|moderat|rerank|image|vision-preview/i;
  return (ids || []).find((id) => !skip.test(id)) || '';
}

// setKeyNote shows the answer of the station under the key field of this very
// row, not in the shared status line of the header.
function setKeyNote(row, text, kind) {
  const note = row && row.querySelector('.key-note');
  if (!note) return;
  note.textContent = text || '';
  note.classList.remove('ok', 'error');
  if (kind) note.classList.add(kind);
}

// applyProviderGuess puts what the bridge recognised from the key into the row.
// It writes the state and the visible controls instead of re-rendering: the
// operator may be typing in another field of the same row.
function applyProviderGuess(provider, row, payload) {
  if (!payload || !payload.detected) return false;
  if (!String(provider.name || '').trim() && payload.provider) provider.name = payload.provider;
  if (payload.base_url) provider.base_url = payload.base_url;
  if (payload.api) provider.api = payload.api;
  const ids = Array.isArray(payload.models) ? payload.models : [];
  if (ids.length) providerModels[provider.name] = ids;
  if (ids.length && !String(provider.model || '').trim()) provider.model = firstUsableModel(ids);
  if (row) {
    const set = (field, value) => {
      const control = row.querySelector('[data-field="' + field + '"]');
      if (control && value != null) control.value = String(value);
    };
    set('name', provider.name);
    set('base_url', provider.base_url);
    set('api', provider.api);
    set('model', provider.model);
    if (provider.name) {
      row.dataset.syncedName = provider.name;
      syncProviderSelects();
    }
    updateKeyBadge(row, provider);
  }
  return true;
}

// applyProviderKey asks the station to recognise the service from the key (or
// from the chosen preset), fills the row and reports under the key field. The
// key is sent once per distinct value unless the operator presses «Применить».
async function applyProviderKey(provider, row, force) {
  const key = String(provider._key || '').trim();
  if (!key) {
    setKeyNote(row, 'Введите ключ, затем нажмите «Применить»', 'error');
    return null;
  }
  const signature = key + '|' + (provider._preset || '');
  if (!force && provider._guessedKey === signature) return provider._guess || null;
  provider._guessedKey = signature;
  setKeyNote(row, 'Определяю сервис по ключу…', '');
  try {
    const payload = await postJSON(LLM_DETECT_URL, {
      api_key: key,
      preset: provider._preset || '',
      provider: String(provider.name || '').trim().toLowerCase()
    });
    provider._guess = payload;
    if (!payload || !payload.detected) {
      setKeyNote(row, (payload && payload.note) || 'Сервис не распознан по ключу — выберите его в списке «Сервис» или впишите адрес вручную', 'error');
      return payload;
    }
    applyProviderGuess(provider, row, payload);
    const count = modelIdsFor(provider.name).length;
    if (payload.verified) {
      setKeyNote(row, 'Применено: ' + (payload.provider || provider.name) + ', моделей: ' + count + '. Нажмите «Сохранить».', 'ok');
    } else {
      setKeyNote(row, 'Адрес заполнен (' + (payload.provider || provider.name) + '), но сервис не ответил: ' +
        (payload.note || 'нет ответа'), 'error');
    }
    return payload;
  } catch (error) {
    provider._guessedKey = '';
    setKeyNote(row, error.message, 'error');
    return null;
  }
}

function updateKeyBadge(row, provider) {
  const badge = row.querySelector('.key-state');
  if (!badge) return;
  const saved = Boolean(providerKeysState[provider.name]) && !provider._clearKey;
  const typed = Boolean(provider._key);
  if (provider._clearKey) {
    badge.textContent = 'ключ будет удалён';
  } else if (typed) {
    badge.textContent = 'новый ключ — сохранится';
  } else if (saved) {
    badge.textContent = 'ключ сохранён';
  } else {
    badge.textContent = 'ключа нет';
  }
  badge.classList.toggle('set', saved || typed);
}

function providerButton(label, action) {
  const button = el('button', 'ghost', label);
  button.type = 'button';
  button.dataset.providerAction = action;
  return button;
}

function renderProviders(force) {
  const list = document.getElementById('llm-providers-list');
  if (!force && list.contains(document.activeElement)) return;
  list.replaceChildren();
  if (!providersState.length) {
    list.appendChild(el('p', 'muted small', 'Пока нет подключений. Добавьте первую нейросеть.'));
    return;
  }
  providersState.forEach((provider, index) => {
    const row = el('div', 'settings-row form');
    row.dataset.kind = 'provider';
    row.dataset.index = String(index);
    row.dataset.syncedName = provider.name || '';

    // A named service fills the address and the format; the key alone cannot
    // tell Qwen from DeepSeek, so the operator picks those here.
    const preset = el('select', 'preset-pick');
    preset.setAttribute('aria-label', 'Сервис');
    preset.appendChild(option('', 'выбрать сервис…'));
    for (const item of llmPresetsState) preset.appendChild(option(item.id, item.label));
    preset.value = provider._preset || '';
    preset.addEventListener('change', () => {
      const chosen = llmPresetsState.find((item) => item.id === preset.value);
      provider._preset = preset.value;
      provider._guessedKey = '';
      if (!chosen) return;
      const set = (field, value) => {
        const control = row.querySelector('[data-field="' + field + '"]');
        if (control) control.value = value;
      };
      if (!String(provider.name || '').trim()) {
        provider.name = chosen.name;
        set('name', chosen.name);
      }
      provider.base_url = chosen.base_url;
      provider.api = chosen.api;
      set('base_url', chosen.base_url);
      set('api', chosen.api);
      syncProviderSelects();
      updateKeyBadge(row, provider);
      setKeyNote(row, 'Адрес и формат заполнены. Введите ключ и нажмите «Применить».', '');
    });
    row.appendChild(settingsField('Сервис', preset, 'wide'));

    const name = settingsControl('input', 'name', provider.name, 'например, deepseek');
    name.type = 'text';
    name.autocomplete = 'off';
    // The stored form is lower case, and the name doubles as the credential key:
    // fold what the operator types instead of rejecting "DeepSeek" later.
    name.addEventListener('input', () => {
      const folded = name.value.replace(/[A-Z]/g, (letter) => letter.toLowerCase());
      if (folded === name.value) return;
      const caret = name.selectionStart;
      name.value = folded;
      name.setSelectionRange(caret, caret);
    });
    const nameField = settingsField('Название', name);
    nameField.appendChild(el('span', 'muted small', 'латиница, цифры, «-», «_», до 32 символов'));
    row.appendChild(nameField);

    const api = settingsControl('select', 'api', provider.api || 'chat_completions');
    for (const value of PROVIDER_APIS) api.appendChild(option(value, value));
    api.value = provider.api || 'chat_completions';
    const apiField = settingsField('Формат API', api);
    apiField.appendChild(el('span', 'muted small',
      'chat_completions = POST /chat/completions (DeepSeek, OpenRouter, OpenAI, …); ' +
      'responses = POST /responses (Responses API, только у OpenAI и совместимых)'));
    row.appendChild(apiField);

    const base = settingsControl('input', 'base_url', provider.base_url, 'https://api.deepseek.com/v1');
    base.type = 'text';
    base.autocomplete = 'off';
    row.appendChild(settingsField('Адрес API', base, 'wide'));

    const combo = modelCombo('model', provider.model, 'например, deepseek-chat',
      () => modelIdsFor(String(provider.name || '').trim()));
    row.appendChild(comboField('Модель', combo.wrap));

    const key = settingsControl('input', 'api_key', provider._key, '');
    key.type = 'password';
    key.autocomplete = 'new-password';
    key.placeholder = provider._clearKey ? 'ключ будет удалён' : 'Пусто — не менять';
    const keyLine = el('div', 'key-line');
    const apply = el('button', 'ghost', 'Применить');
    apply.type = 'button';
    apply.title = 'Определить сервис по ключу, заполнить адрес и список моделей';
    // The key is sent to the station only here (or on Enter in the field): the
    // answer lands under this very field instead of the shared status line.
    apply.addEventListener('click', () => applyProviderKey(provider, row, true));
    keyLine.append(key, apply);
    const keyField = el('div', 'field wide');
    keyField.appendChild(el('span', null, 'API-ключ'));
    keyField.appendChild(keyLine);
    keyField.appendChild(el('span', 'key-state'));
    keyField.appendChild(el('span', 'key-note small'));
    row.appendChild(keyField);

    const actions = el('div', 'row-actions');
    actions.appendChild(providerButton('Проверить', 'test'));
    actions.appendChild(providerButton('Забыть ключ', 'forget'));
    const remove = el('button', 'ghost danger-solid', 'Удалить');
    remove.type = 'button';
    remove.dataset.remove = 'provider';
    actions.appendChild(remove);
    row.appendChild(actions);

    list.appendChild(row);
    updateKeyBadge(row, provider);
  });
}

function renderWakeDictionary() {
  let datalist = document.getElementById('wake-dictionary-list');
  if (!datalist) {
    datalist = el('datalist');
    datalist.id = 'wake-dictionary-list';
    datalist.className = 'wake-dictionary';
    document.body.appendChild(datalist);
  }
  datalist.replaceChildren();
  for (const item of wakeDictionary) {
    const choice = el('option');
    choice.value = item.word;
    if (item.name && item.name !== item.word) choice.label = item.name;
    datalist.appendChild(choice);
  }
  const parts = [];
  if (wakeDictionary.length) {
    parts.push('выучено: ' + wakeDictionary.map((item) => item.name || item.word).join(', '));
  } else {
    parts.push('обученных слов не найдено');
  }
  if (wakeDictionaryNote) parts.push(wakeDictionaryNote);
  const known = new Set();
  for (const item of wakeDictionary) {
    known.add(String(item.word || '').toLowerCase());
    known.add(String(item.name || '').toLowerCase());
  }
  const unknown = [];
  for (const word of wordsState) {
    const value = String(word.word || '').trim();
    if (value && !known.has(value.toLowerCase())) unknown.push(value);
  }
  if (unknown.length) parts.push('не обучено, не сработает: ' + unknown.join(', '));
  document.getElementById('wake-dictionary-hint').textContent = parts.join(' · ');

  const train = document.getElementById('wake-train');
  const trainHint = document.getElementById('wake-train-hint');
  if (wakePage) {
    train.href = wakePage;
    train.hidden = false;
    trainHint.textContent = 'откроется в новой вкладке под этой же сессией';
  } else {
    train.hidden = true;
    trainHint.textContent = 'страница обучения выключена: ssh -L 8780:127.0.0.1:8780 root@192.168.1.210, затем http://127.0.0.1:8780/';
  }
}

async function loadWakeDictionary() {
  try {
    const payload = await getJSON(WAKE_DICTIONARY_URL);
    wakeDictionary = Array.isArray(payload.words) ? payload.words : [];
    wakeDictionaryNote = payload.error || '';
  } catch (error) {
    wakeDictionary = [];
    wakeDictionaryNote = error.message;
  }
  renderWakeDictionary();
}

function renderWakeWords(force) {
  const list = document.getElementById('wake-words-list');
  if (!force && list.contains(document.activeElement)) return;
  list.replaceChildren();
  if (!wordsState.length) {
    list.appendChild(el('p', 'muted small', 'Пока нет старт-слов. Добавьте первое слово.'));
    return;
  }
  wordsState.forEach((word, index) => {
    const row = el('div', 'settings-row form');
    row.dataset.kind = 'word';
    row.dataset.index = String(index);

    const wordInput = settingsControl('input', 'word', word.word, 'например, брифинг');
    wordInput.type = 'text';
    wordInput.autocomplete = 'off';
    // The list holds the words the daemon is actually trained on: a typed name
    // that is not in it can never fire, and the hint above says so.
    wordInput.setAttribute('list', 'wake-dictionary-list');
    row.appendChild(settingsField('Слово или фраза', wordInput));

    const action = settingsControl('select', 'action', word.action || 'listen');
    for (const name of settingsActions) {
      action.appendChild(option(name, ACTION_LABELS[name] || name));
    }
    action.value = word.action || 'listen';
    row.appendChild(settingsField('Действие', action));

    const agent = settingsControl('select', 'agent', word.agent);
    agent.className = 'word-agent-select';
    agent.appendChild(option('', 'без роли'));
    for (const item of agentsState) {
      if (item.name) agent.appendChild(option(item.name, item.name));
    }
    row.appendChild(settingsField('Роль', agent));

    const text = settingsControl('textarea', 'text', word.text, 'Фраза для роли или команды. Пусто — вопрос голосом');
    text.rows = 3;
    const textField = settingsField('Текст / фраза', text, 'wide');
    // An empty phrase is a mode of its own: the word only opens the microphone
    // and the role answers what the person says into it.
    textField.appendChild(el('span', 'muted small word-text-hint', 'Пусто — роль ответит на сказанное после слова. Дальше та же роль ведёт разговор до тишины или «хватит».'));
    row.appendChild(textField);

    const remove = el('button', 'ghost danger', 'Удалить слово');
    remove.type = 'button';
    remove.dataset.remove = 'word';
    const actions = el('div', 'row-actions');
    actions.appendChild(remove);
    row.appendChild(actions);
    list.appendChild(row);
    updateWordRow(row, word);
  });
}

function renderAgents(force) {
  const list = document.getElementById('agents-list');
  if (!force && list.contains(document.activeElement)) return;
  list.replaceChildren();
  if (!agentsState.length) {
    list.appendChild(el('p', 'muted small', 'Пока нет ролей. Добавьте первую роль.'));
    syncAgentSelects();
    return;
  }
  agentsState.forEach((agent, index) => {
    const row = el('div', 'settings-row form');
    row.dataset.kind = 'agent';
    row.dataset.index = String(index);
    row.dataset.syncedName = agent.name || '';

    const name = settingsControl('input', 'name', agent.name, 'буквы (рус. и лат.) в нижнем регистре, цифры, - и _');
    name.type = 'text';
    name.autocomplete = 'off';
    row.appendChild(settingsField('Имя роли', name));

    const provider = settingsControl('select', 'provider', agent.provider);
    provider.className = 'agent-provider';
    provider.appendChild(option('', 'по умолчанию'));
    for (const item of providersState) {
      if (item.name) provider.appendChild(option(item.name, item.name));
    }
    provider.value = agent.provider || '';
    row.appendChild(settingsField('Нейросеть', provider));

    const voice = settingsControl('select', 'voice', agent.voice);
    voice.appendChild(option('', 'голос Ассистента'));
    for (const voiceName of settingsVoices) voice.appendChild(option(voiceName, voiceName));
    voice.value = agent.voice || '';
    row.appendChild(settingsField('Голос', voice));

    // The list is the one of the chosen connection; the filter box on top of it
    // finds a model by a part of the name (for example «free»).
    const modelCombo1 = modelCombo('model', agent.model, 'модель по умолчанию', () => {
      const select = row.querySelector('[data-field="provider"]');
      return modelIdsFor(select ? select.value : agent.provider);
    });
    row.appendChild(comboField('Модель', modelCombo1.wrap));

    const prompt = settingsControl('textarea', 'system_prompt', agent.system_prompt);
    prompt.rows = 5;
    row.appendChild(settingsField('Системный промпт', prompt, 'wide'));

    const description = settingsControl('input', 'description', agent.description, 'чем занимается — это читают другие роли');
    description.type = 'text';
    description.maxLength = 300;
    description.autocomplete = 'off';
    row.appendChild(settingsField('Описание для других ролей', description, 'wide'));

    const peers = settingsControl('input', 'peers', '');
    peers.type = 'checkbox';
    peers.checked = Boolean(agent.peers);
    row.appendChild(settingsField('Может спрашивать другие роли и передавать им разговор', peers));

    const sysadmin = settingsControl('input', 'sysadmin', '');
    sysadmin.type = 'checkbox';
    sysadmin.checked = Boolean(agent.sysadmin);
    row.appendChild(settingsField('Сисадмин (доступ к хостам, память; опасное — через подтверждение)', sysadmin));

    const remove = el('button', 'ghost danger', 'Удалить роль');
    remove.type = 'button';
    remove.dataset.remove = 'agent';
    const actions = el('div', 'row-actions');
    actions.appendChild(remove);
    row.appendChild(actions);
    list.appendChild(row);
    updateAgentRow(row, agent);
  });
  syncAgentSelects();
  syncProviderSelects();
}

// updateAgentRow shows which fields a linked role actually uses. A role with a
// provider inherits its model, address and format from that provider, so the
// free-text model is disabled instead of silently ignored.
function updateAgentRow(row, agent) {
  const model = row.querySelector('[data-field="model"]');
  if (!model) return;
  // A role's own model overrides the one of the connection (resolveLLM), so the
  // field stays editable; empty means "take it from the connection".
  const linked = Boolean(agent.provider);
  model.disabled = false;
  model.placeholder = linked ? 'из подключения (или выберите)' : 'модель по умолчанию';
}

// syncProviderSelects keeps the role list in step with the provider list: a role
// may name a provider that was just renamed or deleted, and the write path would
// reject such a role.
function syncProviderSelects() {
  const list = document.getElementById('agents-list');
  const names = providersState.map((provider) => provider.name).filter(Boolean);
  for (const select of list.querySelectorAll('.agent-provider')) {
    const row = select.closest('[data-kind="agent"]');
    const index = Number(row.dataset.index);
    const agent = agentsState[index];
    if (!agent) continue;
    if (agent.provider && !names.includes(agent.provider)) {
      agent.provider = '';
      updateAgentRow(row, agent);
    }
    select.replaceChildren(option('', 'по умолчанию'));
    for (const name of names) select.appendChild(option(name, name));
    select.value = agent.provider || '';
  }
}

function updateWordRow(row, word) {
  const listening = word.action === 'listen';
  const asking = word.action === 'ask';
  const agent = row.querySelector('[data-field="agent"]');
  const text = row.querySelector('[data-field="text"]');
  if (agent) {
    agent.value = word.agent || '';
    agent.disabled = !asking;
  }
  if (text) {
    text.value = word.text || '';
    const field = text.closest('.field');
    field.hidden = listening;
    const hint = field.querySelector('.word-text-hint');
    if (hint) hint.hidden = !asking;
  }
}

function syncAgentSelects() {
  const list = document.getElementById('wake-words-list');
  for (const select of list.querySelectorAll('.word-agent-select')) {
    const index = Number(select.closest('[data-kind="word"]').dataset.index);
    const word = wordsState[index];
    if (!word) continue;
    const current = word.agent || '';
    const names = agentsState.map((agent) => agent.name).filter(Boolean);
    if (current && !names.includes(current)) word.agent = '';
    select.replaceChildren(option('', 'без роли'));
    for (const name of names) select.appendChild(option(name, name));
    select.value = word.agent || '';
  }
}

// --- Маршрутизация ---------------------------------------------------------
//
// The routing section is one JSON object: rules in priority order plus the
// classifier settings. The page keeps a working copy in routingState and sends
// it back whole, exactly like the provider and role lists, so what the operator
// sees is what the bridge compiles.

const ROUTE_LABELS = { llm: 'нейросеть', yandex: 'Алиса', home: 'умный дом' };

const SOURCE_LABELS = {
  rule: 'правило',
  classifier: 'классификатор',
  default: 'если правило не сработало',
  classifier_error: 'классификатор не ответил',
  playback: 'управление воспроизведением',
  dialogue: 'продолжение диалога',
  dialogue_retry: 'просьба о нейросети',
  dialogue_reset: 'сброс диалога',
  dialogue_agent: 'роль в диалоге',
  pinned: 'закреплённая роль'
};

function routeLabel(route) {
  const text = String(route || '');
  if (text.indexOf('agent:') === 0) return 'роль ' + (text.slice('agent:'.length) || '?');
  return ROUTE_LABELS[text] || text || '—';
}

// routeValues builds the list from the role state, because a route may name a
// role the operator just added. The current value is kept even if its role is
// gone: silently rewriting a rule would change the policy on the next save.
function routeValues(current, withClassify) {
  const values = ['llm', 'yandex', 'home'];
  if (withClassify) values.push('classify');
  for (const agent of agentsState) {
    if (agent && agent.name) values.push('agent:' + agent.name);
  }
  if (current && !values.includes(current)) values.push(current);
  return values;
}

function normalizeRouting(routing) {
  const source = routing && typeof routing === 'object' ? routing : {};
  const classifier = source.classifier && typeof source.classifier === 'object' ? source.classifier : {};
  const refusal = source.refusal && typeof source.refusal === 'object' ? source.refusal : {};
  return {
    rules: Array.isArray(source.rules) ? source.rules.map(normalizeRoutingRule) : [],
    default_route: String(source.default_route || 'classify'),
    classifier: {
      timeout_ms: Number(classifier.timeout_ms) || 8000,
      on_error: String(classifier.on_error || 'yandex'),
      prompt: typeof classifier.prompt === 'string' ? classifier.prompt : ''
    },
    // The prefix and phrase lists are not on the form: the checkbox turns the
    // matcher on, and the lists travel with the state so they survive a save.
    refusal: {
      enabled: Boolean(refusal.enabled),
      prefixes: Array.isArray(refusal.prefixes) ? refusal.prefixes.slice() : [],
      contains_any: Array.isArray(refusal.contains_any) ? refusal.contains_any.slice() : []
    }
  };
}

// Built-in rules used to carry English names; older configs still do.
const legacyRuleNames = {
  bridge_updates: 'обновление моста',
  speaker_playback: 'управление воспроизведением',
  music_from_site: 'музыка с сайта',
  assistant_readonly: 'история и страницы',
  home_devices: 'устройства дома',
  complex_queries: 'сложные задачи',
  yandex_services: 'сервисы Яндекса',
  questions_to_llm: 'вопросы нейросети',
};

function normalizeRoutingRule(rule) {
  const match = rule && rule.match && typeof rule.match === 'object' ? rule.match : {};
  const list = (value) => (Array.isArray(value) ? value.slice() : []);
  return {
    name: legacyRuleNames[String((rule && rule.name) || '')] || String((rule && rule.name) || ''),
    route: String((rule && rule.route) || 'llm'),
    disabled: Boolean(rule && rule.disabled),
    match: {
      contains_any: list(match.contains_any),
      contains_all_groups: Array.isArray(match.contains_all_groups)
        ? match.contains_all_groups.map((group) => list(group))
        : [],
      contains_none: list(match.contains_none),
      starts_with_any: list(match.starts_with_any),
      regex: typeof match.regex === 'string' ? match.regex : ''
    }
  };
}

function termsToText(terms) {
  return Array.isArray(terms) ? terms.join(', ') : '';
}

// textToTerms drops empty pieces: a trailing comma would otherwise become an
// empty term, which the bridge rejects as "contains_any has an empty term".
function textToTerms(text) {
  return String(text || '').split(',').map((part) => part.trim()).filter((part) => part.length > 0);
}

// groupsToText keeps the two levels readable: groups are separated by «;»,
// alternatives inside one group by «,».
function groupsToText(groups) {
  if (!Array.isArray(groups)) return '';
  return groups
    .map((group) => (Array.isArray(group) ? group.join(', ') : ''))
    .filter((piece) => piece.length > 0)
    .join('; ');
}

function textToGroups(text) {
  return String(text || '').split(';').map(textToTerms).filter((group) => group.length > 0);
}

// selectValue shows a value that the fixed option list does not know (an agent
// route, a role that was deleted elsewhere). Blanking the select would change
// the policy the next time «Сохранить» is pressed.
function selectValue(select, value, label) {
  const text = String(value || '');
  if (!text) {
    select.value = '';
    return;
  }
  if (!Array.from(select.options).some((item) => item.value === text)) {
    select.appendChild(option(text, label || text));
  }
  select.value = text;
}

function ruleControl(tag, field, value, placeholder) {
  const control = el(tag);
  control.dataset.ruleField = field;
  control.value = value == null ? '' : String(value);
  if (placeholder) control.placeholder = placeholder;
  return control;
}

function ruleButton(label, attribute, value, extraClass) {
  const button = el('button', 'ghost' + (extraClass ? ' ' + extraClass : ''), label);
  button.type = 'button';
  button.dataset[attribute] = value;
  return button;
}

function routingRuleRow(rule, index) {
  const row = el('div', 'settings-row form routing-rule');
  row.dataset.kind = 'rule';
  row.dataset.index = String(index);
  row.classList.toggle('is-off', rule.disabled);

  const name = ruleControl('input', 'name', rule.name, 'например, вопросы нейросети');
  name.type = 'text';
  name.autocomplete = 'off';
  row.appendChild(settingsField('Имя правила', name));

  const route = ruleControl('select', 'route', rule.route);
  for (const value of routeValues(rule.route, false)) route.appendChild(option(value, routeLabel(value)));
  route.value = rule.route;
  row.appendChild(settingsField('Кому отвечать', route));

  const toggleRow = el('label', 'check');
  const toggle = el('input');
  toggle.type = 'checkbox';
  toggle.dataset.ruleField = 'disabled';
  toggle.checked = !rule.disabled;
  toggleRow.appendChild(toggle);
  toggleRow.appendChild(document.createTextNode(' включено'));
  row.appendChild(toggleRow);

  const actions = el('div', 'row-actions');
  actions.appendChild(ruleButton('↑', 'ruleMove', 'up'));
  actions.appendChild(ruleButton('↓', 'ruleMove', 'down'));
  actions.appendChild(ruleButton('Удалить', 'ruleRemove', 'rule', 'danger'));
  row.appendChild(actions);

  // The conditions stay folded: a rule is usually read by its name, and the
  // five match fields are verbose enough to bury the priority order.
  const details = el('details', 'routing-match');
  details.appendChild(el('summary', null, 'Условия'));
  const grid = el('div', 'form');
  const condition = (label, tag, field, value, placeholder, extra) => {
    const control = ruleControl(tag, field, value, placeholder);
    if (tag === 'textarea') control.rows = 2;
    control.spellcheck = false;
    grid.appendChild(settingsField(label, control, extra));
  };
  condition('Содержит любые из', 'input', 'contains_any', termsToText(rule.match.contains_any), 'через запятую: погода, таймер');
  condition('Содержит все группы', 'input', 'contains_all_groups', groupsToText(rule.match.contains_all_groups), 'группы через «;», варианты через запятую: включи, поставь; свет, лампу');
  condition('Не содержит', 'input', 'contains_none', termsToText(rule.match.contains_none), 'через запятую: песню, музыку');
  condition('Начинается с', 'input', 'starts_with_any', termsToText(rule.match.starts_with_any), 'через запятую: спроси, объясни');
  condition('Регулярное выражение', 'textarea', 'regex', rule.match.regex, '^объясни\\s+.+');
  details.appendChild(grid);
  details.appendChild(el('p', 'muted small', 'Достаточно одного положительного условия: «содержит», «все группы», «начинается с» или выражение. Пустые условия не считаются.'));
  row.appendChild(details);
  return row;
}

function renderRoutingRules() {
  const list = document.getElementById('routing-rules');
  if (!routingState) return;
  list.replaceChildren();
  if (!routingState.rules.length) {
    list.appendChild(el('p', 'muted small', 'Правил нет: фразу решает классификатор или значение «если правило не сработало».'));
    return;
  }
  routingState.rules.forEach((rule, index) => list.appendChild(routingRuleRow(rule, index)));
}

function renderRoutingPresets() {
  const select = document.getElementById('routing-preset');
  select.replaceChildren();
  if (!routingPresets.length) {
    select.appendChild(option('', 'нет готовых режимов'));
    return;
  }
  for (const preset of routingPresets) select.appendChild(option(preset.id, preset.label));
}

function renderRouting(force) {
  if (!routingState) return;
  const section = document.getElementById('section-routing');
  if (!force && section.contains(document.activeElement)) return;
  selectValue(document.getElementById('routing-default'), routingState.default_route, routeLabel(routingState.default_route));
  document.getElementById('routing-timeout').value = String(routingState.classifier.timeout_ms);
  selectValue(document.getElementById('routing-on-error'), routingState.classifier.on_error, routeLabel(routingState.classifier.on_error));
  document.getElementById('routing-refusal').checked = Boolean(routingState.refusal.enabled);
  document.getElementById('routing-prompt').value = routingState.classifier.prompt;
  renderRoutingPresets();
  renderRoutingRules();
}

// editRoutingRule writes one field of one rule. It never re-renders the list:
// re-rendering while typing would replace the control the caret is in.
function editRoutingRule(event) {
  const control = event.target;
  const field = control.dataset.ruleField;
  if (!field) return;
  const row = control.closest('[data-kind="rule"]');
  if (!row || !routingState) return;
  const rule = routingState.rules[Number(row.dataset.index)];
  if (!rule) return;
  const value = control.type === 'checkbox' ? control.checked : control.value;
  if (field === 'name') rule.name = value;
  else if (field === 'route') rule.route = value;
  else if (field === 'disabled') {
    rule.disabled = !value;
    row.classList.toggle('is-off', rule.disabled);
  } else if (field === 'contains_any') rule.match.contains_any = textToTerms(value);
  else if (field === 'contains_all_groups') rule.match.contains_all_groups = textToGroups(value);
  else if (field === 'contains_none') rule.match.contains_none = textToTerms(value);
  else if (field === 'starts_with_any') rule.match.starts_with_any = textToTerms(value);
  else if (field === 'regex') rule.match.regex = value;
}

function routingRuleAction(event) {
  if (!routingState) return;
  const move = event.target.closest('[data-rule-move]');
  if (move) {
    const row = move.closest('[data-kind="rule"]');
    if (!row) return;
    const index = Number(row.dataset.index);
    const target = index + (move.dataset.ruleMove === 'up' ? -1 : 1);
    if (target < 0 || target >= routingState.rules.length) return;
    const rules = routingState.rules;
    const held = rules[index];
    rules[index] = rules[target];
    rules[target] = held;
    renderRoutingRules();
    return;
  }
  const remove = event.target.closest('[data-rule-remove]');
  if (remove) {
    const row = remove.closest('[data-kind="rule"]');
    if (!row) return;
    routingState.rules.splice(Number(row.dataset.index), 1);
    renderRoutingRules();
  }
}

function applyRoutingPreset() {
  const select = document.getElementById('routing-preset');
  const preset = routingPresets.find((item) => item.id === select.value);
  const note = document.getElementById('routing-preset-note');
  if (!preset) {
    note.textContent = '';
    return;
  }
  routingState = normalizeRouting(preset.config);
  renderRouting(true);
  note.textContent = preset.note + ' Проверьте форму и нажмите «Сохранить».';
}

function presetNote() {
  const select = document.getElementById('routing-preset');
  const preset = routingPresets.find((item) => item.id === select.value);
  document.getElementById('routing-preset-note').textContent = preset ? preset.note : '';
}

function addRoutingRule() {
  if (!routingState) return;
  routingState.rules.push({ name: '', route: 'llm', disabled: false, match: { contains_any: [], contains_all_groups: [], contains_none: [], starts_with_any: [], regex: '' } });
  renderRoutingRules();
}

function resetRouting() {
  const preset = routingPresets.find((item) => item.id === 'smart') || routingPresets[0];
  if (!preset) return;
  routingState = normalizeRouting(preset.config);
  renderRouting(true);
  document.getElementById('routing-preset-note').textContent = 'Вернули стандартный набор правил. Нажмите «Сохранить», чтобы применить.';
}

function routingPayload() {
  if (!routingState) return null;
  return {
    rules: routingState.rules.map((rule) => ({
      name: String(rule.name || '').trim(),
      route: rule.route,
      disabled: Boolean(rule.disabled),
      match: {
        contains_any: rule.match.contains_any,
        contains_all_groups: rule.match.contains_all_groups,
        contains_none: rule.match.contains_none,
        starts_with_any: rule.match.starts_with_any,
        regex: rule.match.regex
      }
    })),
    default_route: routingState.default_route,
    classifier: {
      timeout_ms: Number(routingState.classifier.timeout_ms) || 0,
      on_error: routingState.classifier.on_error,
      prompt: routingState.classifier.prompt
    },
    refusal: {
      enabled: Boolean(routingState.refusal.enabled),
      prefixes: routingState.refusal.prefixes,
      contains_any: routingState.refusal.contains_any
    }
  };
}

function readRoutingForm() {
  if (!routingState) return;
  routingState.default_route = document.getElementById('routing-default').value;
  routingState.classifier.timeout_ms = Number(document.getElementById('routing-timeout').value) || 0;
  routingState.classifier.on_error = document.getElementById('routing-on-error').value;
  routingState.classifier.prompt = document.getElementById('routing-prompt').value;
  routingState.refusal.enabled = document.getElementById('routing-refusal').checked;
}

// testRouting asks the live policy about one phrase. The answer comes from the
// saved policy, not from unsaved edits, and the hint says so.
async function testRouting() {
  const input = document.getElementById('routing-test-text');
  const result = document.getElementById('routing-test-result');
  const text = input.value.trim();
  result.classList.remove('error');
  if (!text) {
    result.textContent = 'введите фразу';
    result.classList.add('error');
    return;
  }
  result.textContent = 'спрашиваю…';
  try {
    const payload = await postJSON(ROUTING_TEST_URL, { text: text });
    const parts = ['маршрут: ' + routeLabel(payload && payload.route)];
    if (payload && payload.rule) parts.push('правило: ' + payload.rule);
    if (payload && payload.source) parts.push('источник: ' + (SOURCE_LABELS[payload.source] || payload.source));
    result.textContent = parts.join(' · ') + ' (по сохранённым правилам)';
  } catch (error) {
    result.textContent = error.message;
    result.classList.add('error');
  }
}

// --- Обновление ------------------------------------------------------------

function shortDate(iso) {
  if (!iso) return '';
  const date = new Date(iso);
  if (isNaN(date.getTime())) return '';
  return date.toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit', year: 'numeric' });
}

async function refreshUpdate() {
  const status = document.getElementById('update-status');
  const release = document.getElementById('update-release');
  const button = document.getElementById('update-check');
  button.disabled = true;
  status.classList.remove('error');
  status.textContent = 'проверяю…';
  try {
    const payload = await getJSON(UPDATE_URL);
    document.getElementById('update-build').textContent =
      (payload.current_version || 'dev') + (payload.current_tag ? ' (' + payload.current_tag + ')' : '');
    document.getElementById('update-upstream').textContent = payload.upstream || '—';
    document.getElementById('update-command').textContent = payload.command || '—';
    if (payload.latest_tag) {
      const published = shortDate(payload.latest_published);
      document.getElementById('update-latest').textContent = payload.latest_tag + (published ? ' · ' + published : '');
      release.hidden = !payload.latest_url;
      if (payload.latest_url) release.href = payload.latest_url;
    } else {
      document.getElementById('update-latest').textContent = '—';
      release.hidden = true;
    }
    if (payload.error) {
      status.textContent = 'нет ответа от GitHub: ' + payload.error;
      status.classList.add('error');
    } else if (payload.update_available) {
      status.textContent = 'есть новый релиз ' + payload.latest_tag + ' — запустите команду ниже';
    } else if (payload.current_tag && payload.latest_tag) {
      status.textContent = 'обновлений нет';
    } else {
      // Without a stamped tag the build cannot be compared to a release: say so
      // instead of claiming "нет обновлений" on a build the panel cannot place.
      status.textContent = 'сборка без тега: сравнивать не с чем';
    }
  } catch (error) {
    status.textContent = error.message;
    status.classList.add('error');
  } finally {
    button.disabled = false;
  }
}

// refreshStationVolume reads the level the speaker is at right now, so the
// slider opens where the room actually is instead of where the file says.
async function refreshStationVolume() {
  const range = document.getElementById('station-volume-range');
  const value = document.getElementById('station-volume-value');
  const status = document.getElementById('station-volume-status');
  if (!range) return;
  try {
    const payload = await getJSON(STATION_VOLUME_URL);
    if (!payload.available) {
      range.disabled = true;
      value.textContent = '—';
      status.textContent = 'недоступно: ' + (payload.error || 'нет микшера');
      status.classList.add('error');
      return;
    }
    range.disabled = false;
    range.value = String(payload.percent);
    value.textContent = payload.percent + '%';
    paintSlider(range);
    status.classList.remove('error');
    status.textContent = payload.has_saved ? 'запоминается на станции' : '';
  } catch (error) {
    value.textContent = '—';
    status.textContent = error.message;
    status.classList.add('error');
  }
}

// applyStationVolume moves the mixer. The number the bridge reads back is shown,
// not the one that was sent: amixer may round to its own scale.
async function applyStationVolume(percent) {
  const range = document.getElementById('station-volume-range');
  const value = document.getElementById('station-volume-value');
  const status = document.getElementById('station-volume-status');
  if (!range || range.disabled) return;
  range.disabled = true;
  status.classList.remove('error');
  status.textContent = 'применяю…';
  try {
    const payload = await postJSON(STATION_VOLUME_URL, { percent });
    range.value = String(payload.percent);
    value.textContent = payload.percent + '%';
    paintSlider(range);
    status.textContent = 'уровень ' + payload.percent + '%';
  } catch (error) {
    status.textContent = error.message;
    status.classList.add('error');
    // The mixer may have moved or not: show what it really holds.
    await refreshStationVolume();
  } finally {
    range.disabled = false;
  }
}

function initStationVolume() {
  const range = document.getElementById('station-volume-range');
  if (!range) return;
  paintSlider(range);
  range.addEventListener('input', () => {
    document.getElementById('station-volume-value').textContent = range.value + '%';
    paintSlider(range);
  });
  // `change` fires when the thumb is released or a key is pressed, so dragging
  // does not send a request per pixel.
  range.addEventListener('change', () => { applyStationVolume(Number(range.value)); });
}

// renderSettings refreshes the page from a settings snapshot. A scope limits the
// refresh to one section: the response always carries the whole file, so a save
// of one section must not replace the state of another one and throw away what
// is typed there. The row lists render from their state arrays, which hold the
// edits, so a forced redraw after a save keeps them.
function renderSettings(payload, force, scope) {
  const all = !scope;
  const touchingProviders = all || scope === 'providers';
  const touchingWords = all || scope === 'words';
  const touchingRoles = all || scope === 'roles';
  const touchingRouting = all || scope === 'routing';
  if (touchingRoles) {
    agentsState = Array.isArray(payload.agents) ? payload.agents : [];
    savedAgentNames = agentsState.map((agent) => String((agent && agent.name) || ''));
  }
  if (touchingProviders) {
    providersState = Array.isArray(payload.llm_providers) ? payload.llm_providers : [];
    savedProviders = new Map(providersState.map((provider) => [
      String(provider.name || ''), JSON.stringify(providerPayload(provider))
    ]));
    llmPresetsState = Array.isArray(payload.llm_presets) ? payload.llm_presets : llmPresetsState;
    if (payload.provider_models && typeof payload.provider_models === 'object') providerModels = payload.provider_models;
    providerKeysState = payload.provider_keys && typeof payload.provider_keys === 'object'
      ? payload.provider_keys
      : {};
  }
  const wakeWords = touchingWords ? (payload.wake_words || {}) : {};
  if (touchingWords) {
    wordsState = Array.isArray(wakeWords.words) ? wakeWords.words : [];
  }
  settingsActions = Array.isArray(payload.actions) ? payload.actions : ['listen', 'ask', 'command'];
  settingsVoices = Array.isArray(payload.voices) ? payload.voices : [];
  wakePage = typeof payload.wake_page === 'string' ? payload.wake_page : '';
  routingPresets = Array.isArray(payload.routing_presets) ? payload.routing_presets : routingPresets;
  // The section follows the file; a first load after an upgrade gets the policy
  // the bridge actually runs, which the server fills in when the file is old.
  if (touchingRouting) routingState = normalizeRouting(payload.routing);

  const form = document.getElementById('settings-form');
  if (touchingWords && (force || !form.contains(document.activeElement))) {
    document.getElementById('settings-dry-run').checked = Boolean(wakeWords.dry_run);
  }
  // The role rows build their provider options from providersState, so a
  // provider save redraws both lists.
  if (touchingProviders) renderProviders(force);
  if (touchingWords) renderWakeWords(force);
  if (touchingRoles || touchingProviders) renderAgents(force);
  if (touchingRouting) renderRouting(force);
  renderWakeDictionary();
}

async function loadSettings() {
  renderSettings(await getJSON(SETTINGS_URL), false);
}

async function refreshSettingsSafe() {
  try {
    await loadSettings();
  } catch (error) {
    const status = document.getElementById('settings-status');
    status.textContent = error.message;
    status.classList.add('error');
  }
}

function editSettingsField(event) {
  const control = event.target;
  if (!control.dataset.field) return;
  const row = control.closest('[data-kind]');
  if (!row) return;
  const index = Number(row.dataset.index);
  if (row.dataset.kind === 'word') {
    const word = wordsState[index];
    if (!word) return;
    word[control.dataset.field] = control.value;
    if (control.dataset.field === 'action') {
      if (word.action === 'listen') {
        word.text = '';
        word.agent = '';
      } else if (word.action === 'command') {
        word.agent = '';
      }
      updateWordRow(row, word);
    }
  } else if (row.dataset.kind === 'provider') {
    const provider = providersState[index];
    if (!provider) return;
    if (control.dataset.field === 'api_key') {
      provider._key = control.value;
      if (control.value) provider._clearKey = false;
      updateKeyBadge(row, provider);
      return;
    }
    provider[control.dataset.field] = control.value;
    if (control.dataset.field === 'name' && event.type === 'change') {
      // A rename is a new provider name everywhere: the roles linked to it and
      // the stored key both follow, otherwise the old name would be rejected as
      // an unknown provider on save.
      const previousName = row.dataset.syncedName || '';
      if (previousName && previousName !== provider.name) {
        for (const agent of agentsState) {
          if (agent.provider === previousName) agent.provider = provider.name;
        }
        if (previousName in providerKeysState) {
          // A stored key belongs to the old name and cannot travel with it: the
          // server drops it when that provider disappears from the list. Say so
          // now instead of showing "ключ сохранён" until the next save.
          delete providerKeysState[previousName];
          document.getElementById('settings-status').textContent =
            'имя изменено: ключ нужно ввести заново';
        }
        if (providerModels[previousName]) {
          providerModels[provider.name] = providerModels[previousName];
          delete providerModels[previousName];
        }
      }
      row.dataset.syncedName = provider.name || '';
      syncProviderSelects();
      updateKeyBadge(row, provider);
    }
  } else if (row.dataset.kind === 'agent') {
    const agent = agentsState[index];
    if (!agent) return;
    agent[control.dataset.field] = control.type === 'checkbox' ? control.checked : control.value;
    if (control.dataset.field === 'provider') {
      updateAgentRow(row, agent);
    }
    if (control.dataset.field === 'name' && event.type === 'change') {
      const previousName = row.dataset.syncedName || '';
      if (previousName && previousName !== agent.name) {
        for (const word of wordsState) {
          if (word.agent === previousName) word.agent = agent.name;
        }
      }
      row.dataset.syncedName = agent.name || '';
      syncAgentSelects();
    }
  }
}

function removeSettingsItem(event) {
  const button = event.target.closest('[data-remove]');
  if (!button) return;
  const row = button.closest('[data-kind]');
  if (!row) return;
  const index = Number(row.dataset.index);
  if (row.dataset.kind === 'word') {
    wordsState.splice(index, 1);
    renderWakeWords(true);
  } else if (row.dataset.kind === 'provider') {
    const removed = providersState[index];
    providersState.splice(index, 1);
    if (removed) {
      delete providerKeysState[removed.name];
      delete providerModels[removed.name];
    }
    syncProviderSelects();
    renderProviders(true);
  } else if (row.dataset.kind === 'agent') {
    agentsState.splice(index, 1);
    syncAgentSelects();
    renderAgents(true);
  }
}

// providerAction runs the two live checks against a saved provider. Both endpoints
// resolve the provider from the file, so a row that was never saved is reported
// as such instead of pretending to test it.
async function providerAction(event) {
  const button = event.target.closest('[data-provider-action]');
  if (!button) return;
  const row = button.closest('[data-kind="provider"]');
  if (!row) return;
  const provider = providersState[Number(row.dataset.index)];
  if (!provider) return;
  const status = document.getElementById('settings-status');
  status.classList.remove('error');
  const action = button.dataset.providerAction;
  if (action === 'forget') {
    provider._clearKey = true;
    provider._key = '';
    updateKeyBadge(row, provider);
    status.textContent = 'ключ будет удалён при сохранении';
    return;
  }
  // «Проверить» works on the row as it stands, saved or not: it proves the key
  // and the address, refreshes the stored model list and asks the chosen model
  // one tiny question. The answer goes under the key field.
  setKeyNote(row, 'Проверяю подключение…', '');
  try {
    const payload = await postJSON(LLM_CHECK_URL, {
      provider: String(provider.name || '').trim().toLowerCase(),
      base_url: String(provider.base_url || '').trim(),
      api: provider.api || 'chat_completions',
      model: String(provider.model || '').trim(),
      api_key: String(provider._key || '').trim()
    });
    const name = String(provider.name || '').trim().toLowerCase();
    if (Array.isArray(payload.models) && name) providerModels[name] = payload.models;
    const parts = [];
    if (Array.isArray(payload.models)) parts.push('доступ есть, моделей: ' + payload.models.length);
    else if (payload.models_error) parts.push('список моделей: ' + payload.models_error);
    if (payload.chat_ok) parts.push('ответ за ' + (payload.latency_ms || 0) + ' мс: «' + (payload.sample || 'пусто') + '»');
    else if (payload.chat_error) parts.push('модель не ответила: ' + payload.chat_error);
    setKeyNote(row, parts.join('; ') || 'нет данных', payload.ok ? 'ok' : 'error');
  } catch (error) {
    setKeyNote(row, error.message, 'error');
  }
}

// settingsSaving keeps a double click on «Сохранить» from sending the same
// payload twice; the wrapper below turns the flag off on every exit path.
let settingsSaving = false;

// Each «Сохранить» saves its own section. The write path is a partial patch
// (settingsPatch keeps pointers, an absent section is left alone), so saving the
// roles cannot overwrite an unsaved provider row, and a broken row in one
// section no longer blocks the others. The section title goes into the shared
// status line in the header, because all four buttons write there.
const settingsScopeLabels = {
  providers: 'Нейросети',
  words: 'Старт-слова',
  roles: 'Роли',
  routing: 'Маршрутизация'
};

async function submitSettings(event) {
  event.preventDefault();
  if (settingsSaving) return;
  // event.submitter is the button that started the submit: the four «Сохранить»
  // write different sections. It is empty only when the form was submitted from
  // code, and then the whole configuration is written, as before.
  const scope = event.submitter && event.submitter.dataset
    ? String(event.submitter.dataset.scope || '')
    : '';
  settingsSaving = true;
  try {
    await applySettings(scope);
  } finally {
    settingsSaving = false;
  }
}

async function applySettings(scope) {
  const status = document.getElementById('settings-status');
  const label = settingsScopeLabels[scope] || '';
  const withPrefix = (text) => (label ? label + ': ' + text : text);
  const all = !scope;
  status.classList.remove('error');
  const body = {};
  let renamed = false;
  if (all || scope === 'providers') {
    // A key pasted into a fresh row is enough to save: fill the row from the key
    // first, otherwise the checks below would refuse an empty address or model.
    for (const provider of providersState) {
      if (!String(provider._key || '').trim()) continue;
      // A missing model is fine: the guess only fills the list to pick from.
      const needsGuess = !String(provider.name || '').trim() ||
        !String(provider.base_url || '').trim() || !String(provider.model || '').trim();
      if (!needsGuess) continue;
      const row = document.querySelector('[data-kind="provider"][data-index="' +
        String(providersState.indexOf(provider)) + '"]');
      status.textContent = withPrefix('определяю провайдера по ключу…');
      try {
        await applyProviderKey(provider, row, false);
      } catch (error) {
        // Not fatal: the row may already be complete by hand, and the checks below
        // report anything that is still missing.
      }
    }
    // Reported here, in the operator's words, instead of the strict decoder's
    // "llm_providers[].name must be 1..32 characters…" from the server.
    for (const provider of providersState) {
      const name = String(provider.name || '').trim().toLowerCase();
      if (!name || !String(provider.base_url || '').trim() || !String(provider.model || '').trim()) {
        status.textContent = withPrefix('у подключения нужны название, адрес API и модель');
        status.classList.add('error');
        return;
      }
      if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(name)) {
        status.textContent = withPrefix('название подключения: латиница, цифры, «-», «_», до 32 символов — например deepseek');
        status.classList.add('error');
        return;
      }
      // One canonical spelling: the name is the credential key in secrets.json and
      // the value a role points at, so fold it here and carry the roles along.
      // A rename is also written with the roles: a role pointing at the old name
      // would be rejected as an unknown provider by the server.
      if (name !== provider.name) {
        for (const agent of agentsState) {
          if (agent.provider === provider.name) agent.provider = name;
        }
        provider.name = name;
        renamed = true;
      }
    }
    body.llm_providers = providersState.map(providerPayload);
  }
  // One spelling for a role name: trimmed and lower-case, Cyrillic included. The
  // start words that point at the old spelling follow it.
  for (const agent of agentsState) {
    const before = String(agent.name || '');
    const folded = before.trim().toLowerCase();
    if (folded === before) continue;
    for (const word of wordsState) {
      if (word.agent === before) word.agent = folded;
    }
    agent.name = folded;
  }
  // A start word may point at a role that was just added and not saved yet: the
  // server checks the words against the roles of the same request, so the roles
  // go along, otherwise the word would be refused as pointing at an unknown role.
  const savedRoles = new Set(savedAgentNames);
  const wordsNeedRoles = scope === 'words' && wordsState.some((word) => {
    const name = String((word && word.agent) || '').trim();
    return name !== '' && !savedRoles.has(name);
  });
  if (all || scope === 'roles' || renamed || wordsNeedRoles) body.agents = agentsState;
  if (all || scope === 'words') {
    body.wake_words = {
      dry_run: document.getElementById('settings-dry-run').checked,
      words: wordsState
    };
  }
  if (all || scope === 'routing') {
    // The routing form is read here, not on every keystroke: the section has
    // controls outside the rule rows (prompt, timeout, refusal) and one read keeps
    // the payload and what the operator sees in step.
    readRoutingForm();
    body.routing = routingPayload();
  }
  status.textContent = withPrefix('сохраняю…');
  try {
    const payload = await postJSON(SETTINGS_URL, body);
    status.textContent = withPrefix(payload && payload.unchanged ? 'без изменений' : 'применено');
    renderSettings(payload || {}, true, scope);
    if (all || scope === 'words') loadWakeDictionary();
  } catch (error) {
    status.textContent = withPrefix(error.message);
    status.classList.add('error');
    // The status line sits in the page header; the button that failed may be far below.
    status.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }
}

async function submitVoice(event) {
  event.preventDefault();
  const status = document.getElementById('voice-status');
  status.textContent = 'применяю…';
  const patch = {
    voice: document.getElementById('voice-name').value,
    candidates: parseCandidates(document.getElementById('voice-candidates').value)
  };
  const speed = document.getElementById('voice-speed').value.trim();
  if (speed !== '') patch.speed = Number(speed);
  const volume = document.getElementById('voice-volume').value.trim();
  if (volume !== '') patch.volume = Number(volume);
  try {
    const payload = await postJSON(VOICE_URL, patch);
    voiceFormDirty = false;
    status.textContent = payload && payload.unchanged ? 'без изменений' : 'применено';
    await refreshState();
  } catch (error) {
    status.textContent = error.message;
  }
}

async function testVoice() {
  const status = document.getElementById('voice-status');
  const play = document.getElementById('voice-play').checked;
  status.textContent = play ? 'готовлю фразу…' : 'синтез…';
  try {
    const payload = await postJSON(VOICE_TEST_URL, {
      voice: document.getElementById('voice-name').value,
      text: document.getElementById('voice-text').value.trim(),
      speed: parseFloat(document.getElementById('voice-speed').value) || 0,
      volume: parseFloat(document.getElementById('voice-volume').value) || 0,
      emotion: document.getElementById('voice-emotion').value,
      effect: document.getElementById('voice-effect').value,
      whisper: document.getElementById('voice-whisper').checked,
      play: play,
      per_case_ms: 5000
    });
    const result = payload && payload.results && payload.results[0];
    if (result && result.ok) {
      status.textContent = play
        ? 'фраза готова на 60 с: коснитесь станции или скажите «Алиса»/старт-слово — она прозвучит вместо ответа'
        : 'голос принят, ' + result.elapsed_ms + ' мс';
    } else {
      status.textContent = 'ошибка: ' + ((result && result.error) || 'нет ответа');
    }
  } catch (error) {
    status.textContent = error.message;
  }
}

async function submitPassword(event) {
  event.preventDefault();
  const status = document.getElementById('pw-status');
  const current = document.getElementById('pw-current').value;
  const next = document.getElementById('pw-next').value;
  const confirm = document.getElementById('pw-confirm').value;
  if (next !== confirm) {
    status.textContent = 'новые пароли не совпадают';
    return;
  }
  if (next.length < 8) {
    status.textContent = 'пароль короче 8 символов';
    return;
  }
  status.textContent = 'меняю…';
  try {
    await postJSON(PASSWORD_URL, { current: current, next: next });
    status.textContent = 'пароль изменён, остальные сессии закрыты';
    document.getElementById('password-form').reset();
  } catch (error) {
    status.textContent = error.message;
  }
}

// loadSession fetches the CSRF token bound to this browser session. Without it
// every mutating request is refused by the server.
async function loadSession() {
  const session = await getJSON(SESSION_URL);
  csrfToken = session.csrf || '';
  if (!csrfToken) throw new Error('no csrf token');
}

function startTimers() {
  setInterval(() => { if (!document.hidden) refreshAll(); }, REFRESH_STATE_MS);
  setInterval(() => { if (!document.hidden) refreshLogsSafe(); }, REFRESH_LOGS_MS);
  setInterval(() => { if (!document.hidden) refreshMetricsSafe(); }, REFRESH_METRICS_MS);
}

// The page is split into tabs (capsules): one group of cards is visible at a
// time. Every card keeps its own «Сохранить» (data-scope), so hiding a group
// never changes what a save writes. The chosen tab survives a reload.
const TAB_KEY = 'midi-panel-tab';
const THEME_KEY = 'midi-panel-theme';

function showTab(name, remember) {
  const buttons = Array.from(document.querySelectorAll('#tabs [data-tab]'));
  if (!buttons.some((button) => button.dataset.tab === name)) name = buttons.length ? buttons[0].dataset.tab : '';
  for (const button of buttons) button.setAttribute('aria-pressed', String(button.dataset.tab === name));
  for (const card of document.querySelectorAll('[data-group]')) {
    card.classList.toggle('is-hidden', card.dataset.group !== name);
  }
  if (remember) {
    try { localStorage.setItem(TAB_KEY, name); } catch (error) { /* private mode: no memory */ }
  }
}

function initTabs() {
  const buttons = Array.from(document.querySelectorAll('#tabs [data-tab]'));
  if (!buttons.length) return;
  let saved = '';
  try { saved = localStorage.getItem(TAB_KEY) || ''; } catch (error) { saved = ''; }
  showTab(saved, false);
  for (const button of buttons) button.addEventListener('click', () => showTab(button.dataset.tab, true));
  // 1..N jump between tabs, unless the operator is typing.
  document.addEventListener('keydown', (event) => {
    if (event.ctrlKey || event.metaKey || event.altKey) return;
    const target = event.target;
    if (target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName)) return;
    const index = Number(event.key) - 1;
    if (Number.isInteger(index) && index >= 0 && index < buttons.length) showTab(buttons[index].dataset.tab, true);
  });
}

function initTheme() {
  let theme = '';
  try { theme = localStorage.getItem(THEME_KEY) || ''; } catch (error) { theme = ''; }
  document.body.classList.toggle('light', theme === 'light');
  const toggle = document.getElementById('theme-toggle');
  if (!toggle) return;
  toggle.addEventListener('click', () => {
    const light = document.body.classList.toggle('light');
    try { localStorage.setItem(THEME_KEY, light ? 'light' : 'dark'); } catch (error) { /* no memory */ }
  });
}

document.addEventListener('DOMContentLoaded', () => {
  initTabs();
  initTheme();
  initVoiceSliders();
  initStationVolume();
  document.getElementById('refresh').addEventListener('click', () => {
    refreshAll();
    refreshLogsSafe();
    refreshMetricsSafe();
  });
  document.getElementById('warn-only').addEventListener('change', refreshLogsSafe);
  for (const button of document.querySelectorAll('#metrics-range .range')) {
    button.addEventListener('click', () => {
      metricsRange = Number(button.dataset.range) || 3600;
      for (const peer of document.querySelectorAll('#metrics-range .range')) {
        peer.classList.toggle('is-active', peer === button);
      }
      refreshMetricsSafe();
    });
  }
  document.getElementById('voice-form').addEventListener('submit', submitVoice);
  document.getElementById('voice-form').addEventListener('input', () => { voiceFormDirty = true; });
  document.getElementById('voice-test').addEventListener('click', testVoice);
  const settingsForm = document.getElementById('settings-form');
  settingsForm.addEventListener('submit', submitSettings);
  // Enter in a field saves the section the caret is in. Left to the browser, an
  // implicit submission clicks the first submit button of the form — the
  // providers — so a caret in the roles would write the providers instead.
  settingsForm.addEventListener('keydown', (event) => {
    if (event.key !== 'Enter' || event.isComposing || event.defaultPrevented) return;
    const control = event.target;
    if (!control || control.tagName !== 'INPUT') return;
    if (control.type === 'checkbox' || control.type === 'radio') return;
    const section = control.closest('section[id]');
    const button = section ? section.querySelector('button[data-scope]') : null;
    if (!button) return;
    event.preventDefault();
    button.click();
  });
  settingsForm.addEventListener('input', editSettingsField);
  settingsForm.addEventListener('change', editSettingsField);
  settingsForm.addEventListener('click', removeSettingsItem);
  settingsForm.addEventListener('click', providerAction);
  document.getElementById('add-llm-provider').addEventListener('click', () => {
    providersState.push({ name: '', api: 'chat_completions', base_url: '', model: '' });
    renderProviders(true);
  });
  document.getElementById('add-wake-word').addEventListener('click', () => {
    wordsState.push({ word: '', action: 'listen', text: '', agent: '' });
    renderWakeWords(true);
  });
  document.getElementById('add-agent').addEventListener('click', () => {
    agentsState.push({ name: '', system_prompt: '', voice: '', model: '', description: '', peers: false, sysadmin: false });
    renderAgents(true);
  });
  // Routing lives inside the same form, but keeps its own state and controls:
  // the generic settings handler stops at controls without data-field.
  settingsForm.addEventListener('input', editRoutingRule);
  settingsForm.addEventListener('change', editRoutingRule);
  settingsForm.addEventListener('click', routingRuleAction);
  document.getElementById('add-routing-rule').addEventListener('click', addRoutingRule);
  document.getElementById('routing-reset').addEventListener('click', resetRouting);
  document.getElementById('routing-preset-apply').addEventListener('click', applyRoutingPreset);
  document.getElementById('routing-preset').addEventListener('change', presetNote);
  document.getElementById('routing-test').addEventListener('click', testRouting);
  document.getElementById('routing-test-text').addEventListener('keydown', (event) => {
    // Enter inside the phrase box runs the check instead of submitting the
    // whole settings form: the box is a probe, not a field to save.
    if (event.key !== 'Enter') return;
    event.preventDefault();
    testRouting();
  });
  document.getElementById('update-check').addEventListener('click', refreshUpdate);
  document.getElementById('password-form').addEventListener('submit', submitPassword);
  loadSession()
    .then(() => {
      refreshAll();
      refreshLogsSafe();
      refreshMetricsSafe();
      refreshSettingsSafe();
      loadWakeDictionary();
      refreshStationVolume();
      // The release check is cached on the station for ten minutes, so opening
      // the page does not spend the unauthenticated GitHub rate limit.
      refreshUpdate();
      startTimers();
    })
    .catch((error) => {
      console.error(error);
      document.getElementById('stamp').textContent = 'нет связи';
    });
});

// ---- Sysadmin tab. Everything that comes from a host or a model goes in via
// textContent / .value only. The queue refreshes every 3 s while the tab is
// visible; forms are redrawn only on open and after a save, so typing is safe.
const SYSADMIN_URL = '/api/panel/sysadmin/';
let sysadminState = null;

function sysadminVisible() {
  const section = document.getElementById('section-sysadmin');
  return Boolean(section) && !section.classList.contains('is-hidden') && !document.hidden;
}

function sysadminSay(id, text, isError) {
  const node = document.getElementById(id);
  if (!node) return;
  node.textContent = text || '';
  node.classList.toggle('error', Boolean(isError));
}

function sysadminLeft(expires) {
  const sec = Math.max(0, Math.round((new Date(expires).getTime() - Date.now()) / 1000));
  return Math.floor(sec / 60) + ':' + String(sec % 60).padStart(2, '0');
}

function sysadminItemCard(item, pending) {
  const card = el('div', 'settings-card');
  const head = el('strong', '', (item.host || '—') + ' · ' + item.tool);
  card.appendChild(head);
  card.appendChild(el('div', '', item.summary));
  const meta = pending
    ? 'источник: ' + item.origin + ' · осталось ' + sysadminLeft(item.expires)
    : 'источник: ' + item.origin + ' · ' + item.status + (item.scope ? ' · ' + item.scope : '') + (item.note ? ' · ' + item.note : '');
  card.appendChild(el('div', 'muted small', meta));
  if (!pending && (item.result || item.error)) {
    const out = el('pre', 'update-command', item.error ? 'ошибка: ' + item.error : item.result);
    card.appendChild(out);
  }
  if (pending) {
    const actions = el('div', 'actions');
    const choices = item.remember
      ? [['once', 'Только сейчас', ''], ['session', 'В сессии', ''], ['always', 'Всегда', ''], ['deny', 'Отклонить', 'ghost']]
      : [['once', 'Разрешить', ''], ['deny', 'Отклонить', 'ghost']];
    const buttons = choices.map(([scope, label, cls]) => {
      const button = el('button', cls, label);
      button.type = 'button';
      return [scope, button];
    });
    const decide = async (scope) => {
      for (const [, button] of buttons) button.disabled = true;
      try {
        await postJSON(SYSADMIN_URL + 'approve', { id: item.id, scope });
      } catch (error) {
        sysadminSay('sysadmin-error', error.message, true);
      }
      refreshSysadmin(false);
    };
    for (const [scope, button] of buttons) button.addEventListener('click', () => decide(scope));
    actions.append(...buttons.map(([, button]) => button));
    card.appendChild(actions);
  }
  return card;
}

function renderSysadminQueue(state) {
  const queue = document.getElementById('sysadmin-queue');
  const recent = document.getElementById('sysadmin-recent');
  if (!queue || !recent) return;
  const pending = Array.isArray(state.queue) ? state.queue : [];
  queue.replaceChildren();
  if (!pending.length) queue.appendChild(el('p', 'muted small', 'Ничего не ждёт.'));
  if (pending.length > 1) {
    const bulk = el('div', 'actions');
    for (const [scope, label, cls] of [['once', 'Разрешить все (' + pending.length + ')', ''], ['deny', 'Отклонить все', 'ghost']]) {
      const button = el('button', cls, label);
      button.type = 'button';
      button.addEventListener('click', async () => {
        if (scope === 'once' && !window.confirm('Разрешить все ' + pending.length + ' заявок один раз?')) return;
        button.disabled = true;
        const failures = [];
        for (const item of pending) {
          try {
            await postJSON(SYSADMIN_URL + 'approve', { id: item.id, scope });
          } catch (error) {
            failures.push(error.message);
          }
        }
        if (failures.length) sysadminSay('sysadmin-error', failures.join('; '), true);
        refreshSysadmin(false);
      });
      bulk.appendChild(button);
    }
    queue.appendChild(bulk);
  }
  for (const item of pending) queue.appendChild(sysadminItemCard(item, true));
  // The fold opens by itself when a new request arrives and stays as the
  // operator left it otherwise.
  const queueBox = document.getElementById('sysadmin-queue-box');
  const queueTitle = document.getElementById('sysadmin-queue-title');
  if (queueTitle) queueTitle.textContent = 'Ожидают подтверждения' + (pending.length ? ' (' + pending.length + ')' : '');
  if (queueBox && pending.length > (renderSysadminQueue.lastPending || 0)) queueBox.open = true;
  renderSysadminQueue.lastPending = pending.length;
  recent.replaceChildren();
  const done = Array.isArray(state.recent) ? state.recent : [];
  const recentTitle = document.getElementById('sysadmin-recent-title');
  if (recentTitle) recentTitle.textContent = 'Недавние' + (done.length ? ' (' + done.length + ')' : '');
  if (!done.length) recent.appendChild(el('p', 'muted small', 'Пока пусто.'));
  for (const item of done) recent.appendChild(sysadminItemCard(item, false));
  const err = document.getElementById('sysadmin-error');
  if (err) {
    err.hidden = !state.error;
    err.textContent = state.error ? 'Ошибка настроек: ' + state.error : '';
  }
}

function fillSysadminHostForm(host) {
  const h = host || { name: '', addr: '', port: '', user: '', key_path: '', mac: '', has_password: false };
  document.getElementById('sa-host-name').value = h.name;
  document.getElementById('sa-host-addr').value = h.addr;
  document.getElementById('sa-host-port').value = h.port || '';
  document.getElementById('sa-host-user').value = h.user;
  document.getElementById('sa-host-pass').value = '';
  document.getElementById('sa-host-clear').checked = false;
  document.getElementById('sa-host-key').value = h.key_path || '';
  document.getElementById('sa-host-mac').value = h.mac || '';
  document.getElementById('sa-host-pass-note').textContent = h.has_password ? 'пароль сохранён; пусто — не менять' : '';
}

function renderSysadminHosts(state) {
  const list = document.getElementById('sysadmin-hosts');
  list.replaceChildren();
  const hosts = Array.isArray(state.hosts) ? state.hosts : [];
  if (!hosts.length) list.appendChild(el('p', 'muted small', 'Хостов нет.'));
  for (const host of hosts) {
    const row = el('div', 'settings-card');
    const title = host.name + ' — ' + host.user + '@' + host.addr + ':' + host.port;
    row.appendChild(el('strong', '', title));
    row.appendChild(el('div', 'muted small', (host.has_password ? 'пароль сохранён' : 'без пароля') +
      (host.key_path ? ' · ключ ' + host.key_path : '') + (host.mac ? ' · MAC ' + host.mac : '')));
    const actions = el('div', 'actions');
    const edit = el('button', 'ghost', 'Изменить');
    const del = el('button', 'ghost', 'Удалить');
    edit.type = 'button';
    del.type = 'button';
    edit.addEventListener('click', () => fillSysadminHostForm(host));
    del.addEventListener('click', async () => {
      if (!window.confirm('Удалить хост ' + host.name + '?')) return;
      try {
        await postJSON(SYSADMIN_URL + 'host/delete', { name: host.name });
        sysadminSay('sa-host-status', 'удалён', false);
      } catch (error) {
        sysadminSay('sa-host-status', error.message, true);
      }
      refreshSysadmin(true);
    });
    actions.append(edit, del);
    row.appendChild(actions);
    list.appendChild(row);
  }
}

function sysadminRuleRow(rule) {
  const row = el('div', 'actions');
  row.dataset.kind = 'sa-rule';
  const tool = el('input');
  tool.type = 'text';
  tool.placeholder = 'инструмент (sysadmin_exec)';
  tool.value = rule.tool || '';
  tool.dataset.field = 'tool';
  const match = el('input');
  match.type = 'text';
  match.placeholder = 'подстрока (например apt)';
  match.value = rule.match || '';
  match.dataset.field = 'match';
  const mode = el('select');
  for (const m of ['allow', 'ask', 'deny']) mode.appendChild(option(m, m));
  mode.value = rule.mode || 'ask';
  mode.dataset.field = 'mode';
  const del = el('button', 'ghost', '×');
  del.type = 'button';
  del.addEventListener('click', () => row.remove());
  row.append(tool, match, mode, del);
  return row;
}

function renderSysadminTelegram(state) {
  const tg = state.telegram || { enabled: false, users: [], agent: '', has_token: false };
  document.getElementById('sa-tg-enabled').checked = Boolean(tg.enabled);
  document.getElementById('sa-tg-users').value = (tg.users || []).join(', ');
  document.getElementById('sa-tg-agent').value = tg.agent || '';
  document.getElementById('sa-tg-token').value = '';
  document.getElementById('sa-tg-clear').checked = false;
  document.getElementById('sa-tg-token-note').textContent = tg.has_token ? 'токен сохранён; пусто — не менять' : 'токен не задан';
}

function renderSysadminPolicy(state) {
  const policy = state.policy || { default: 'ask', rules: [] };
  document.getElementById('sa-policy-default').value = policy.default || 'ask';
  const list = document.getElementById('sysadmin-rules');
  list.replaceChildren();
  for (const rule of policy.rules || []) list.appendChild(sysadminRuleRow(rule));
  const charter = document.getElementById('sa-charter');
  const info = state.charter || {};
  if (charter && info.text !== undefined && document.activeElement !== charter && charter.dataset.dirty !== '1') {
    charter.value = info.text;
    document.getElementById('sa-charter-note').textContent = info.custom ? 'свой текст' : 'стандартный текст';
  }
}

function renderSysadminMemory(state) {
  const pp = document.getElementById('sysadmin-passport');
  pp.replaceChildren();
  const passport = state.passport || {};
  for (const section of Object.keys(passport).sort()) pp.appendChild(sysadminPassportRow(section, passport[section]));
  const facts = document.getElementById('sysadmin-facts');
  facts.replaceChildren();
  const list = Array.isArray(state.facts) ? state.facts : [];
  if (!list.length) facts.appendChild(el('p', 'muted small', 'Фактов нет.'));
  for (const fact of list) {
    const row = el('div', 'settings-card');
    row.appendChild(el('strong', '', fact.topic));
    row.appendChild(el('div', '', fact.text));
    row.appendChild(el('div', 'muted small', new Date(fact.time).toLocaleString()));
    const forget = el('button', 'ghost', 'Забыть');
    forget.type = 'button';
    forget.addEventListener('click', async () => {
      try {
        await postJSON(SYSADMIN_URL + 'memory/forget', { id: fact.id });
      } catch (error) {
        sysadminSay('sysadmin-error', error.message, true);
      }
      refreshSysadmin(true);
    });
    row.appendChild(forget);
    facts.appendChild(row);
  }
}

function sysadminPassportRow(section, text) {
  const row = el('div', 'settings-card');
  row.appendChild(el('strong', '', section));
  const area = el('textarea');
  area.rows = 4;
  area.maxLength = 4000;
  area.spellcheck = false;
  area.value = text || '';
  const save = el('button', '', 'Сохранить раздел');
  save.type = 'button';
  save.dataset.sysadminSave = 'passport';
  const status = el('span', 'muted small');
  save.addEventListener('click', async () => {
    try {
      await postJSON(SYSADMIN_URL + 'passport', { section, text: area.value });
      status.textContent = 'сохранено';
      status.classList.remove('error');
    } catch (error) {
      status.textContent = error.message;
      status.classList.add('error');
    }
  });
  const actions = el('div', 'actions');
  actions.append(save, status);
  row.append(area, actions);
  return row;
}

async function refreshSysadmin(full) {
  try {
    const state = await getJSON(SYSADMIN_URL + 'state');
    sysadminState = state;
    renderSysadminQueue(state);
    if (full) {
      renderSysadminHosts(state);
      renderSysadminPolicy(state);
      renderSysadminMemory(state);
      renderSysadminTelegram(state);
    }
  } catch (error) {
    sysadminSay('sysadmin-error', error.message, true);
    const err = document.getElementById('sysadmin-error');
    if (err) err.hidden = false;
  }
}

function initSysadmin() {
  if (!document.getElementById('section-sysadmin')) return;
  fillSysadminHostForm(null);
  document.getElementById('sa-host-save').addEventListener('click', async () => {
    const portText = document.getElementById('sa-host-port').value.trim();
    const body = {
      name: document.getElementById('sa-host-name').value.trim(),
      addr: document.getElementById('sa-host-addr').value.trim(),
      port: portText ? Number(portText) : 0,
      user: document.getElementById('sa-host-user').value.trim(),
      key_path: document.getElementById('sa-host-key').value.trim(),
      mac: document.getElementById('sa-host-mac').value.trim(),
      password: document.getElementById('sa-host-pass').value,
      clear_password: document.getElementById('sa-host-clear').checked
    };
    try {
      await postJSON(SYSADMIN_URL + 'host', body);
      sysadminSay('sa-host-status', 'сохранено', false);
      fillSysadminHostForm(null);
      refreshSysadmin(true);
    } catch (error) {
      sysadminSay('sa-host-status', error.message, true);
    }
  });
  document.getElementById('sa-tg-save').addEventListener('click', async () => {
    const raw = document.getElementById('sa-tg-users').value.split(/[\s,;]+/).filter(Boolean);
    const users = raw.map(Number);
    if (users.some((n) => !Number.isSafeInteger(n) || n <= 0)) {
      sysadminSay('sa-tg-status', 'user_id — положительные числа через запятую', true);
      return;
    }
    const body = {
      enabled: document.getElementById('sa-tg-enabled').checked,
      users,
      agent: document.getElementById('sa-tg-agent').value.trim(),
      token: document.getElementById('sa-tg-token').value.trim(),
      clear_token: document.getElementById('sa-tg-clear').checked
    };
    try {
      await postJSON(SYSADMIN_URL + 'telegram', body);
      sysadminSay('sa-tg-status', 'сохранено, бот перезапущен', false);
      refreshSysadmin(true);
    } catch (error) {
      sysadminSay('sa-tg-status', error.message, true);
    }
  });
  document.getElementById('sa-rule-add').addEventListener('click', () => {
    document.getElementById('sysadmin-rules').appendChild(sysadminRuleRow({ tool: '', match: '', mode: 'ask' }));
  });
  document.getElementById('sa-policy-save').addEventListener('click', async () => {
    const rules = [];
    for (const row of document.querySelectorAll('#sysadmin-rules [data-kind="sa-rule"]')) {
      rules.push({
        tool: row.querySelector('[data-field="tool"]').value.trim(),
        match: row.querySelector('[data-field="match"]').value.trim(),
        mode: row.querySelector('[data-field="mode"]').value
      });
    }
    try {
      await postJSON(SYSADMIN_URL + 'policy', { default: document.getElementById('sa-policy-default').value, rules });
      sysadminSay('sa-policy-status', 'сохранено', false);
    } catch (error) {
      sysadminSay('sa-policy-status', error.message, true);
    }
  });
  const charterBox = document.getElementById('sa-charter');
  charterBox.addEventListener('input', () => { charterBox.dataset.dirty = '1'; });
  const saveCharter = async (reset) => {
    try {
      const out = await postJSON(SYSADMIN_URL + 'charter', { text: charterBox.value, reset });
      charterBox.value = out.charter || '';
      charterBox.dataset.dirty = '';
      document.getElementById('sa-charter-note').textContent = out.custom ? 'свой текст' : 'стандартный текст';
      sysadminSay('sa-charter-status', reset ? 'сброшено' : 'сохранено', false);
    } catch (error) {
      sysadminSay('sa-charter-status', error.message, true);
    }
  };
  document.getElementById('sa-charter-save').addEventListener('click', () => saveCharter(false));
  document.getElementById('sa-charter-reset').addEventListener('click', () => saveCharter(true));
  document.getElementById('sa-pp-add').addEventListener('click', async () => {
    const input = document.getElementById('sa-pp-section');
    const section = input.value.trim();
    if (!section) return;
    try {
      await postJSON(SYSADMIN_URL + 'passport', { section, text: '—' });
      input.value = '';
      sysadminSay('sa-pp-status', '', false);
      refreshSysadmin(true);
    } catch (error) {
      sysadminSay('sa-pp-status', error.message, true);
    }
  });
  let wasVisible = false;
  setInterval(() => {
    const visible = sysadminVisible();
    if (visible) refreshSysadmin(!wasVisible);
    wasVisible = visible;
  }, 3000);
  document.querySelectorAll('#tabs [data-tab="sysadmin"]').forEach((button) => {
    button.addEventListener('click', () => { wasVisible = true; refreshSysadmin(true); });
  });
  if (sysadminVisible()) { wasVisible = true; refreshSysadmin(true); }
}

document.addEventListener('DOMContentLoaded', initSysadmin);

