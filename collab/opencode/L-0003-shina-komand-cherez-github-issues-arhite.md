---
id: L-0003
title: Шина команд через GitHub Issues: архитектура, watcher, протокол, откат
from: opencode
to: arena
kind: task
status: open
branch: local/opencode
needs: A-0002
---

## Задача

Спроектировать и реализовать **дополнительный** канал управления локальной машиной
через GitHub Issues. Arena ставит команду issue с меткой `oc:run`, локальный watcher
исполняет разрешённое действие и пишет результат комментарием.

Текущий git-мост (`collab/`, `L-*`, `A-*`, плагин `arena-bridge`) **не менять**: он
остаётся рабочим и является путём отката.

## Архитектура (оформить в `docs/arena-command-bus.md`)

- Роли: Arena — постановщик, watcher — исполнитель на Windows, оператор — арбитр.
- Поток: issue → claim → execute → comment → close.
- Метки: `oc:run` (готова), `oc:running` (взята), `oc:done`, `oc:failed`,
  `oc:paused` (опционально).
- Идемпотентность: claim через метку; повторно не выполнять. «Зависшая»
  `oc:running` старше N минут → `oc:failed` с предупреждением, без автоперезапуска.
- Доверие: `author_association` ∈ {OWNER, MEMBER, COLLABORATOR} и `author.login`
  ∈ allowlist. Тело фиксируется (sha256) при claim; при несовпадении — отказ.
- Публичность: issues публичны → в командах и результатах нет секретов, токенов,
  `secrets.json`, ключей, полного env. Результат обрезается и редактируется regex.

## Команды v1 (только whitelist)

JSON в теле issue в fenced-блоке:

```json
{"id":"CMD-0001","action":"git_status","args":{},"timeout_s":120,"note":"..."}
```

Разрешённые действия:

- `git_status` — ветка, ahead/behind, изменённые файлы.
- `git_log` — последние N коммитов (N ≤ 30).
- `run_tests` — `go test` по пакетам из allowlist/регэкспа.
- `build` — сборка без деплоя.
- `read_log` — файл из разрешённых каталогов (`logs/`), максимум 200 строк.
- `grep` — поиск по репозиторию: regex + glob, лимит совпадений.
- `deploy_check` — read-only проверка доступности станции/healthz, без изменений.

Никакого произвольного `exec`, `push`, `deploy`, записи файлов и правки конфигов в v1.

## Реализация

- `scripts/arena_watcher.py` — Python 3.11, stdlib + `gh` + `git`, без внешних
  зависимостей.
- `scripts/arena_watcher.example.json` — конфиг: repo, allowed_logins, workdir,
  poll_s, таймауты, allowlist путей и пакетов.
- Флаги: `--once`, `--dry-run`, `--init-labels`, `--verbose`.
- Lock-файл (один экземпляр), `.arena/paused` — стоп.
- Лог `.arena/watcher.log` (`/.arena/` уже в `.gitignore`).
- Все действия — через фиксированные шаблоны команд. Аргументы валидируются,
  никакой склейки shell-строк из тела issue.
- Windows: UTF-8, пути, `gh.exe`/`git.exe` из конфига или PATH.

## Документация и откат

- `docs/arena-command-bus.md`: протокол, схема, безопасность, примеры issue,
  как читать результат, ограничения.
- `AGENTS.md`: короткий раздел «Commands via Issues» + ссылка на docs, без
  переписывания существующего протокола.
- Откат: watcher не запущен → поведение ровно как сейчас; git-мост не тронут;
  достаточно остановить процесс (и при желании удалить метки). Описать в docs.
- Не менять `collab/bridge.json`, плагин `arena-bridge`, Go-код, установщик,
  `control.sh`.

## Тесты

- `scripts/test_arena_watcher.py` (unittest): разбор JSON, отказ при неизвестном
  action и плохом авторе, дедуп по claim, редакция секретов, обрезка, разбор
  меток, dry-run.
- Прогон: `python -m unittest discover -s scripts -p "test_arena_watcher.py"`.
- **Реальные issues и метки в GitHub не создавать** — проверка на моках и
  фикстурах. Живой круг сделает opencode после merge.

## Критерий готовности

A-0003 содержит:

1. Ссылку на коммит(ы) в `arena/9fc2baa0-yastation-sysmultiagent`.
2. Список файлов и краткое обоснование решений.
3. Результат тестов: команда и вывод; честно — что не прогонялось.
4. Раздел «как включить» и «как откатить».
5. Подтверждение, что в diff **именно коммита A-0003** нет `cmd/`, `internal/`,
   `collab/` (изменения D1–D4 из A-0002 не считаются).

## Ограничения

- Только новые файлы + минимальная правка `AGENTS.md`. Go-код, `collab/`, `cmd/`,
  `internal/` не трогать.
- Issues и метки в GitHub не создавать.
- Секреты не запрашивать и не вставлять; в примерах — заглушки.
- Если не успеваешь — зафиксируй состояние и что осталось, не бросай полурабочий
  файл без пометки.
