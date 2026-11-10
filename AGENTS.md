# AGENTS.md — протокол моста opencode ↔ Arena

Этот репозиторий использует мост Arena (skill `arena-bridge`). Чат Arena — только
транспорт для пинга; всё, что должно пережить сессию, живёт в git.

## Роли и ветки

| Сторона | Пишет | Ветка |
| --- | --- | --- |
| opencode | `collab/opencode/L-NNNN-*.md`, `collab/runs/*.json` | `mine/local/opencode` |
| Arena | `collab/arena/A-NNNN-*.md` | `mine/arena/opencode` |

Конфигурация моста — `collab/bridge.json`. Квоты governor — там же, состояние в
`.arena/governor.json` (не коммитится).

## Один круг

1. `arena_round({ title, body, needs })` → файл `L-NNNN` + runlog + пинг.
2. `git add/commit/push mine local/opencode`.
3. Отправить пинг в чат Arena одной строкой.
4. `arena_guard({ kind: "send", task: "L-NNNN", record: true })`.
5. `arena_wait({ id: "A-NNNN", timeout: 900 })` — ждать коммит, не опрашивать браузер.
6. `arena_show({ id: "A-NNNN" })` → чтение ответа; при необходимости новый `L-*`.

## Формат файла L-*

```markdown
---
id: L-0001
from: opencode
to: arena
kind: task
status: open
branch: local/opencode
commit: <sha>
needs: A-0000
---

## Задача
<одна проверяемая цель>

## Контекст
<файлы, строки, ограничения>

## Критерий готовности
<проверяемое утверждение>

## Ограничения
<что нельзя менять>
```

Arena отвечает файлом `A-NNNN` с тем же `id` в `needs`, статусом и разбором.

## Правила

- Одна задача — один проверяемый результат и один коммит `A-*`.
- В чат — только пинг: `[L-NNNN]`, refspec, SHA, путь к файлу, ветка для ответа.
- `collab/opencode/**` правит только opencode, `collab/arena/**` — только Arena.
- Несогласие оформляется новым `L-*`, а не правкой чужого файла.
- Секреты, ключи, `secrets.json`, `runtime.env`, сертификаты — никогда в `collab/`
  и никогда в чат.
- Только текст: без бинарников, ZIP и больших дампов (лимит workspace 128 МБ).
- Ответ Arena проверяется `arena_diff`/`git show`, а поведение — прогоном на машине
  оператора. Источник истины — прогон, не текст модели.

## Квоты

`min_interval_s` 180, `max_per_hour` 4, `max_per_day` 8. Запрет governor
(`allowed: false`) — пауза, а не повод обходить.
