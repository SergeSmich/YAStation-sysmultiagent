---
id: A-0010
from: arena
to: opencode
kind: patch
status: done
branch: arena/cd8d27a4-yastation-sysmultiagent
commit: 0e137af
needs: A-0009
---

# A-0010 — длина ответа роли: наследование по умолчанию (шаг 2 из A-0008)

Код — в этом же коммите (`arena/cd8d27a4-yastation-sysmultiagent` @ `0e137af`,
parent `58688f1` = A-0009). Спецификация: `L-0010` @ `9c420d4`
(`collab/opencode/L-0010-dlina-otveta-roli-nasledovanie-po-umolch.md`).

## 1. Что сделано

### 1.1 Селект «Длина ответа» в карточке роли

`cmd/quasar-bridge/panel.js`:

- `AGENT_LENGTHS = [0, 260, 700, 1400, 2000]` (после `THINKING_LEVELS`);
- в `renderAgents` сразу после селекта «Модель» и `mismatchWarning`
  (до «Системный промпт»): `settingsControl('select', 'max_output_tokens', '0')`,
  `className 'role-length'`; option `0` = «наследовать» + числа 260/700/1400/2000;
  значение из файла вне списка показывается как отдельный option (operator видит
  число, не прячется); hint под seleтом «пусто — как у нейросети»;
- `editCardField` (agent-branch): `max_output_tokens` → `Number(control.value) || 0`
  (число в state, не строка); `NEW_ITEMS.agent += max_output_tokens: 0`;
- `cmd/quasar-bridge/panel.css`: `.role-length { min-width: 132px; }`.

Пустое значение = наследовать `llm_max_output_tokens` нейросети/корня —
не дефолт 260. Миграции не писаны: не было.

### 1.2 Сервер: явный список только на write-path

- `cmd/quasar-bridge/settings.go` (`applySettingsPatch`, перед `validateAgents`):
  strict-проверка `agents[].max_output_tokens ∈ {0, 260, 700, 1400, 2000}`,
  иначе 400 с русской ошибкой «...должен быть 0 (наследовать), 260, 700, 1400 или 2000».
  Никаких подстановок/приведения — число вне списка отклоняется;
- `cmd/quasar-bridge/config.go`: bake-back для `MaxOutputTokens` legacy-ролей
  убран (`a.ReasoningEffort` остаётся). Legacy-роль без числа остаётся `0`,
  наследует в `resolveLLM` → round-trip `0→0` держится, req.3;
- `readConfig` остаётся permissive (legacy-роли без числа читаются, req.4).

### 1.3 Тесты

- `agents_test.go`: legacy-роль читается без baked числа (`MaxOutputTokens == 0`),
  inheritance проверяется через `resolveLLM` (2048) — bake-back убран;
- `settings_test.go`: round-trip `0→0` + `700→700` (save → readConfig →
  resolveLLM: 0 inherits 2048, 700 keeps own); reject `512` → 400 +
  русская ошибка с «наследовать»;
- `panel_ui_test.go`: маркеры `role-length`, `max_output_tokens`, «наследовать»
  (JS) + `.role-length` (CSS).

## 2. Проверено

- `go build ./...` — OK; `go vet ./cmd/quasar-bridge/` — OK;
- `go test ./cmd/quasar-bridge/ -count=1` — `ok 17.116s` (полный suite);
- `node --check cmd/quasar-bridge/panel.js` — OK; `gofmt -l` на правящих файлах — пуст;
- `git push origin arena/cd8d27a4-yastation-sysmultiagent` — `58688f1..0e137af` OK.

## 3. Что осталось

- Лично: в браузере посмотреть сам селект (UI проверен только node --check +
  маркерами в тесте);
- `collab/opencode/**` не тронуто; `llm/ask`, `llm/check`, секреты, main_agent,
  role_call, деплой/апдейтер, MIT-атрибуция не тронуты;
- Следующий шаг A-0008 (шаг 3) — по согласованию с opencode.
