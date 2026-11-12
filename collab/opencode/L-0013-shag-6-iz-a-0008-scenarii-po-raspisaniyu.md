---
id: L-0013
title: Шаг 6 из A-0008: сценарии по расписанию — планировщик и карточка в панели
from: opencode
to: arena
kind: task
status: open
branch: local/opencode
needs: A-0008
---

## Задача

Шаг 6 из `A-0008`: сценарии по расписанию. Один сценарий = время `HH:MM` + дни
недели + одно действие; сервер срабатывает сам по локальному времени станции,
панель показывает и редактирует список. Голосовая маршрутизация не меняется:
«поставь будильник/таймер» голосом по-прежнему уходят в Яндекс.

## Контекст

Планировщика в коде нет: `scenario`/`scheduler`/`cron` в `cmd/` и `internal/`
не встречаются. Таймеры и будильники голосом обслуживает Яндекс
(`internal/bridge/routing.go:92` правило, `:102` промпт классификатора) — это
не трогаем.

Что уже есть и на что опираться:

- маршруты панели: `cmd/quasar-bridge/panel.go:233-264` (`requireSession` /
  `requireMutation`); у `panelServer` есть `ConfigPath` (`main.go:576`).
- атомарная запись файла: `writeFileAtomic(path, data)` —
  `cmd/quasar-bridge/voiceconfig.go:183` (tmp + fsync + rename, режим 0600).
- громкость станции: `writeStationVolume(percent int) error` —
  `panel_volume.go:65`.
- звук: `assistant.Tools.Call(ctx, "play_sound", args)` —
  `internal/assistant/tools.go` (`Call`), `internal/assistant/sound.go`
  (`soundDefinition`); допустимые источники: `notification`, `success`,
  `error`, `timer` или точный http(s)-URL.
- зона времени: `device_context.timezone` (`config.go:125`, дефолт
  `Europe/Moscow`), проверка `validTimezone` (`config.go:810`).
- UI: карточки строятся в `panel.js` (`card(...)`, `foldCard`), порядок карточек
  (`quasar.panel.cardOrder`) вешается на контейнер `[data-card-order-wired]`,
  ручка — `summary`/`.list-head`/`h2`; CSP запрещает инлайн-`<style>` и
  `onclick`.

## Что сделать (MVP)

1. **Хранилище.** Файл `scenarios.json` рядом с `config.json` (каталог
   `filepath.Dir(panelServer.ConfigPath)`), 0600, запись только через
   `writeFileAtomic`. `config.json` не расширять. Схема:

   ```json
   {"scenarios":[{"id":"s-1","enabled":true,"time":"07:30","days":[1,2,3,4,5],
     "action":{"type":"sound","source":"timer"},
     "last_fired":"2026-11-13T07:30"}]}
   ```

   `days` — ISO 1..7, 1 = понедельник. `action` — ровно один из двух вариантов:
   `{"type":"sound","source":"notification|success|error|timer"}` или
   `{"type":"volume","percent":0..100}`. `id` генерирует сервер.

2. **Планировщик.** Одна горутина, тик 20–30 с, время в зоне
   `device_context.timezone`. Сценарий срабатывает не более одного раза в
   минуту (по `last_fired`) и не повторяется после рестарта в ту же минуту.
   Действия вызываются вне блокировки списка. Если `assistant_tools.enabled`
   false, `sound`-сценарий пишет предупреждение в журнал и не роняет процесс;
   `volume` работает независимо.

3. **API.** `GET /api/panel/scenarios` (session) отдаёт
   `{"timezone":"...","scenarios":[...]}`. `POST /api/panel/scenarios`
   (mutation) принимает полный список, валидирует (`HH:MM`, дни 1..7 непустые,
   `percent` 0..100, тип действия из двух) и возвращает сохранённый список.
   Ошибки — по-русски, 400 без изменения файла.

4. **Панель.** Карточка «Сценарии» во вкладке «Станция»: список строк
   (включён, время, дни, действие), `+` — добавить строку, «Удалить» — убрать,
   сохранение по карточке, после сохранения список перечитывается. Карточка
   участвует в существующем порядке (`data-card-key`, ручка), без инлайн-стилей
   и `onclick`, поля в 12-колоночной сетке.

## Критерий готовности

- `go build ./...`, `go vet ./...`, `go test ./cmd/quasar-bridge ./internal/bridge -count=1`
  зелёные; новые тесты: (а) сценарий срабатывает ровно один раз в минуту и
  только в свои дни; (б) `POST` с битым временем/процентами → 400 и файл не
  изменился; (в) `scenarios.json` пишется с режимом 0600; (г) пустой список
  валиден; (д) `GET` не отдаёт ничего лишнего.
- В панели: добавить сценарий «ближайшая минута, сегодня, звук `timer`» →
  сохраняется, через минуту сигнал слышен, `last_fired` обновился; удалить →
  запись исчезла из файла.
- Голосовые «поставь будильник» по-прежнему уходят в Яндекс (routing не
  изменён).

## Ограничения

- Не менять `internal/bridge/routing.go`, голосовую ветку и `role_call`;
  сценарии голосом не создаются и не перехватывают вызовы ролей.
- `main_agent`, `secrets.json`, секреты, деплой, апдейтер и PowerShell-механизм
  не трогать.
- Только `panel.css`, `panel.html`, `panel.js` и Go; CSP: без инлайн-стилей и
  `onclick`; ошибки пользователю — по-русски.
