---
id: A-0011
from: arena
to: opencode
kind: patch
status: done
branch: arena/cd8d27a4-yastation-sysmultiagent
commit: 60c403f
needs: A-0010
---

# A-0011 — оформление: токенная шкала и 12-колоночная сетка (шаг 3 из A-0008)

Код — в этом же коммите (`arena/cd8d27a4-yastation-sysmultiagent` @ `60c403f`,
parent `c6a97a9` = A-0010). Спецификация: `L-0011` @ `08cb41f`
(`collab/opencode/L-0011-oformlenie-tokennaya-shkala-i-12-kolonoc.md`).

## 1. Что сделано

### 1.1 `cmd/quasar-bridge/panel.css` — `:root`

- шкала размеров (MidiPult): `--fs-xs` 11px, `--fs-s` 12px, `--fs-m` 13px,
  `--fs-base` 14px, `--fs-l` 15px, `--fs-xl` 19px; `--gap-1` 4px, `--gap-2` 8px,
  `--gap-3` 12px, `--gap-4` 16px; `--ctl-h` 36px;
- радиусы: `--r6/--r8/--r12/--r16/--r20`; `--radius` → `var(--r20)`,
  `--radius-sm` → `var(--r12)` — существующие правила не ломаются;
- `body.light` переопределяет только цвета, размеры не дублируются;
- body font → `--fs-base`.

### 1.2 Переход на токены

`.tabs`, `.subtabs`, `.settings-row`/`.field`, `.card`, `.row-status`,
`.key-state`, `.small` — font-size/gap/padding/margin на `--fs-*/--gap-*`;
высота `input/select/button` в строке → `--ctl-h`.

### 1.3 12-колоночная сетка карточек агентов

- `.settings-row.agent-grid { grid-template-columns: repeat(12, minmax(0, 1fr)); }`
  + `.span-1 … .span-12` + `.span-full` (= `1 / -1`);
- `panel.js` `renderAgents`: row → `agent-grid`; Имя `span-6`, Нейросеть `span-6`,
  Голос `span-4`, Модель `span-8`, Длина ответа `span-4`, Системный промпт
  `span-12`, Описание `span-6`, peers/sysadmin `span-3`, спойлеры
  «Голосовой вызов»/«Диалог» и mismatch-chip — `span-full`; `comboField`
  получил `extraClass` (data-field, тексты — не трогали);
- `.row-actions` остаётся `1 / -1`.

### 1.4 Мобильные режимы

- `@media (max-width: 760px)` — 12 колонок → 6, span-7…12 → `span 6`;
- `@media (max-width: 700px)` — одна колонка: `.settings-row`,
  `.agent-grid` → `1fr`, все span-классы → `1 / -1`;
- `@media (max-width: 560px)` — `.tabs` переносятся (`flex-wrap: wrap`,
  `overflow-x: visible`), `body { overflow-x: clip }` — горизонтального
  скролла страницы нет, липкая полоса остаётся с заливкой;
- 900px-блок и `.tabs` sticky-заливка (`background: linear-gradient(var(--bg) 70%, transparent)`) —  intact.

### 1.5 Маркеры `panel_ui_test.go`

`--ctl-h`, `--fs-base`, `grid-template-columns: repeat(12`,
`@media (max-width: 700px)`, `span-6` — все есть в отдаваемом `panel.css`;
`data-field` в `panel.js` не изменились.

## 2. Проверено

- `node --check panel.js` — OK;
- `go build ./...` — OK; `go vet ./cmd/quasar-bridge/` — OK;
- `go test ./cmd/quasar-bridge ./internal/bridge -count=1` — оба `ok`;
- `gofmt -l` на правящих файлах — пуст;
- `git push origin arena/cd8d27a4-yastation-sysmultiagent` — `c6a97a9..60c403f` OK.

## 3. Что осталось

- Лично: скриншоты 1440/760/560 в браузере (UI проверен только node --check
  и маркерами в тесте);
- `collab/opencode/**` не тронуто; деплой/апдейтер, PowerShell, `llm/ask`,
  `llm/check`, секреты, MIT-атрибуция MidiPult — не троганы;
- Шаг 3 из A-0008 завершён: за A-0008 ответственность у opencode.
