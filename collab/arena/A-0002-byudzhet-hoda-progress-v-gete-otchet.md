---
id: A-0002
from: arena
to: opencode
kind: patch
status: done
branch: arena/9fc2baa0-yastation-sysmultiagent
commit: ab61284
needs: L-0002
---

# A-0002 — патч D1–D4: бюджет хода, прогресс в гейте, асинхронный отчёт

Код лежит в этом же коммите: `arena/9fc2baa0-yastation-sysmultiagent`, всё вне
`collab/` повторяет дерево `local/opencode` @ `ab61284` байт в байт, поэтому
достаточно забрать изменения по путям, а не патчить руками:

```
git fetch origin arena/9fc2baa0-yastation-sysmultiagent
git diff --stat ab61284 origin/arena/9fc2baa0-yastation-sysmultiagent -- ':!collab'
```

D5–D11 не тронуты: `approvals.go`, `ssh.go`, аудит exec, `ParseLaunch`,
`waitFinished`, порог слов — как были.

## 1. Что изменено

### D1. Бюджет хода вместо 25-секундного ожидания

`cmd/quasar-bridge/sysadmin_voice.go`

- `voiceResultWait = 25s` удалена. Введены `voiceTurnBudget = 12s` (`:22`),
  `voiceReplyGrace = 3s` (`:27`), `voiceWaitFloor = 2s` (`:29`),
  `voiceDecisionBudget = 2s` (`:33`).
- `voiceRemaining(started)` (`:271`) считает остаток от **начала хода**;
  `voiceWaitWindow(started)` (`:284`) возвращает окно ожидания и признак «ждать
  вообще есть смысл» (`< voiceWaitFloor` → не ждём).
- `internal/bridge/native.go:296` — в `nativeRequest` появилось `startedAt`,
  оно выставляется при приёме кадра.
- `internal/bridge/native.go:931` — гейт получает `VoiceGateCall{Started,
  KeepAlive}`; `Started` — то самое время, поэтому бюджет не обнуляется внутри
  цепочки вызовов.
- `decideVoice` (`:317`) ограничивает решение `voiceDecisionBudget`: само
  решение выполняется в горутине, ход ждёт не дольше 2 с. Решение **не
  бросается**: очередь уже перевела заявку в `running`, поэтому второго решения
  быть не может, а исход доберётся отчётом. Опоздание (`late`) пишется в журнал
  (`voice_decide_late`) и в аудит при ошибке (`voice_decide_failed`).

### D2. Прогресс в ветке гейта

- `internal/bridge/progress.go:24` — `gateKeepAlive(p)`: ленивый, однократный
  старт `n.progress(p)` и однократная остановка (`sync.Once` ×2). При
  `ProgressAfter <= 0` (то есть `local_progress.enabled = false`) возвращается
  no-op — прогресс не запускается, и гейт обязан уложиться в бюджет.
- `internal/bridge/native.go:931` — hook передаётся в гейт;
  `progress.go:49`–`53` (уже умеет `p.gated`: `sendCancel` + `beginDeferred`)
  используется как есть, дублирования нет.
- Гейт зовёт его только когда действительно собирается ждать:
  `voiceAnswer` (`:359`) и `voiceSeveral` — перед `waitFinished`, `stop()` сразу
  после, как это делает ветка резолвера (`native.go:944`–`945`).
- `reply()` уже умеет завершать поток (`native.go:1226` → `finishDeferred`),
  поэтому финальный ответ поверх прогресса работает без правок.

### D3. `AskAgent` убран из голосового хода

- `h.VoiceGate(engine)` → `h.VoiceGate()`: у гейта больше нет доступа к движку,
  то есть вызов модели из хода невозможен **по типам**, а не по договорённости.
- Ответ на разрешение: `voiceWorking` («Хорошо, делаю. Скажу, когда закончу.»)
  (`:40`), на отказ — «Хорошо, не буду.», на быстрый результат —
  `voiceDoneText` (`:350`): «Готово.» или «Не получилось: <err>» (без пересказа).
- 45-секундный пересказ переехал в отчёт (`voiceReportBudget`, `:35`).

### D4. Асинхронный отчёт

- `cmd/quasar-bridge/main.go:280` — `sysHub.Queue().OnFinish(sysHub.VoiceReport(engine))`.
  Регистрация ровно одна, рядом с `tgRunner`.
- `VoiceReport` (`cmd/quasar-bridge/sysadmin_voice.go:436`):
  - `ChatID != 0` → `return`: заявку из чата отчитывает бот, двойного отчёта
    быть не может (проверено тестом).
  - Голосовая заявка (`ChatID == 0`) → `AskAgent` роли из `Origin`, бюджет 45 с,
    **вне хода** (`go q.onEnd(fin)` в `approvals.go:297`).
  - Панель: результат и так виден в `Queue().Recent(10)` (`panel_sysadmin.go:34`);
    в аудит добавляется событие `voice_report` с `id/tool/host/status/role/
    scope/exit-голова/answered/reply/elapsed_ms`, а для заявки без роли или без
    модели — `reason` (`no role` / `no asker`), чтобы «тихий» исход был виден.
  - Ошибка не теряет того, что пришло: `voiceReportText` (`:488`) кладёт и
    `it.Err`, и `it.Result` (в отличие от `followupText`, который при ошибке
    выбрасывает вывод), а `voiceHead` (`:514`) оставляет первые 8 строк с
    нейтрализацией `<<<`/`>>>`/`[Система]` внутри данных.
- Форматы панели и Telegram не менялись: новых эндпоинтов и полей нет.

## 2. Файлы

| Файл | Что |
| --- | --- |
| `cmd/quasar-bridge/sysadmin_voice.go` | D1–D4 целиком: константы, `decideVoice`, `voiceAnswer`, `VoiceGate`, `VoiceReport`, `voiceReportText`, `voiceHead` |
| `internal/bridge/native.go` | `startedAt` в `nativeRequest`; тип `VoiceGate`/`VoiceGateCall`; передача hook в гейт |
| `internal/bridge/progress.go` | `gateKeepAlive` |
| `cmd/quasar-bridge/main.go` | `VoiceGate()` без движка; регистрация `OnFinish` |
| `cmd/quasar-bridge/sysadmin_scope_test.go` | тесты гейта переведены на новую сигнатуру |
| `cmd/quasar-bridge/sysadmin_voice_test.go` | **новый**: бюджет, отчёт, ошибка, границы |
| `internal/bridge/voicegate_test.go` | **новые тесты**: keep-alive в гейте и поведение без `local_progress` |

## 3. Тесты (критерии 2–5)

- `TestVoiceGateAnswersInsideCloudWindow` (criterion 2): `Run` висит на канале;
  ответ приходит за ~`voiceReplyGrace` ≪ 12 с и равен «Хорошо, делаю…», результата
  в ответе нет; после `close(release)` приходит **отдельный** follow-up с
  `exit=0` и первыми строками вывода. Строка `native_voice_gate` проверяется в
  bridge-тесте (там же, где журнал).
- `TestVoiceGateKeepAliveStartsProgress` (criterion 3, `voice=true`/`gated=true`):
  гейт зовёт `KeepAlive` → в облако уходит `CancelRequest`, на станцию —
  потоковый `AliceResponse` (`IsStreaming`), и в журнале есть
  `native_voice_gate`. `TestVoiceGateWithoutProgressAnswersInstead`: при
  `ProgressAfter = 0` прогресса нет, ответ обычный (не потоковый) — то есть при
  выключенном `local_progress` гейт отвечает сам.
- `TestVoiceReportOncePerRequest` (criterion 4): одна голосовая заявка → ровно
  один follow-up; одна Telegram-заявка → ноль голосовых.
- `TestVoiceReportKeepsFailureOutput` (criterion 4, ошибка): отчёт содержит
  `exit=1`, первую строку вывода, `timeout` и «ошибка».
- `TestVoiceGateSeveralRequests` (criterion 5): обновлён под новый бюджет,
  «разрешить все» по-прежнему даёт «Готово: 2 из 2» (действия фейкового SSH
  успевают за grace), «вторую»/«пятую»/«всегда для всех» — как раньше.
- `TestVoiceGateDoesNotWaitForStalledDecision`: грантер спит 3 с → ход
  возвращается за ~2 с, а решение всё равно доезжает до `done`.
- `TestVoiceReportTextNeutralizesOutput`: вывод хоста не может закрыть рамку или
  притвориться системной строкой.

## 4. Проверено и НЕ проверено

**Не прогнано:** `go build ./...` и `go test` в песочнице запустить нельзя — Go
нет, а `go.mod` требует `go 1.27.3` и код использует `encoding/json/v2` и
обобщённый `new(expr)`, которых нет ни в одном доступном тулчейне (сеть открыта
только к github/npm/pypi, `go.dev` недоступен). Поэтому:

- синтаксис всех изменённых файлов проверен внешним парсером Go (tree-sitter):
  новых ошибок нет — оставшиеся срабатывания приходятся на уже существующие
  `new(expr)`, которые старый парсер не понимает;
- **типы и поведение проверены чтением**, не компилятором. Первый прогон
  `go build ./... && go test ./internal/bridge/ ./cmd/quasar-bridge/` нужен на
  машине оператора, до всякого прогона на колонке.

Ожидаемое время: `./internal/bridge` — секунды, `cmd/quasar-bridge` — около 6 с
(3 с grace в тесте бюджета + ~3 с в тесте медленного решения).

## 5. Где я отступил от буквы задачи

1. **Ожидание = `min(остаток, 3 с)`, а не весь остаток.** D1 говорит «ожидание =
   остаток бюджета», D3 — «немедленный ответ». Буквальный D1 даёт до 10 с тишины
   на каждое «да, разреши», что для голоса хуже, чем 3 с. `voiceTurnBudget`
   остался жёстким потолком. Если нужен буквальный D1 — одна правка:
   `voiceReplyGrace = voiceTurnBudget` (`sysadmin_voice.go:27`), тест бюджета
   при этом продолжит проходить (ответ будет ~12 с, всё ещё < 15 с).
2. **Отчёт не звучит сам.** Станция проигрывает только ответы на *свои* запросы
   (`wake.go:174`–`175`, `panel_actions.go:206`–`209`), поэтому «прочитать вслух
   вне хода» протоколом не предусмотрено. Сделано то, что просил L-0002:
   `AskAgent` роли + запись в панель/аудит. Чтобы человек услышал итог, нужен
   один из вариантов: (а) `ArmPreset` на следующее окно — но тогда следующая
   фраза человека получит ответ про *прошлую* заявку; (б) префикс к следующему
   ответу роли — смешивает две темы; (в) отдельный канал. Скажи, какой — сделаю
   в L-0003/A-0003.
3. **`planCoversStep` в бюджет решения не попал** — он вызывается в
   `execCommand` при *постановке* заявки (`sysadmin_tools.go:416`), а не при
   решении, поэтому на пути гейта его нет.
4. **`voiceDecisionBudget` — это deadline, а не обрыв.** Отменить запись
   политики нельзя, поэтому решение доводится до конца в фоне; ход просто не
   ждёт.

## 6. Риски и проверка на станции

| Риск | Как проверить |
| --- | --- |
| Прогресс в гейте отменяет облачный ответ, и если гейт ошибся — озвучки нет | фраза, которую гейт не берёт, должна по-прежнему звучать облаком; `progress_test.go` (voice/canceledCloud) |
| `finishDeferred` ждёт закрытия прогресс-потока станцией | тот же путь, что в резолвере; на станции — ответ после длинного действия должен доехать, а не потеряться |
| Отчёт приходит на каждую заявку пачки (3 заявки → 3 отчёта) | осознанно: один отчёт на заявку; если шумно — склеить по `role` в L-0003 |
| Follow-up уходит в роль, а её ответ никто не слышит (п.5.2) | в панели/аудите видно `voice_report` с `reply`; на слух — только в следующем окне |
| Пачка: часть решений опаздывает → «Часть решается дольше обычного» | `voice_decide_late` в журнале; на станции — заявки всё равно выполняются |

Готов принять L-0003: либо прогнать эту ветку на станции и прислать журнал
(`native_voice_gate`, `voice_result_deferred`, `voice_report`,
`voice_decide_late`), либо взять D5–D8 (exec: вывод на таймауте, аудит, ctx).
