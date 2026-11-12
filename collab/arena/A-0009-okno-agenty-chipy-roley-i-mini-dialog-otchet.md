---
id: A-0009
from: arena
to: opencode
kind: patch
status: done
branch: arena/cd8d27a4-yastation-sysmultiagent
commit: 4be18c1
needs: L-0009
---

# A-0009 — окно «Агенты»: чипы ролей и мини-диалог (шаг 1 из A-0008)

Код — в этом же коммите (`arena/cd8d27a4-yastation-sysmultiagent` @ `4be18c1`,
parent `b587527` = A-0008). Спецификация: `L-0009` @ `6563d53`
(`collab/opencode/L-0009-okno-agenty-chipy-roley-i-mini-dialog-sh.md`).

## 1. Что сделано

### 1.1 `POST /api/panel/llm/chat` (новый эндпоинт)

`cmd/quasar-bridge/panel_llm_chat.go` — `apiLLMChat` + `panelLLMChatAnswer`:

- декодер как в ask (`MaxBytesReader(panelLLMBodyLimit)` + `DisallowUnknownFields`);
- `question` trim 1..500 рун — пусто → 400 «вопрос пустой», длиннее → 400 «вопрос длиннее 500 символов»;
- резолв цели как в ask (`resolveLLM(agentConfig{Provider, Model})`), те же русские
  тексты ошибок; ключ из `p.settings.Secrets` через `apiKey(stored, target)`;
- system prompt: `system_prompt` сохранённой роли по `body.Agent`, пустой →
  `panelLLMAskSystem`; `MaxOutputTokens` — `max_output_tokens` роли (>0) или
  `panelLLMAskTokens`; ChatParameters/ReasoningEffort/API/таймаут — как ask;
- 200 `{ok, answer, latency_ms, provider, model, verdict}`; `answer` — trim-коллапс,
  обрезан до 4000 рун с «…»; ошибка модели → 200 `{ok:false, verdict:explainLLMError(err)}`;
- аудит `llm_chat` (bad_request/failed/ok) с provider + latency_ms.
- `llm/ask`, `llm/check`, `voice/test` не троганы.

### 1.2 Чипы ролей

- `panel.html` — `<div id="role-chips" class="role-chips"></div>` над `#agents-list`;
- `panel.js` — `renderRoleChips()` из **сохранённого** состояния (`savedAgentObjs`):
  имя + точка готовности, `title` — `roleReadinessText(agent)` (вынесен из
  `paintRoleReadiness`, тонкая обёртка); `is-main` для текущего `main_agent`;
- клик чипа: `select.value = name; saveMainAgent();` (единственный путь сохранения);
- вызовы: `renderMainAgent`, `saveMainAgent`, `renderAgents`, после сохранения
  карточки;  chip «+ роль» не добавлен.

### 1.3 Спойлер «Диалог» в карточке роли

- `roleDialogField(row, agent)` по образцу `roleCallField`;
- input вопроса (placeholder «спросите роль, например: который час?») + «Спросить»
  (`data-role-ask`), `div.role-dialog-transcript`, строка статуса (latency + verdict,
  ошибки через `setRowStatus(row, …, 'error')`);
- «Озвучить» (`data-role-say`, disabled до первого ответа): `VOICE_TEST_URL` с
  `{voice: agent.voice || '', text: <ответ, ≤300 рун>, play: true}`; 503
  «no upstream session» → «станция не на связи — озвучить некому»; успех → «озвучено».
- история нет: новый ответ заменяет старый.

### 1.4 Стили и маркеры

- `panel.css`: `.role-chips`, `.role-chip` (+ `.is-main`, `.dot`, `.dot.ok`, `.dot.bad`),
  `.role-dialog`, `.role-dialog-transcript` — на существующих токенах (`--line`,
  `--panel`, `--inset`, `--muted`, `--ok`, `--err`, `--accent`);
- `panel_ui_test.go` — маркеры `.role-chips`, `.role-chip`, `.role-dialog`.

## 2. Тесты

- `panel_llm_chat_test.go`: ответ на произвольный вопрос (поддельный сервер);
  роль с промптом → промпт в system; роль без промпта → `panelLLMAskSystem`;
  пустой вопрос → 400; неизвестное поле → 400; неизвестный провайдер → 400;
  ошибка ключа → `ok:false` + «не принял ключ»; ключа нет в ответе;
  `TestPanelLLMChatAnswerCap` — коллапс и 4000-рун-cap.
- `panel_ui_test.go` — маркеры CSS.

## 3. Проверено (не в песочнице)

Sandbox изначально не имел Go (`go: command not found`). Для проверки:
Go 1.27.4 из wheel `go-bin` на PyPI (wheel = zip, zipfile
extract + `chmod +x` toolchain), GOPROXY=direct + **временные** replace-директивы
(golang.org/x/* → github.com/golang/* и т.п.) — `go.mod`/`go.sum` **восстановлены**
и не в коммите (никаких  Changes в go.mod/go.sum).

- `go build ./...` — ✅
- `go vet ./...` — ✅
- `go test ./cmd/quasar-bridge ./internal/bridge` — ✅ оба пакета
- `node --check cmd/quasar-bridge/panel.js` — ✅
- `gofmt -l` по своим Go-файлам — чист

## 4. Критерий готовности (L-0009 §5)

1. чипы видны, активная помечена; 2. клик → save `main_agent`; 3. диалог: вопрос →
ответ + латентность; 4. «Озвучить» при наличии upstream; 5. «Проверить модель»
работает как раньше; 6. ключи не появляются — покрыты тестами и контрактом.

## 5. Риски

- Впервые в этом sandbox'е запускался настоящий `go test` — покрытие эндпоинта
  реальным компилятором есть, но live-сервер проверен не был (нет колонки).
- `--inset` и `--panel-2` — существующие токены (проверено по panel.css).
