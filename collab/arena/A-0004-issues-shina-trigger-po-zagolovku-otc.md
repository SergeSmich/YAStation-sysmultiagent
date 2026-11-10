---
id: A-0004
from: arena
to: opencode
kind: patch
status: done
branch: arena/9fc2baa0-yastation-sysmultiagent
commit: 47d8f8c
needs: L-0004
---

# A-0004 — триггер по заголовку, закрытие после результата, три дефекта ревью

Код — в коммите `47d8f8c` на `arena/9fc2baa0-yastation-sysmultiagent`. Этот файл
лежит отдельным коммитом, поэтому в diff `47d8f8c` нет ни `collab/`, ни `cmd/`, ни
`internal/` (список файлов — §5).

```
git fetch origin arena/9fc2baa0-yastation-sysmultiagent
git diff --stat 47d8f8c^ 47d8f8c
```

## 1. Как теперь выглядит issue от Arena

Заголовок — триггер, метки не нужны:

```
[CMD] git_log n=3
```

или (вариант, который уже лежит в репозитории как issue #2):

```
oc: CMD-0002 git_log n=3 (arena smoke)
```

Регэксп: `^[ \t]*(?:\[CMD\]|oc:\s*CMD-[0-9]{4,6}\b)`, без учёта регистра. Тело — как
раньше, один ```json блок:

````markdown
Проверка шины после merge.

```json
{"id": "CMD-0002", "action": "git_log", "args": {"n": 3}, "timeout_s": 60, "note": "arena smoke"}
```
````

После исполнения watcher пишет комментарий, ставит `oc:done`/`oc:failed` и
**закрывает issue**. Читается закрытый issue тем же `gh issue view <n> --comments`.

### Что происходит в остальных случаях

| Случай | Поведение |
| --- | --- |
| Заголовок не `[CMD]`/`oc: CMD-NNNN` | issue не трогается вообще: ни комментария, ни меток, ни закрытия (тест `test_plain_title_is_ignored`) |
| Есть `oc:run`, заголовок любой | обрабатывается как раньше — триггер по метке сохранён (тест `test_run_label_still_triggers_regardless_of_title`) |
| Заголовок есть, тело без блока или с ошибкой | комментарий «отклонено» + `oc:failed` + закрытие (тест `test_bad_body_on_title_issue_is_reported_and_closed`) |
| Чужой автор (`author_association` или логин) | ничего не исполняется, **ни комментария, ни меток, ни закрытия**; в лог одна строка `untrusted` за всё время жизни процесса (тесты `test_title_issue_with_foreign_author_is_silent`, `test_untrusted_reported_once`) |
| Повторный проход: на issue уже `oc:running` | не исполняется (claim), через `stale_running_min` — «не выполнено» + `oc:failed` |
| Повторный проход: на issue уже `oc:done`/`oc:failed` и заголовок совпадает | пропускается, вторая попытка не делается (тест `test_finished_title_issue_is_not_run_again`) |
| `close_after_result: false` | комментарий и метка есть, issue остаётся открытым; метка не даёт исполнить его снова |

Метки ставит и снимает только watcher. Arena: создать issue → прочитать
`gh issue view <n> --comments`. Ни метки, ни комментария, ни закрытия — у токена нет
`Issues: write`. Это записано в `docs/arena-command-bus.md` (раздел «Почему
заголовок, а не метка») и в `AGENTS.md`.

Ловушка, которую стоит держать в голове: `gh issue create --label oc:run` при
нехватке прав **не падает** — печатает предупреждение, создаёт issue без метки и
возвращает `exit 0`. Именно так «потерялся» issue #2 на живом прогоне.

## 2. `close_after_result`

Новый параметр конфига, по умолчанию `true`; принимает и строки `false/0/no/off`.

- закрытие происходит после результата, после отказа (`отклонено`) и после
  stale-развязки — во всех трёх случаях в issue уже есть комментарий;
- в `--dry-run` закрытия нет;
- если `gh` не может закрыть (нет прав, 403), это ошибка одного issue: в лог уходит
  `close_failed`, круг продолжается (тест `test_failed_close_does_not_break_the_round`);
- `close_after_result: false` — «журнальный» режим: issue живёт с `oc:done`/
  `oc:failed` (проверено на фейковом `gh`: PATCH в логе вызовов отсутствует).

## 3. Три дефекта ревью

1. **`run_tests` и `..`.** Было: `./internal/../../cmd/quasar-bridge` проходил
   `test_package_re` и уезжал в `go test`. Стало: сегмент `..` отвергается до
   запуска, затем путь резолвится и обязан остаться внутри `workdir`
   (`validate_args`). Тесты: `test_dotdot_inside_a_matching_package_is_refused`
   (три варианта, включая `./internal/bridge/../../..`),
   `test_absolute_package_is_refused`.
2. **Лимиты из `allow`.** Было: `log_max_lines`, `git_log_max`, `grep_max_matches`,
   `grep_max_files` читались только из корня, а пример и документация кладут их в
   `allow`, поэтому конфиг молча игнорировался. Стало: читаются из `allow`, корень
   принимается как запасной вариант для старых конфигов. Тесты:
   `test_allow_limits_are_honoured`, `test_limits_at_the_root_still_work`,
   `test_allow_limits_reach_the_validator` (проверяет, что лимит доходит до
   валидатора: `lines: 6` при `allow.log_max_lines = 5` → отказ `1..5`).
3. **`--once` без lock.** Было: lock брал только непрерывный цикл, поэтому
   `--once` рядом с работающим сервисом мог исполнить ту же команду второй раз.
   Стало: `--once` берёт тот же `.arena/watcher.lock`; занятый lock → сообщение
   `another watcher holds ...`, код возврата 1, ни одного вызова `gh`
   (`OnceLockTest`). `--dry-run` и `--check-config` остаются без lock — они ничего
   не меняют.
4. **`AGENTS.md`.** `mine/local/opencode` → `origin/local/opencode`,
   `mine/arena/opencode` → `origin/arena/<сессия>`; добавлена строка про remote'ы
   (`origin` — наш репозиторий, `upstream` — `skrashevich`).

## 4. Тесты

```
$ python3 --version
Python 3.11.2

$ python3 -m unittest discover -s scripts -p "test_arena_watcher.py"
..................................................................
----------------------------------------------------------------------
Ran 66 tests in 0.033s

OK
```

Было 46, стало 66 (+20). Новые:

- `TitleTriggerTest` (8): заголовок `[CMD]`, заголовок `oc: CMD-0002`, чужой
  заголовок игнорируется, `oc:run` по-прежнему работает, законченный issue не
  повторяется, чужой автор молчит, `untrusted` пишется в лог один раз, битое тело
  на title-issue даёт «отклонено» + закрытие;
- `CloseAfterResultTest` (4): закрытие по умолчанию, `close_after_result: false`,
  no close в dry-run, падение закрытия не ломает круг;
- `OnceLockTest` (3): `--once` при занятом lock → код 1 и ноль вызовов `gh`,
  `--once` при свободном lock исполняет, `--dry-run` без lock;
- `ArgsTestLimits` (1) и `TestsAndBuildTest` (2): лимиты из `allow` доходят до
  валидатора, `..` и абсолютный путь отвергаются;
- `ConfigTest` (2): лимиты из `allow`, лимиты из корня.

Дополнительно прогнан сквозной прогон через настоящий `GhClient` с фейковым
`gh`-исполняемым файлом (проверка URL и методов, не только фейкового клиента):

```
api repos/o/n/issues?state=open&per_page=100&page=1 --method GET
api repos/o/n/issues/7/labels --method POST --input -
api repos/o/n/issues/7 --method GET
api repos/o/n/issues/7/comments --method POST --input -
api repos/o/n/issues/7/labels/oc%3Arunning --method DELETE
api repos/o/n/issues/7/labels --method POST --input -
api repos/o/n/issues/7 --method PATCH --input -
```

(issue без меток с заголовком `[CMD] CMD-0003 git_status`: найден по заголовку,
исполнен, закрыт; при `close_after_result: false` последней строки нет.)

### Что не прогонялось

- **Живой GitHub.** По условию задачи issues, метки и комментарии не создавались;
  закрытие и `state_reason` проверены только на фейковом `gh` и моках.
- **Windows.** Пути, `gh.exe`, `msvcrt`-ветка lock — не исполнялись (песочница на
  Linux). Правка `--once`/lock касается обоих режимов, но проверена на POSIX.
- **Настоящие `go`/`git`** за действиями — по-прежнему фейковый runner (Go в
  песочнице нет).
- **`deploy_check`** по живому healthz — не вызывался.

После merge стоит проверить именно issue #2: заголовок `oc: CMD-0002 ...`, тело
валидное, меток нет — он должен попасть в обработку по заголовку, получить
комментарий с `git_log -n3` и закрыться.

## 5. Diff и служебное

`git show --stat 47d8f8c`:

```
 AGENTS.md                          |  22 +++--
 docs/arena-command-bus.md          | 101 ++++++++++++++-----
 scripts/arena_watcher.example.json |   1 +
 scripts/arena_watcher.py           | 126 ++++++++++++++++++++----
 scripts/test_arena_watcher.py      | 195 ++++++++++++++++++++++++++++++--
```

В `47d8f8c` нет `cmd/`, `internal/`, `collab/` — и нет изменений git-моста,
`collab/bridge.json`, установщика, `control.sh`. Из production-кода удалён
неиспользуемый `GhClient.list_issues` (выборка теперь `list_open_issues`: три
страницы по 100, без меток).

## 6. Включение и откат (обновлено)

Включение:

1. `cp scripts/arena_watcher.example.json .arena/watcher.json`, заполнить `repo`,
   `workdir`, `allowed_logins`, `health[].url`; при желании — `close_after_result`.
2. `python scripts/arena_watcher.py --check-config` → `ok`.
3. Метки — **не обязательны** для Arena; `--init-labels` нужен только если вы
   пользуетесь триггером `oc:run` или хотите видеть состояние в списке issue.
4. `python scripts/arena_watcher.py --once --dry-run --verbose` (без lock), затем
   `--once` вживую, затем автозапуск. Помните: `--once` рядом с работающим
   сервисом теперь завершится ошибкой lock — это и есть защита.
5. Issue от Arena: заголовок `[CMD] ...`, тело с ```json блоком; метки не ставить,
   `gh issue create --label` не поможет — при нехватке прав метка просто не
   появится.

Откат: остановить процесс, удалить `.arena/watcher.json` (и `.arena/watcher.lock`
после жёсткого убийства). Issue с `[CMD]` перестанут читаться; уже закрытые остаются
закрытыми, `oc:*` при желании снимаются вручную. Git-мост и Go-код шина не
затрагивает.
