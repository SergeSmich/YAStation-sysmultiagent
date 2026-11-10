---
id: A-0006
from: arena
to: opencode
kind: answer
status: done
branch: arena/9fc2baa0-yastation-sysmultiagent
commit: cf5ee9a
needs: L-0006
---

# A-0006 — реализация `main_agent`: основная модель как указатель на роль

Отчёт по L-0006. Круг — только код и панель: кодовый коммит `cf5ee9a` (без путей
`collab/`), этот файл — отдельным коммитом. Дизайн — A-0005 (§2, §4).
Все ссылки на код — по `arena/9fc2baa0-yastation-sysmultiagent` @ `cf5ee9a`.

## 1. Итог по пунктам задания

| Пункт L-0006 | Что сделано | Где |
| --- | --- | --- |
| 1. `config.go` | поле `MainAgent string \`json:"main_agent"\`` рядом с `llm_*`; `validateMainAgent()`; вызов в общей цепочке `readConfig` после `validateAgents`; `resolveLLM` не менялся | `config.go:146`, `config.go:541`, `config.go:313` |
| 2. `internal/bridge/engine.go` | пара клиентов в `atomic.Pointer[mainClients]`, `SetMain`, аксессоры `llmClient`/`answerer`/`mainPrompt`/`guard`; все чтения `e.LLM` в рабочем коде идут через аксессоры (осталось три — внутри самих аксессоров); `Guard` — корневой клиент | `engine.go:39`, `engine.go:50–115`; `retry.go:72`; `safety.go:35,48` |
| 3. `cmd/quasar-bridge/agents.go` | `buildMainAnswerer` (`resolveLLM` + ключ из `secrets.json`/env + промпт роли + настройки генерации); `assistant.Agent` только при `assistant_tools.enabled`; **без** `sysadmin`/`peers` | `agents.go:94` |
| 4. `main.go` | `Engine{LLM: lc, Guard: lc}`; корневой клиент остаётся для `Guard`, `Home.Planner` и пустого `main_agent`; ошибка `main_agent` на старте — отказ (`os.Exit(1)`); `slog.Info("main_agent_ready", …)`; горячая пересборка в `applySettingsConfig` | `main.go:203`, `main.go:265–283`, `main.go:325` |
| 5. `settings.go` | `settingsPatch.MainAgent *string`, применение и валидация в `applySettingsPatch`, `main_agent` + `main_target` в `settingsSnapshot`, `warnings[]` | `settings.go:33`, `settings.go:180–270`, `settings.go:98` |
| 6. Панель | карточка «Основная модель» первой в «Агенты → Роли», подсказка в «Маршрутизации», чип «модель от другого провайдера» | `panel.html:66`, `panel.js:1299–1425`, `panel.js:1803` |
| 7. Тесты | новые `mainagent_test.go`, `mainmodel_test.go`, дополнения в `panel_ui_test.go` | см. §5 |
| 8. Документация | раздел «Основная модель» в руководстве, абзац про маршрут `llm` в решениях | `docs/guide.md`, `docs/design.md` |

## 2. Как это работает

`main_agent` не новая сущность, а указатель: пустая строка — клиент `llm_*`
(поведение прежнее, байт в байт), имя роли — её нейросеть, модель, ключ,
параметры генерации и её системный промпт.

Пара «думает/говорит» лежит в неизменяемой структуре:

- `LLM` — аксессор `llmClient()`: классификатор `default_route: "classify"` и
  повтор предыдущего вопроса. Инструкция классификатора своя и не заменяется
  промптом роли, поэтому меньшая модель не переписывает политику маршрутизации;
- `answerer()` — кто говорит с человеком: пара `main`, затем `Assistant`, затем
  `LLM`;
- `mainPrompt()` — промпт роли, пусто — общий промпт ассистента;
- `guard()` — проверка недоверенных данных: `Guard`, иначе `LLM`. Роль не
  участвует: проверка не должна слабеть вместе с моделью, которая отвечает.

`SetMain(llm, answerer, prompt)` публикует всю тройку атомарно, поэтому
сохранение из панели и ход в полёте видят либо старую пару, либо новую, но не
смесь.

Инструменты: `sysadmin` и `peers` — не наследуются, это инвариант (маршрут `llm`
ловит всё, что не совпало с правилами: отвечать он может, действовать нет).
Инструменты `assistant_tools` — по-прежнему выключатель установки: включены →
клиент роли оборачивается в `assistant.Agent` с тем же `tools`.

Ошибки не прячутся: неизвестная роль или роль без модели — `400` при сохранении
и отказ загрузить `config.json` при старте. Тихого отката на `llm_*` нет.

## 3. Порядок горячего применения

`applySettingsConfig` сначала собирает **оба** результата (`buildMainAnswerer`,
`buildAgents`), и только потом публикует: `engine.SetMain(...)`, затем
`engine.SetAgents(...)`. Отказ любой из сборок оставляет живыми прежние
ответчик и реестр ролей — правка роли, которая используется как основная, не
может оставить мост в половинчатом состоянии. `applyWake` и `SetRouting`
по-прежнему после.

## 4. Панель

- «Агенты → Роли», первая карточка: список «Как `llm_*` (по умолчанию)» + роли;
  под списком — что реально ответит: `нейросеть: … · модель: … · ключ: есть/нет ·
  промпт: роли/общий` (для роли сисадмина добавляется «инструменты не
  наследуются»);
- сохранение — отдельный частичный патч `{"main_agent": "…"}`, поэтому
  несохранённые правки карточек ролей не теряются;
- «Проверить модель» — существующий `POST /api/panel/llm/check` (адрес и API
  берутся из сохранённой нейросети, ключ — из хранилища). Если роль наследует
  `llm_*`, адреса в панели нет: карточка честно говорит, где проверять, вместо
  невнятной ошибки;
- «Маршрутизация»: строка «Маршрут «нейросеть» отвечает: роль … / клиент `llm_*`»;
- карточка роли: чип «модель роли X ≠ модель нейросети Y» + кнопка «взять модель
  нейросети». Кнопка меняет только поле в форме (сохраняется вместе с
  карточкой); сама панель ничего не перезаписывает;
- ключи в `GET` не отдаются: `main_target` содержит только `key_set: bool`.

## 5. Тесты

Новые (`cmd/quasar-bridge/mainagent_test.go`):

- `TestValidateMainAgent` — пусто, известная роль, неизвестная роль, роль без
  модели, имя в верхнем регистре, пробелы вокруг имени;
- `TestReadConfigRejectsUnknownMainAgent` — отказ загрузки конфигурации;
- `TestBuildMainAnswererKeepsTheDeploymentClient` — пустой `main_agent` оставляет
  корневую пару;
- `TestBuildMainAnswererUsesTheRole` — клиент роли, промпт роли, обёртка в
  `assistant.Agent` с тем же `tools`;
- `TestBuildMainAnswererNeverBringsRoleTools` — роль с `sysadmin: true, peers:
  true` даёт только модель и промпт, инструментов нет;
- `TestBuildMainAnswererRejectsUnknownRole`;
- `TestMainAgentSettingsRoundTrip` — запись файла, живое применение
  (`settingsDeps.Apply`), `main_agent` и `main_target` в снимке, `key_set` —
  булево;
- `TestMainAgentClears` — пустая строка возвращает `llm_*`;
- `TestMainAgentUnknownRoleIsRejected` — `400`, файл и живой мост не тронуты;
- `TestMainTargetViewNeverCarriesTheKey` — значение ключа не попадает ни в одно
  поле снимка;
- `TestSettingsWarningsOnProviderModelMismatch` — предупреждение
  `agent`/`model`/`provider_model` и его отсутствие при совпадении.

Новые (`internal/bridge/mainmodel_test.go`):

- `TestSetMainSwitchesAnswerer` — пустой `main_agent` → корневой клиент, роль →
  клиент роли, промпт роли доходит до модели и сохраняет голосовое ограничение;
- `TestClassifierUsesMainClientWithItsOwnPrompt` — классифицирует клиент роли, но
  со системной инструкцией классификатора; корневой клиент не вызывается;
- `TestGuardStaysOnTheDeploymentClient` / `TestGuardFallsBackToTheDeploymentClient`
  — `question_safety` не уходит в роль;
- `TestSetMainIgnoresNil`;
- `TestSetMainIsSafeUnderLoad` — гонка `SetMain` с `Fallback`/`llmClient`/
  `mainPrompt` (для прогона под `-race`).

Правка существующего: `panel_ui_test.go` — в список требуемых классов CSS
добавлены `.main-agent-box` и `.model-mismatch` (ожидания остальных тестов не
менялись).

## 6. Что не прогонялось

В песочнице **нет Go 1.27.3** (и никакого другого тулчейна: `go`, `gofmt`
отсутствуют), поэтому `go build`, `go vet`, `go test` и `go test -race` не
запускались. Что удалось проверить:

- синтаксический разбор всех изменённых и новых `.go` файлов парсером Go
  (tree-sitter) — ошибок и пропущенных узлов нет;
- `node --check cmd/quasar-bridge/panel.js` — синтаксис панели в порядке;
- проверка неиспользуемых импортов в изменённых файлах — новых нет;
- `scripts/check-public-tree.py`: три срабатывания в `panel.html`/`panel.js` —
  это прежние фикстуры (адрес и MAC в подсказках), сдвинутые моими вставками;
  новых нет. Остальные срабатывания скрипта есть и на `dca78e5` (тестовые
  фикстуры), к этому кругу не относятся.

Оператору нужно прогнать:

```
go build ./...
go vet ./...
gofmt -l cmd internal            # ожидается пустой вывод
go test ./internal/bridge ./internal/sysadmin ./cmd/quasar-bridge
go test -race ./internal/bridge -run 'TestSetMain|TestClassifier|TestGuard'
```

Ожидания старых тестов не менялись: при пустом `main_agent` маршрут `llm`
отвечает тем же клиентом, что и раньше.

## 7. Совместимость и откат

- Старый `config.json` без `main_agent` читается как раньше, поле не пишется в
  файл, пока его не меняют из панели (`writeConfigMembers` пишет только
  присланные секции).
- `main_agent` попадает в `config.json` только при сохранении из панели;
  ключи — по-прежнему в `secrets.json` и `runtime.env`.
- Три роли не пересоздавались, их поведение по умолчанию не менялось.
- Откат: убрать `main_agent` (панель или файл) — маршрут `llm` немедленно
  возвращается к `llm_*`; полностью — откатить коммит `cf5ee9a`.

## 8. Границы круга

`role_call`, `greeting`, `callable`, алиасы и резолвер вызова — не в этом круге
(L-0007). PowerShell-механизм обновления не тронут, деплой не выполнялся.

## 9. Вопросы к opencode

1. Проверка основной модели, когда роль наследует `llm_*`: сейчас панель не
   знает адрес корневого клиента и отправляет оператора в «Нейросети». Если
   хотите проверку прямо из карточки — нужно решить, отдавать ли `llm_base_url`
   в снимке настроек (это адрес, не ключ).
2. `slog`-события: `main_agent_ready` (старт, `role` = `llm_*` или имя роли),
   `agent_model_mismatch` (после сохранения), `settings_applied` теперь
   содержит `main_agent`. Если нужен другой набор полей для разбора логов —
   скажите в L-0007.
