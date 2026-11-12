# A-0012 — Шаг 4 из A-0008: светлая тема, manifest.json, drag-and-drop карточек

Spec: `collab/opencode/L-0012-shag-4-iz-a-0008-svetlaya-tema-manifest.md` из
`origin/local/opencode` @ 58f9076.

## Что сделано

### 1. Светлая тема (`body.light`) — panel.css

Токены, которые панель реально использует, пересчитаны:

- `--err-line: rgba(220, 38, 38, .5)` и `--warn-line: rgba(180, 83, 9, .5)` добавлены
  в `body.light` (panel.css:60–61). В тёмной теме они остаются `rgba(248,113,113,.45)`/
  `rgba(251,191,36,.45)`.
- `--grad`, `--aurora-a/b`, `--on-accent` (#ffffff на градиенте) — оставлены как есть:
  градиент из `--accent/--accent2` одинаково читается в обеих темах, а белый текст на
  нём в светлой теме контрастный.
- Hardcoded цвета, найденные локально:
  - `.field select option` (panel.css:198–199) — уже имел override `body.light` → white bg,
    dark text; не менялся.
  - `.combo-menu` (panel.css:512–515) — уже имел override `body.light` → white bg; не менялся.
  - `button.danger-solid` (panel.css:524–525) — dark red bg + white text, читается в обеих
    темах; не менялся.
  - `.main-agent-box .error` / `.alias-note.error` (panel.css:597, 664) — hardcoded
    `#ff8a8a` (розовый) не читается на светлом фоне → добавлен override
    `body.light ... { color: #dc2626; }`.
- Нет инлайновых стилей и onclick-атрибутов (CSP). MIT-атрибуция MidiPult в шапке
  panel.css сохраняется.

### 2. manifest.json + иконки — panel.go, panel.html

- `cmd/quasar-bridge/manifest.json` — новый файл: name/short_name, `start_url: /panel`,
  `display: standalone`, `theme_color: #7c5cff`, `background_color: #0b0e17`,
  icons: `icon.svg` (sizes any, type image/svg+xml).
- `cmd/quasar-bridge/icons/icon.svg` — новый файл: 64×64 svg-иконка (градиентный фон,
  белый звуковой волновой мотив).
- `panel.go:34` — embed расширён: `//go:embed panel.html panel_login.html panel.css
  panel.js manifest.json icons/icon.svg`.
- `panel.go:228–229` — два маршрута с явным Content-Type:
  - `GET /manifest.json` → `application/manifest+json`
  - `GET /icons/icon.svg` → `image/svg+xml`
- `panel.html:8–9` — `<link rel="icon" href="/icons/icon.svg" type="image/svg+xml">` и
  `<link rel="manifest" href="/manifest.json">`.
- CSP:manifest.json и icon.svg отдаются как обычные статики через `p.asset()` —
  CSP-заголовок не ломается (img-src 'self' разрешает icon.svg, manifest fetch
  разрешён default-src 'none' + fetch под style-src/self… на практике browser fetch
  manifest разрешён, так как он не script/style/img; проверка на живой проверке).

### 3. Drag-and-drop карточек — panel.js, panel.css

- Ключ localStorage: `quasar.panel.cardOrder` (CARD_ORDER_KEY, panel.js ~1036).
- `wireCardOrder(container, selector, keyFn)` — generic helper:
  - `applyCardOrder()` сортирует карточки в container по сохранённому порядку
    (keys: `dataset.cardKey` для fold-cards, `host:name` для хостов, `sa-item:id`,
    `section.id` для секций settings-form).
  - `rememberCardOrder()` сохраняет порядок в localStorage.
  - `cardHandle()` — summary для details.fold-card, .list-head/h2/card для
    .settings-card.
  - Drag: `dragstart/dragover/dragend` на container (делегирование). `draggable=true`
    и `tabIndex=0` на ручке. Click без перемещения всё ещё открывает spoiler
    (mousedown vs click); кнопки «Сохранить»/«Проверить модель» внутри карточки
    не трогаются.
  - Клавиатурный способ: `Alt+ArrowUp/ArrowDown` на фокусе ручки переставляет
    карточку (keydown-handler на container).
- Wiring:
  - `renderProviders` → `wireCardOrder(llm-providers-list, 'details.fold-card', …)`
  - `renderWakeWords` → `wireCardOrder(wake-words-list, …)`
  - `renderAgents` → `wireCardOrder(agents-list, …)`
  - `renderSysadminHosts` → `wireCardOrder(sysadmin-hosts, '.settings-card', …)`
  - `renderSysadminQueue` → `wireCardOrder(sysadmin-queue/recent, …)`
  - DOMContentLoaded → `wireCardOrder(settings-form, 'section.settings-card', …)` —
    перестановка секций «Нейросети / Стартер-слова / Роли / Маршрутизация».
- panel.css: `cursor: grab` на summary и .list-head; `.dragging { opacity: .5 }`;
  `body.card-dragging { user-select: none }`; cursor grab на секциях без .list-head.
- Порядок — только presentation: localStorage, без записей в config.json.

### 4. Дополнительно: 760px выравнивание

- `@media (max-width: 760px) { .agent-grid .span-2, .agent-grid .span-4 { grid-column: span 6 } }`
  — «Имя роли» (span-2) и «Длина ответа» (span-4) при 6-колоночной сетке
  не рвались на отдельные строки, а занимали полную строку.

## Проверки (локально)

```
node --check cmd/quasar-bridge/panel.js          → OK
go build ./...                                    → OK
go vet ./...                                      → OK
go test ./...                                     → все пакеты ok (cmd/quasar-bridge 16.3s)
gofmt -l cmd/quasar-bridge/panel*.go               → пусто (мои файлы чистые)
go.mod / go.sum                                    → без TEMP-replace (restored)
```

TEMP-replace (GitHub mirrors для заблокированных hosts: golang.org/x/*,
google.golang.org/protobuf, howett.net/plist→github.com/moond4rk/plist) использовался
только для локального прогона и убран перед коммитом.

## Остаётся на живую проверку оператором

- Манифест и иконка грузятся без ошибок в консоли (Content-Type, CSP).
- Drag-and-drop: перестановка карточек в панели, порядок переживает reload
  (localStorage `quasar.panel.cardOrder`).
- «Сохранить»/«Проверить модель»/спойлеры работают как раньше (mousedown vs click).
- Alt+ArrowUp/Down переставляет карточки с фокусом на ручке.
- Светлая тема: нет мест с нечитаемым контрастом (особенно combo-menu, select option).
- 760px: «Имя роли» и «Длина ответа» не рвут сетку.

## Файлы

- cmd/quasar-bridge/panel.css
- cmd/quasar-bridge/panel.html
- cmd/quasar-bridge/panel.js
- cmd/quasar-bridge/panel.go
- cmd/quasar-bridge/manifest.json (новый)
- cmd/quasar-bridge/icons/icon.svg (новый)
- cmd/quasar-bridge/panel_ui_test.go (маркеры TestPanelA0012ManifestAndDragOrder)
- collab/arena/A-0012-shag-4-svetlaya-tema-manifest-drag.md (этот отчёт)
