# A-0013 — Шаг 6 из A-0008: сценарии по расписанию — планировщик и карточка в панели

Spec: `collab/opencode/L-0013-shag-6-iz-a-0008-scenarii-po-raspisaniyu.md` из
`origin/local/opencode` @ a84d9ae.

## Что сделано

### 1. Хранилище — `cmd/quasar-bridge/scenarios.go`

- `scenarios.json` рядом с `config.json` (каталог `filepath.Dir(configPath)`), режим
  0600, запись только через `writeFileAtomic` (tmp + fsync + rename,
  `cmd/quasar-bridge/voiceconfig.go:183`). `config.json` не расширяется.
- Схема exactly по spec: `{"scenarios":[{"id","enabled","time","days","action","last_fired"}]}`,
  `days` — ISO 1..7 (1 = понедельник), `action` — `{"type":"sound","source":
  "notification|success|error|timer"}` или `{"type":"volume","percent":0..100}`.
  `id` генерирует сервер (`s-N`, `assignScenarioIDs`).
- `scenarioStore` — тонкая wrapper-структура (path + timezone + mutex), методы
  `load/save/saveValidated/setLastFired`; `setLastFired` перечитывает список и
  обновляет только проfired-записи, чтобы не потерять правки панели между
  тиками.
- Валидация `validateScenarios` — ровно по spec: `HH:MM` (24h, регулярка
  `^([01][0-9]|2[0-3]):([0-5][0-9])$`), непустой набор дней 1..7, `percent` 0..100,
  тип действия из двух; все сообщения — по-русски, они же уходят в панель.
- Пустой список валиден (хранится как `{"scenarios":[]}`).

### 2. Планировщик — `scenarioScheduler` (scenarios.go)

- Одна горутина, тик 25 с (в пределах 20–30 с из spec), контекст мостика —
  выходит на остановку.
- Время в зоне `device_context.timezone` (дефолт `Europe/Moscow`);
  `time/tzdata` embed — `LoadLocation` работает и на станции без системной
  базы поясов. Неизвестная зона → предупреждение в журнал, выполнение в UTC,
  процесс не падает.
- Сценарий срабатывает: только в свою минуту, только в свои дни, не более
  одного раза в минуту (`last_fired` минутной точности), и не повторяется
  после рестарта в ту же минуту.
- Действия вызываются вне блокировки списка: `tick` сначала соберёт due-список,
  потом проиграет звуки/громкости, потом запишет `last_fired`.
- `sound`-сценарий при `assistant_tools.enabled=false` или без живого сеанса
  голоса → предупреждение в журнал (`scenario sound skipped`), процесс жив;
  следующую минуту/день он пробует снова. `volume` работает независимо.

### 3. Звук вне разговора — `internal/bridge/sound.go` (добавлением, без правок ветки)

- Новый метод `(*Native).PushSound(ctx, sound.Request) error`: внутри разговора
  директив звука идёт с финальным ответом (`queueSound`), а у расписания нет
  speaker request — звук уходит как проактивный **push** (`TServerMessage_TEvent_Push`
  с теми же директивами `makeSoundDirective`) прямо на сеанс станции (`n.down`).
  Прибор не может отличить его от проактивного сообщения облака.
- Решено добавить метод в `sound.go` вместо дублирования ~40 строк протокола в
  `cmd/`: это purely additive — существующие пути (`queueSound`, `reply`)
  не тронуты, `routing.go`, voice-ветка и `role_call` не задеты.
- `scenarioSoundPlayer(live, soundEnabled)` — закрывает звук в scheduler на
  `bridge.PushSound`; без сессии/инструментов сообщает причину по-русски.

### 4. API — `panel.go` + `scenarios.go`

- Маршруты в `panel.go:255-256`:
  - `GET /api/panel/scenarios` (requireSession) → `{"timezone":"...",
    "scenarios":[...]}` — ровно два ключа, ничего лишнего.
  - `POST /api/panel/scenarios` (requireMutation) → принимает полный список,
    валидирует как одну сущность (`saveValidated`), при отказе 400 с по-русски
    ошибкой и файл не трогается; при окне отвечает сохранённым списком (с
    серверными id).
- `panelServer.scenarios` и `panelDeps.Scenarios` — один и тот же `*scenarioStore`
  насовмо для панели и планировщика: один файл, один mutex, нет гонки
  «сохранение vs тик».

### 5. Панель — `panel.html`, `panel.js`, `panel.css`

- Карточка «Сценарии по расписанию» во вкладке «Станция» (`section-scenarios`):
  `list-head` с ручкой и «+ сценарий», подпись с зоной времени
  (`#scenario-timezone`), список строк, статус строки.
- Строка сценария: «Включён» (чекбокс, span-2), «Время» (input type=time,
  span-3), «Дни» (7 чекбоксов Пн–Вс, span-4), «Действие» (звук/громкость, span-3),
  зависящее поле (source-select для звука / range+число для громкости, span-3),
  «Сохранить»/«Удалить»/статус строки. 12-колоночная сетка (`agent-grid` +
  `span-*`), никаких инлайн-стилей и `onclick` (CSP).
- Сохранение по карточке: POST-ится **весь** список с этой строкой заменённой/
  добавленной; после успеха список перечитывается (ответ сервера), карточки
  перерисованы; «+»-форма сворачивается, сохранённая раскрывается с «сохранено».
  «Удалить» — с confirm, файл пишется сразу.
- Карточка участвует в существующем порядке: каждая строка — `details.fold-card`
  с `data-card-key`, `wireCardOrder` на контейнер; summary показывает
  время · дни · действие (`cardSummaryText` + `scenarioActionLabel`).
- Сохранение панели (Enter, кнопка «refresh») не перечитывает сценарии:
  список грузится один раз при загрузке страницы и после каждого сохранения
  (как старт-слова), чтобы 5-секундный `refreshAll` не ломал редактирование.
- Ошибки пользователю — по-русски (от сервера и из JS-валидации: «время в
  формате ЧЧ:ММ», «выберите хотя бы один день недели»).

### 6. Тесты — `cmd/quasar-bridge/scenarios_test.go` + `panel_ui_test.go`

(а) `TestScenarioTickFiresOncePerMinuteAndOnItsDays` — срабатывает ровно один
раз в минуту, только в свои дни, не повторяется после рестарта в ту же минуту,
`last_fired` минутная точность.
(б) `TestAPIScenariosRejectsBadLists` — биты `time`/`days`/`percent`/`source`/
`type` → 400 + по-русски ошибка + файл побайтово не изменился.
(в) `TestScenarioFileWrittenWithMode0600` — режим 0600.
(г) `TestAPIScenariosAcceptsEmptyList` — пустой список валиден.
(д) `TestAPIScenariosGetLeaksNothing` — GET отвечает ровно `timezone`+`scenarios`.
Дополнительно: `TestScenarioTickSkipsDisabledAndRespectsVolume`,
`TestScenarioTickKeepsTickingAfterAFailedAction`, `TestScenarioSoundPlayer`,
`TestValidateScenariosMessages`, `TestAPIScenariosRoundTrip`,
`TestPanelA0013ScenarioCard` (маркеры UI + CSP-чистоту markup).

## Что проверено

```
go build ./...                              → OK
go vet ./...                                → OK
go test ./cmd/quasar-bridge ./internal/bridge -count=1
                                            → ok (16.9s) / ok (5.7s), все тесты
node --check cmd/quasar-bridge/panel.js     → OK
gofmt -l (мои файлы)                        → пусто
go.mod / go.sum                             → restored, без TEMP-replace
```

TEMP-replace (GitHub-mirrorы для заблокированных hosts: `golang.org/x/*`,
`google.golang.org/protobuf`, `howett.net/plist` → `DHowett/go-plist v1.0.1`,
`gopkg.in/yaml.v2/v3`, `gopkg.in/check.v1` → локальные checkout) использовался
только для локального прогона и убран перед коммитом. Голосовая маршрутизация
не менялась: «поставь будильник» по-прежнему уходит в Яндекс (`routing.go` не
трогано).

## Остаётся на живую проверку оператором

- На станции: добавить сценарий «ближайшая минута, сегодня, звук `timer`» →
  сохраняется, через минуту сигнал слышен на колонке, `last_fired` обновился в
  `scenarios.json`; удалить → запись исчезла.
- Звук действительно доходит как push (прибор проигрывает stored sound без
  активного разговора) — проверить на реальном устройстве.
- Громкостной сценарий меняет `Master` на станции в заданную минуту.
- Перезапуск моста в ту же минуту не дублирует сигнал.

## Файлы

- cmd/quasar-bridge/scenarios.go (новый: store + scheduler + validation + API-хендлеры)
- cmd/quasar-bridge/scenarios_test.go (новый: тесты (а)–(д) + остальные)
- cmd/quasar-bridge/panel.go (поле, deps, маршруты)
- cmd/quasar-bridge/panel.html / panel.js / panel.css (карточка «Сценарии»)
- cmd/quasar-bridge/panel_ui_test.go (маркеры TestPanelA0013ScenarioCard)
- cmd/quasar-bridge/main.go (store + goroutine планировщика)
- internal/bridge/sound.go (добавлением: PushSound)
- collab/arena/A-0013-scenarii-po-raspisaniyu-otchet.md (этот отчёт)
