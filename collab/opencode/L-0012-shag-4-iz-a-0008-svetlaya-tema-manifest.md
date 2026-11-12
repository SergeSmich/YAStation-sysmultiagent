---
id: L-0012
title: Шаг 4 из A-0008: светлая тема, manifest.json, drag-and-drop карточек
from: opencode
to: arena
kind: task
status: open
branch: local/opencode
commit: 6c124d2
needs: A-0011
---

## Задача

Шаг 4 из A-0008: довести светлую тему (`body.light`) до согласованных токенов, отдать
`manifest.json` + иконки с сервера панели и добавить drag-and-drop порядок карточек
с сохранением в localStorage.

## Контекст

- Наша ветка `local/stage7-voices` = d4724ca (v0.9.1.20 выкачен и проверен на станции 12.11).
  Шаг 3 уже влит (64e690c): токенная шкала `--fs-*`/`--gap-*`/`--ctl-h`/`--r6…20` в `:root`,
  12-колоночная сетка `.settings-row.agent-grid` + `.span-*`, медиа 900/760/700/560.
- Светлая тема есть частично: `body.light` (panel.css:48–64) переопределяет фоны, текст,
  линии, `--shadow`, `--aurora-*`; точечные костыли на `select option` (200) и `.combo-menu` (516).
  Не проверены/не пересчитаны: `--grad`, `--on-accent`, `--err-line`, `--warn-line`, `::after`-акценты.
- Ассеты вшиты в бинарь: `cmd/quasar-bridge/panel.go:34`
  `//go:embed panel.html panel_login.html panel.css panel.js`; `GET /panel.css` и `GET /panel.js`
  (panel.go:226–227) отдают через `p.asset(name, contentType)`. CSP (panel.go:273–276):
  `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self';
  form-action 'self'; base-uri 'none'; frame-ancestors 'none'`, плюс `X-Frame-Options: DENY`.
  Инлайновые `<style>` и атрибуты `style=` запрещены; `data:` в `img` разрешён.
- Карточки для перестановки: `details.fold-card` (роли, нейросети) и `.settings-card`.
- Две наши правки поверх шага 3 — учесть в отчёте: `span-2` у «Имя роли», `span-4` у «Длина ответа»,
  и `min-width` селекта переехал с мёртвого `.role-length` на `.field select.role-length`
  (иначе `.field select { min-width: 0 }` перебивал по специфичности). На 760 px сетка 6 колонок,
  `span-4` уезжает в отдельную строку — рвано, но без вылетов за край; если есть дешёвое выравнивание
  (например, на ≤760 `.agent-grid .span-2`/`.span-4` → `span 3` или полная строка) — предложи.

## Критерий готовности

1. `body.light`: пересчитаны все токены, которые панель реально использует (включая `--grad`,
   `--aurora-*`, `--err-line`, `--warn-line`, `--on-accent`); нет мест с захардкоженным тёмным
   rgba/hex, дающих нечитаемый контраст в светлой теме. Список найденных мест — в отчёте.
2. `manifest.json` и иконки отдаются с сервера (новый `//go:embed` + маршрут(ы) с явным
   Content-Type), `panel.html` ссылается на манифест; CSP не ломается, страница грузится без
   ошибок в консоли.
3. Drag-and-drop: карточки меняются местами, порядок сохраняется в localStorage и переживает
   reload; «Сохранить»/«Проверить модель»/спойлеры продолжают работать (mousedown vs click);
   есть клавиатурный или кнопочный способ перестановки либо в отчёте явно сказано, почему его нет.
4. Отчёт `A-*`: список файлов и строк, что проверено локально (команды и результат),
   что остаётся на живую проверку оператором.

## Ограничения

- Править только `cmd/quasar-bridge/panel.css`, `panel.html`, `panel.js` + минимальный Go-код
  для отдачи манифеста/иконок (embed + маршрут). Не трогать: логику Go-бэкенда, `role_call`,
  `main_agent`, секреты, деплой-скрипты, апдейтер, `panel_login.html`.
- Никаких инлайновых стилей и `onclick`-атрибутов (CSP). Только внешние `panel.css`/`panel.js`
  и `data:` в img.
- Шаги 5–6 (музыка/netcheck, сценарии/таймеры) — не в этом круге.
- Порядок карточек — только presentation: ключ localStorage с префиксом (например,
  `quasar.panel.cardOrder`), никаких записей в config.json.
- MIT-атрибуция MidiPult в шапке panel.css сохраняется.
