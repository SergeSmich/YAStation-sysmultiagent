---
id: L-0009
title: Okno Agenty: chipy roley i mini-dialog (shag 1 iz A-0008)
from: opencode
to: arena
kind: task
status: open
branch: local/opencode
needs: A-0008
---

# L-0009: окно «Агенты» — чипы ролей и мини-диалог (шаг 1 из A-0008)

Основание: `A-0008` §3 и §4, шаг 1. В этом круге — **только шаг 1**.
«Длина ответа» (шаг 2), токенная шкала и сетка (шаг 3), тема/manifest/drag-and-drop
(шаг 4) — не в этом круге, не делать.

## 0. Поправка к A-0008, которая меняет реализацию

Мини-диалог **нельзя** построить на `POST /api/panel/llm/ask`, как предлагает A-0008:
в `cmd/quasar-bridge/panel_llm_ask.go` вопрос зашит константами
(`panelLLMAskSystem`, `panelLLMAskPrompt` = «Сколько будет дважды два?»), ответ режется
до 200 рун (`panelLLMSample`), лимит 512 токенов, а вердикт ищет «четыре» и на любой
другой ответ говорит «модель ответила не то». Поля `question` в теле нет, а декодер
включён с `DisallowUnknownFields()` — лишнее поле даст 400.

`llm/ask` не менять: им пользуется кнопка «Проверить модель» в карточке роли.

## 1. Новый эндпоинт `POST /api/panel/llm/chat`

Новые файлы: `cmd/quasar-bridge/panel_llm_chat.go`, `panel_llm_chat_test.go`.
Регистрация в `panel.go` рядом с остальными: `mux.HandleFunc("POST /api/panel/llm/chat", p.requireMutation(p.apiLLMChat))`.

Тело: `{"agent":"helper","provider":"groq","model":"openai/gpt-oss-120b","question":"..."}`.

- Декодер как в ask: `stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, panelLLMBodyLimit))` + `DisallowUnknownFields()`.
- `question` — trim, 1..500 рун. Пусто → 400 «вопрос пустой», длиннее → 400 «вопрос длиннее 500 символов».
- Резолв цели — как в ask: `cfg.resolveLLM(agentConfig{Provider: normalizeProviderName(body.Provider), Model: strings.TrimSpace(body.Model)})`. Русские тексты ошибок — те же: «нейросеть не сохранена: сначала сохраните её», «не задана модель: выберите её в карточке или у нейросети».
- System prompt: сохранённый `system_prompt` роли из `readConfig` по `body.Agent`; роли нет или промпт пуст → `panelLLMAskSystem`.
- Ключ: `apiKey(stored, target)`, `stored` из `p.settings.Secrets` — как в ask. Ключ в ответ не попадает.
- Параметры: `MaxOutputTokens` = `max_output_tokens` роли, если > 0, иначе `panelLLMAskTokens`; `ChatParameters`, `ReasoningEffort`, `API` — как в ask; таймаут `panelLLMAskTimeout`.
- Ответ 200: `{"ok":true,"answer":"…","latency_ms":N,"provider":"…","model":"…","verdict":"…"}`, где `answer` — полный текст, обрезанный до 4000 рун с «…».
- Ошибка модели: 200 + `{"ok":false,"verdict":explainLLMError(err),"error":err.Error()}` — как в ask.
- Аудит: `p.audit.record("llm_chat", r, result, map[string]any{"provider":…, "latency_ms":…})`. Если список событий аудита закрыт — добавь `llm_chat` туда, где перечислены остальные.

Критерий: запрос с сохранённой нейросетью возвращает ответ именно на заданный вопрос, а не «четыре».

## 2. Чипы ролей

`panel.html`: в `#section-roles` (строка 64) сразу под `#role-call-box`, над `#agents-list`, добавить
`<div id="role-chips" class="role-chips"></div>`.

`panel.js`:

- `renderRoleChips()` строит чипы из **сохранённого** состояния (`savedAgentObjs`), не из `agentsState`: чип не должен показывать несохранённые правки.
- Чип: имя + точка готовности. Текст для `title` — та же строка, что у `paintRoleReadiness` (`panel.js:1674`). Вынеси общий `roleReadinessText(agent)` и оставь `paintRoleReadiness` тонкой обёрткой, чтобы тексты не разъехались.
- Активный `main_agent` — класс `is-main`. Пустой `main_agent` — ни один чип не активен, это нормально.
- Клик: `select.value = name; saveMainAgent();` — единственный путь сохранения (частичный патч `{main_agent}` в `SETTINGS_URL`, `panel.js:1378`). После успеха — `renderRoleChips()` и `renderMainAgent(true)`.
- Чип «+ роль» не делать: добавление уже есть кнопкой `#add-agent`.
- Вызовы `renderRoleChips()` добавить в `renderAgents` (`panel.js:1461`), в `renderMainAgent` (`1338`), после `saveMainAgent` и после сохранения карточки роли.

## 3. Спойлер «Диалог» в карточке роли

`panel.js`: новая `roleDialogField(row, agent)` по образцу `roleCallField` (`1556`), вызов рядом с `row.appendChild(roleCallField(row, agent))` (`1529`). Новых полей в конфиге нет — ничего не сохраняется.

Внутри:

- `input` вопроса (placeholder «спросите роль, например: который час?») и кнопка «Спросить» (`data-role-ask`);
- `div.role-dialog-transcript`: вопрос оператора и ответ роли;
- строка статуса: латентность и вердикт; ошибки — через `setRowStatus(row, …, 'error')`;
- кнопка «Озвучить» (`data-role-say`), disabled до первого ответа: `VOICE_TEST_URL` (`panel.js:8`) с `{voice: agent.voice || '', text: <ответ, обрезанный до 300 рун>, play: true}`. 503 «no upstream session» → «станция не на связи — озвучить некому»; успех → «озвучено»;
- «Спросить»: `postJSON(LLM_CHAT_URL, {agent: agent.name, provider: agent.provider, model: agent.model, question})`, где `agent.name` — значение поля «Имя роли» (у новой роли пустое — тогда промпт общий);
- истории нет: каждый вопрос независимый, новый ответ заменяет старый.

## 4. Стили

`panel.css`: `.role-chips` (flex, wrap), `.role-chip` (+ `.is-main`, `.dot`, `.dot.ok`, `.dot.bad`), `.role-dialog`, `.role-dialog-transcript`. Только текущие токены, никаких новых хардкод-цветов. На узких экранах чипы переносятся, горизонтального скролла нет.

## 5. Тесты и сборка

- `panel_llm_chat_test.go`: ответ на произвольный вопрос (поддельный сервер); пустой вопрос → 400; неизвестное поле → 400; роль с промптом → промпт уходит в system; роль без промпта → `panelLLMAskSystem`; ошибка модели → `ok:false` и русский вердикт; ключа нет в ответе.
- `panel_ui_test.go`: добавить `.role-chips`, `.role-chip`, `.role-dialog` в список маркеров (строка 151).
- Зелёные: `go build ./...`, `go vet ./...`, `go test ./cmd/quasar-bridge ./internal/bridge`.

## Критерий готовности

На живой станции, «Агенты» → «Роли»:

1. над карточками видны чипы всех сохранённых ролей с готовностью, активная помечена;
2. клик по чипу переключает «Основную модель» и сохраняет: в `config.json` меняется только `main_agent`, несохранённые правки карточек не теряются;
3. в карточке роли спойлер «Диалог»: вопрос → ответ в транскрипте + латентность; повторный вопрос заменяет ответ;
4. «Озвучить» читает ответ голосом станции, когда станция на связи;
5. «Проверить модель» (`llm/ask`) работает как раньше и по-прежнему задаёт свой фиксированный вопрос;
6. ни в одном GET-ответе панели не появилось ключей.

## Ограничения

- `llm/ask`, `llm/check`, `voice/test` не менять; новый код — отдельными файлами.
- Не трогать `main_agent`, `role_call`, `settingsSnapshot`, `secrets.json`, деплой и механизм обновления.
- «Длину ответа» не делать. Когда дойдёт: пустое значение роли = наследовать `llm_max_output_tokens` (как в `resolveLLM`), **не** 260 — поправка к A-0008 §4 шаг 2.
- CSS/JS из MidiPult копипастой не переносить; стили писать свои, MIT-атрибуция уже есть.
- Комментарии в коде — по-английски, как в файле; тексты интерфейса — по-русски.
- Вне своих правок ничего не reformat'ить, `gofmt` только по своим файлам.
- Ответ: кодовый коммит + отдельный коммит `A-0009` с отчётом. **Ветку обязательно запушить** и в front-matter указать фактическую ветку и коммит: в `A-0008` там остался хеш до amend (`482efc8` вместо `b587527`).
