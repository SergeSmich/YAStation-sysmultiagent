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
const PASSWORD_URL = '/api/panel/password';
const METRICS_URL = '/api/panel/metrics?range=';
const REFRESH_STATE_MS = 5000;
const REFRESH_LOGS_MS = 15000;
const REFRESH_METRICS_MS = 10000;

let csrfToken = '';
let voiceFormDirty = false;
let metricsRange = 3600;
let agentsState = [];
let providersState = [];
let providerKeysState = {};
let providerModels = {};
let wakeDictionary = [];
let wakeDictionaryNote = '';
let wakePage = '';
let wordsState = [];
let settingsActions = ['listen', 'ask', 'command'];
let settingsVoices = [];

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
  ask: 'тихий вопрос роли',
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
const PROVIDER_APIS = ['chat_completions', 'responses'];

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

function fillModels(datalist, ids) {
  if (!datalist) return;
  datalist.replaceChildren();
  for (const id of ids || []) {
    const item = el('option');
    item.value = id;
    datalist.appendChild(item);
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

    const name = settingsControl('input', 'name', provider.name, 'например, deepseek');
    name.type = 'text';
    name.autocomplete = 'off';
    row.appendChild(settingsField('Название', name));

    const api = settingsControl('select', 'api', provider.api || 'chat_completions');
    for (const value of PROVIDER_APIS) api.appendChild(option(value, value));
    api.value = provider.api || 'chat_completions';
    row.appendChild(settingsField('Формат API', api));

    const base = settingsControl('input', 'base_url', provider.base_url, 'https://api.deepseek.com/v1');
    base.type = 'text';
    base.autocomplete = 'off';
    row.appendChild(settingsField('Адрес API', base, 'wide'));

    const model = settingsControl('input', 'model', provider.model, 'например, deepseek-chat');
    model.type = 'text';
    model.autocomplete = 'off';
    const models = el('datalist');
    models.id = 'llm-models-' + index;
    models.className = 'model-select';
    fillModels(models, provider._models || providerModels[provider.name] || []);
    model.setAttribute('list', models.id);
    const modelField = settingsField('Модель', model);
    modelField.appendChild(models);
    row.appendChild(modelField);

    const key = settingsControl('input', 'api_key', provider._key, '');
    key.type = 'password';
    key.autocomplete = 'new-password';
    key.placeholder = provider._clearKey ? 'ключ будет удалён' : 'пусто — не менять';
    const keyField = settingsField('API-ключ', key);
    keyField.appendChild(el('span', 'key-state'));
    row.appendChild(keyField);

    const actions = el('div', 'row-actions');
    actions.appendChild(providerButton('Проверить', 'test'));
    actions.appendChild(providerButton('Модели', 'models'));
    actions.appendChild(providerButton('Забыть ключ', 'forget'));
    const remove = el('button', 'ghost', 'Удалить');
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

    const text = settingsControl('textarea', 'text', word.text, 'Фраза для роли или команды');
    text.rows = 3;
    const textField = settingsField('Текст / фраза', text, 'wide');
    row.appendChild(textField);

    const remove = el('button', 'ghost', 'Удалить');
    remove.type = 'button';
    remove.dataset.remove = 'word';
    row.appendChild(remove);
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

    const name = settingsControl('input', 'name', agent.name, 'латиница в нижнем регистре, цифры, - и _');
    name.type = 'text';
    name.autocomplete = 'off';
    row.appendChild(settingsField('Имя роли', name));

    const prompt = settingsControl('textarea', 'system_prompt', agent.system_prompt);
    prompt.rows = 5;
    row.appendChild(settingsField('Системный промпт', prompt, 'wide'));

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

    const model = settingsControl('input', 'model', agent.model, 'модель по умолчанию');
    model.type = 'text';
    model.autocomplete = 'off';
    row.appendChild(settingsField('Модель', model));

    const remove = el('button', 'ghost', 'Удалить');
    remove.type = 'button';
    remove.dataset.remove = 'agent';
    row.appendChild(remove);
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
  const linked = Boolean(agent.provider);
  model.disabled = linked;
  model.placeholder = linked ? 'из подключения' : 'модель по умолчанию';
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
    text.closest('.field').hidden = listening;
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

function renderSettings(payload, force) {
  agentsState = Array.isArray(payload.agents) ? payload.agents : [];
  providersState = Array.isArray(payload.llm_providers) ? payload.llm_providers : [];
  providerKeysState = payload.provider_keys && typeof payload.provider_keys === 'object'
    ? payload.provider_keys
    : {};
  const wakeWords = payload.wake_words || {};
  wordsState = Array.isArray(wakeWords.words) ? wakeWords.words : [];
  settingsActions = Array.isArray(payload.actions) ? payload.actions : ['listen', 'ask', 'command'];
  settingsVoices = Array.isArray(payload.voices) ? payload.voices : [];
  wakePage = typeof payload.wake_page === 'string' ? payload.wake_page : '';

  const form = document.getElementById('settings-form');
  if (force || !form.contains(document.activeElement)) {
    document.getElementById('settings-dry-run').checked = Boolean(wakeWords.dry_run);
  }
  renderProviders(force);
  renderWakeWords(force);
  renderAgents(force);
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
    agent[control.dataset.field] = control.value;
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
  const name = String(provider.name || '').trim();
  if (!name) {
    status.textContent = 'укажите название подключения и сохраните его';
    status.classList.add('error');
    return;
  }
  if (action === 'models') {
    status.textContent = 'запрашиваю список моделей…';
    try {
      const payload = await postJSON(LLM_MODELS_URL, { provider: name });
      const ids = Array.isArray(payload && payload.models) ? payload.models : [];
      provider._models = ids;
      providerModels[name] = ids;
      fillModels(row.querySelector('.model-select'), ids);
      status.textContent = ids.length
        ? 'моделей: ' + ids.length + ' — выберите в поле «Модель»'
        : 'провайдер не вернул список моделей, введите вручную';
    } catch (error) {
      status.textContent = error.message + ' (подключение должно быть сохранено)';
      status.classList.add('error');
    }
    return;
  }
  status.textContent = 'проверяю подключение…';
  try {
    const payload = await postJSON(LLM_TEST_URL, { provider: name });
    status.textContent = 'ответ за ' + (payload && payload.latency_ms ? payload.latency_ms : 0) +
      ' мс: ' + ((payload && payload.sample) || 'пусто');
  } catch (error) {
    status.textContent = error.message + ' (подключение должно быть сохранено)';
    status.classList.add('error');
  }
}

async function submitSettings(event) {
  event.preventDefault();
  const status = document.getElementById('settings-status');
  status.classList.remove('error');
  // Reported here, in the operator's words, instead of the strict decoder's
  // "llm_providers[].name must be 1..32 characters…" from the server.
  for (const provider of providersState) {
    if (!String(provider.name || '').trim() ||
        !String(provider.base_url || '').trim() ||
        !String(provider.model || '').trim()) {
      status.textContent = 'у подключения нужны название, адрес API и модель';
      status.classList.add('error');
      return;
    }
  }
  status.textContent = 'сохраняю…';
  try {
    const payload = await postJSON(SETTINGS_URL, {
      // All three sections go back whole: the write path replaces a present
      // section, and a provider list that is edited as a list cannot be patched.
      llm_providers: providersState.map(providerPayload),
      agents: agentsState,
      wake_words: {
        dry_run: document.getElementById('settings-dry-run').checked,
        words: wordsState
      }
    });
    status.textContent = payload && payload.unchanged ? 'без изменений' : 'применено';
    renderSettings(payload || {}, true);
    loadWakeDictionary();
  } catch (error) {
    status.textContent = error.message;
    status.classList.add('error');
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
  status.textContent = play ? 'синтез и воспроизведение на станции…' : 'синтез…';
  try {
    const payload = await postJSON(VOICE_TEST_URL, {
      voice: document.getElementById('voice-name').value,
      play: play,
      per_case_ms: 5000
    });
    const result = payload && payload.results && payload.results[0];
    if (result && result.ok) {
      status.textContent = 'голос принят, ' + result.elapsed_ms + ' мс' + (play ? ' — слушайте станцию' : '');
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

document.addEventListener('DOMContentLoaded', () => {
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
    agentsState.push({ name: '', system_prompt: '', voice: '', model: '' });
    renderAgents(true);
  });
  document.getElementById('password-form').addEventListener('submit', submitPassword);
  loadSession()
    .then(() => {
      refreshAll();
      refreshLogsSafe();
      refreshMetricsSafe();
      refreshSettingsSafe();
      loadWakeDictionary();
      startTimers();
    })
    .catch((error) => {
      console.error(error);
      document.getElementById('stamp').textContent = 'нет связи';
    });
});
