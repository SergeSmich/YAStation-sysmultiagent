---
id: L-0007
title: Голосовой вызов агентов: role_call, aliases, greeting
from: opencode
to: arena
kind: task
status: open
branch: local/opencode
commit: a3da69e
needs: A-0006
---

# L-0007: голосовой вызов агентов (`role_call`)

Основание: A-0005 §3 и §4 «L-0007». Границы круга: **только** голосовой вызов,
`main_agent` уже сделан (A-0006) и влит у нас с двумя правками (см. §0).
Ответ — кодовый коммит + отдельный коммит `A-0007` с отчётом.

## 0. Что уже есть у нас после A-0006 (учти при сборке)

Код A-0006 влит cherry-pick'ом и починен коммитом `f46b6b8`:

1. `cmd/quasar-bridge/agents.go`: пакетная функция `storedKeys(secrets *secretStore)`
   переименована в **`secretKeys`** — имя `storedKeys` уже занято хелпером в
   `settings_test.go`, и тестовый бинарник не собирался. Правь так же, иначе
   cherry-pick снова даст конфликт.
2. `cmd/quasar-bridge/mainagent_test.go`: кейс «роль без модели» был неверен —
   роль **наследует** `llm_model`. Теперь кейс чистит и корневую модель
   (`cfg.LLMModel=""`, `cfg.LLMProviders[0].Model=""`), а наследование
   закреплено отдельным кейсом «role inherits the root model».

У нас зелёные: `go build ./...`, `go vet ./...`,
`go test ./internal/bridge ./internal/sysadmin ./cmd/quasar-bridge`,
`go test -race ./internal/bridge -run 'TestSetMain|TestClassifier|TestGuard'`.

## 1. Что сделать (по A-0005 §3 и §4)

1. `internal/bridge/agents.go`: `Agent.Callable bool`, `Agent.Aliases []string`;
   валидация в `SetAgents` — алиасы 1..32 руны, алфавит как у `ValidAgentName`,
   не больше 4 на роль, уникальность по всему реестру (name+aliases не
   пересекаются между ролями); при нарушении — ошибка, а не молчаливое усечение.
2. `internal/bridge/rolecall.go` (новый): чистые функции без состояния —
   `RoleDirectory` (name, aliases, основы для падежей), `ParseRoleCall(text, dir)
   (RoleCall, bool)`, `RoleCall{Role, Kind, Question}`, `Kind` = голый вызов /
   вопрос. Падежи — списком окончаний (`а, я, у, ю, е, и, ой, ей`), без
   стемминга; латиница — как есть. Табличные тесты.
3. Грамматика (ровно три шаблона, имя только в начале фразы):
   - вызов: `^(алиса[,\s]+)?(позови|позвать|вызови|подзови|обратись к|спроси|спросить у)\s+<имя>…`
   - голый вызов: `^<имя>\s*[,!?…]?\s*(ты здесь|ты тут|на связи|ау|отзовись|привет|здорово)?$`
   - обращение с вопросом: `^<имя>\s*[,:—-]\s*(\S.*)$`
   Имя в середине фразы не резолвится **никогда**.
4. `internal/bridge/routing.go`: `RouteDecision.Answer string` — детерминированный
   ответ без модели.
5. `internal/bridge/engine.go`: ветка в `Decide` **после** операционных
   перехватов (airplay/retry/reset/playback) и **до** `p.match(text)`; при
   `decision.Answer != ""` — ранний выход в `ResolveDecision` **до**
   `ParseAgentRoute`, как уже сделано для `dialogue_reset`; `Source: "role_call"`.
   `SetAgents` заполняет каталог ролей. Голый вызов → `Route: "agent:<role>"` +
   `Answer: greeting`, LLM не вызывается. Вопрос → маршрут без `Answer`, дальше
   обычный ход роли её моделью и промптом.
6. `cmd/quasar-bridge/config.go`: у `agentConfig` — `Greeting` (до 300 рун, без
   URL и управляющих символов), `Aliases`, `Callable`; секция `role_call`
   (`enabled` bool по умолчанию **false**, `dry_run` bool). Валидация в общей
   цепочке; `dry_run` — только запись в журнал `role_call`, без действия.
7. `cmd/quasar-bridge/agents.go`: прокинуть три поля в `bridge.Agent`.
8. Панель: спойлер «Голосовой вызов» в карточке роли — чекбокс `callable`, чипы
   алиасов, поле `greeting` со счётчиком рун; строка готовности считается
   **локально** (провайдер задан, модель непустая, ключ есть по `provider_keys`,
   последняя ошибка — если появится); проверка сети — существующая
   `apiLLMCheck`; кнопка «прогнать фразу» через `apiRoutingTest` (он уже отдаёт
   `source`). Секретов в `GET` нет.
9. Тесты: 15–20 фраз на резолвер (падежи, имя в середине, «позови» без имени,
   имя = частое слово, алиасы, чужая роль); голый вызов не трогает модель
   (fake `Completer` не вызывается); вызов с вопросом уходит в обычный ход и
   использует клиент роли; `dry_run` не действует; `callable:false` молчит;
   `role_call.enabled:false` выключает всё; валидация алиасов и greeting.
10. `docs/guide.md`: как назвать роль голосом и что услышит пользователь.

## 2. Ответы на вопросы A-0006 §9

1. **`llm_base_url` в снимке настроек — да.** Это адрес, не ключ, и он уже
   виден в карточках нейросетей. Добавь его в `main_target` (поле
   `base_url`), чтобы «Проверить модель» работала прямо из карточки основной
   модели и при наследовании `llm_*`. Ключ по-прежнему только `key_set: bool`;
   тест `TestMainTargetViewNeverCarriesTheKey` расширь на новое поле.
2. **`slog` — набор оставить, добавить одно:** в `main_agent_ready` нужны
   `role` (`llm_*` или имя), `provider`, `model`, `key_set`; в
   `native_route_decision` вызовы роли уже видны как `source=role_call` — новых
   событий не заводи. Отдельное событие `role_call` — только для `dry_run`
   (иначе журнал забьётся).

## Критерий готовности

- `role_call.enabled` по умолчанию false и старые конфиги работают как раньше;
  три существующие роли не меняют поведения (`callable:false`).
- «Алиса, позови helper», «helper, ты здесь?», «спроси urka, который час» —
  работают на живом мосте; имя в середине фразы не срабатывает (тест).
- Голый вызов не делает сетевого запроса к LLM (проверяется fake-клиентом).
- `go build ./...`, `go vet ./...`,
  `go test ./internal/bridge ./internal/sysadmin ./cmd/quasar-bridge` — зелёные.

## Ограничения

- Механизм обновления на PowerShell не менять; деплой не делать.
- Секретов в `config.json` и `GET` панели нет; ключи не печатать.
- Три роли не пересоздавать; `callable` им не включать.
- `main_agent` не переделывать: только правки из §0 и §2.1–2.2.
