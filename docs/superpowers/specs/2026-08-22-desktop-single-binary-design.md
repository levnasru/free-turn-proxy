# VK-TURN Desktop Single-Binary — дизайн

Дата: 2026-08-22. Статус: одобрено на словах пользователем в чате, ждёт review этого файла.

## Контекст

`vkturn-desktop` сегодня — это оркестратор, который спавнит `client` (наш TURN+bond
Go-бинарник) и `xray` (VLESS-мост/tun-инбаунд) как отдельные subprocess'ы через
`exec.CommandContext` (`cmd/desktop/launcher.go`: `RunClient`/`RunXray`,
`resolveClientBin`/`resolveXrayBin` ищут их рядом с исполняемым файлом на диске).
Итог — три отдельных файла на дистрибутив/платформу (`client`, `vkturn-desktop`,
`xray`, плюс `wintun.dll` на Windows после сегодняшней TUN-фичи), что захламляет
страницу релиза на GitHub и путает при раздаче семье.

Ранее в этой же сессии рассматривался вариант "встроить client/xray как
библиотеки in-process" (через уже существующие `mobile.StartFlags`/`libxray.Invoke`
— оба паттерна уже проверены в этом репо на Android/iOS-сборках) — пользователь
предпочёл другой путь: **сохранить subprocess-модель как есть** (изоляция сбоя:
упавший `xray` не должен ронять весь `vkturn-desktop`), но **упаковать все три
бинарника в один файл** через `go:embed`, распаковывая их на диск при старте.

## Цель

Один `vkturn-desktop`/`vkturn-desktop.exe` на платформу в дистрибутиве вместо
трёх(+) отдельных файлов. Поведение приложения не меняется — тот же subprocess-
оркестратор, тот же `RunClient`/`RunXray`, то же меню, те же флаги.

## Не-цели

- Не переходим на in-process embedding (`mobile.StartFlags`/`libxray.Invoke`) —
  рассмотрено и отклонено пользователем в пользу сохранения process-изоляции.
- Не трогаем `cmd/client`, `cmd/server`, `mobile/*`, `internal/proxy/*` — они
  как отдельные бинарники/пакеты продолжают существовать и собираться как
  раньше; просто их вывод для desktop-таргета теперь дополнительно копируется
  в место, откуда `cmd/desktop` их embed'ит.
- macOS explicitly не в скоупе (desktop-кит сегодня — только windows_amd64/
  linux_amd64, macOS собирается и распространяется отдельно, см.
  `docs/superpowers/specs/2026-08-14-desktop-client-design.md`).

## Архитектура

### Двухпроходная сборка

1. **Проход 1** — собрать `client` и `xray` для целевой платформы обычным
   образом (`go build ./cmd/client`, `go build github.com/xtls/xray-core/main`),
   положить результат в `cmd/desktop/embedded/<goos>_<goarch>/` (например
   `cmd/desktop/embedded/linux_amd64/client`). Для Windows-таргета туда же
   кладётся `wintun.dll` (тот же файл, что сегодня качается и хешируется в
   `release.yml`'s "Fetch wintun.dll" step — переиспользуем этот шаг, меняем
   только пункт назначения).
2. **Проход 2** — собрать `cmd/desktop` обычным `go build`; `//go:embed`-
   директивы читают файлы из `embedded/<goos>_<goarch>/` на диске в момент
   сборки и зашивают их байты в `vkturn-desktop`.

`cmd/desktop/embedded/` — gitignored (аналог `/xray-dist/`, `/desktop-macos-dist/`
в текущем `.gitignore`), населяется build-скриптом/CI перед запуском прохода 2,
никогда не коммитится.

### Embed-директивы (build-tag per platform, как уже принято в этом пакете)

`go:embed` не умеет читать файл по пути, зависящему от `GOOS`/`GOARCH`
(директива статична) — поэтому embed делается через platform-specific файлы
по уже принятой в этом пакете конвенции (`tray.go`/`tray_other.go`,
`netroute_linux.go`/`netroute_windows.go`):

- `cmd/desktop/embed_linux.go` (`//go:build linux`):
  ```go
  //go:embed embedded/linux_amd64/client
  var embeddedClient []byte
  //go:embed embedded/linux_amd64/xray
  var embeddedXray []byte
  ```
- `cmd/desktop/embed_windows.go` (`//go:build windows`):
  ```go
  //go:embed embedded/windows_amd64/client.exe
  var embeddedClient []byte
  //go:embed embedded/windows_amd64/xray.exe
  var embeddedXray []byte
  //go:embed embedded/windows_amd64/wintun.dll
  var embeddedWintun []byte
  ```

### Рантайм: распаковка

Один новый файл `cmd/desktop/extract.go` (без build-тега, общий): функция
`extractEmbedded() (dir string, err error)`, вызывается один раз в начале
`main()`, до первого обращения к `resolveClientBin`/`resolveXrayBin`:

- Целевая директория — `~/.vkturn/bin/` (та же root-директория, что уже
  используется под `config.json`/`debug.log`, `CachePath()`/`debugLogPath()`
  — уже решённая проблема "куда писать под pkexec", включая сегодняшний фикс
  переброса `$HOME` в `elevate_linux.go`).
- Пишет `embeddedClient`/`embeddedXray`[`/embeddedWintun`] в
  `~/.vkturn/bin/{client,xray}[.exe][,wintun.dll]` с правами `0o755`
  (Windows игнорирует unix-биты, безвредно).
- Перезаписывает безусловно на каждом старте — без version-check/кеширования:
  несколько MB записи на диск не заметны рядом с сетевой частью приложения,
  а протухания версии в принципе быть не может.

### Что меняется в существующем коде

`cmd/desktop/launcher.go`'s `resolveClientBin(dir)`/`resolveXrayBin(dir)` —
сейчас ищут файл рядом с исполняемым файлом (`resolveBin`, несколько
кандидатов пути). Меняются на возврат пути из `extractEmbedded()`'s
результата вместо поиска на диске. `RunClient`/`RunXray`, вся subprocess-
оркестрация (`exec.CommandContext`, каналы `<-chan error`, `-bind-iface`
через `RunClient`'s `extraArgs`) — **не меняются вообще**, работают как есть
поверх извлечённых путей.

## Дистрибутив

- `.goreleaser.yaml`'s `desktop` build (`windows_amd64`/`linux_amd64`) — без
  изменений в самой build-секции; меняется то, что `release.yml`'s job
  теперь собирает `client`/`xray` (уже делает это сегодня для `xray-dist`)
  **до** запуска goreleaser, копируя результат в `cmd/desktop/embedded/`
  вместо (или в дополнение к) текущего `xray-dist/`.
- `release.extra_files` — `client-*`/`xray-*`/`wintun.dll` больше не нужны
  как отдельные release-ассеты specifically для desktop-кита (они всё ещё
  публикуются отдельно для CLI-пользователей/Termux/ручных kit'ов — это вне
  скоупа этого дизайна, не трогаем `raw`/`raw-android` архивы). Итоговый
  desktop-релиз — один `vkturn-desktop-<os>-<arch>` файл.

## Обработка ошибок

- `extractEmbedded()` возвращает ошибку → `main()` печатает понятное
  сообщение ("не удалось распаковать встроенные компоненты: %v") и
  завершается, не показывая меню (аналогично сегодняшней обработке ошибки
  `LoadCache`).
- Права на запись в `~/.vkturn/bin/` — та же директория, что уже
  используется под конфиг/логи, уже пишется успешно при нормальном
  (неэлевированном) запуске; для tun-режима элевированный процесс уже решает
  эту проблему сегодняшним фиксом (`$HOME` пробрасывается через `pkexec env`).

## Тестирование

- Юнит-тест на `extractEmbedded()`: пишет во временную директорию (не в
  реальный `~/.vkturn/`), проверяет, что байты на диске совпадают с
  `embeddedClient`/`embeddedXray`, и что файлы исполняемые (`0o755`) на
  Linux.
- Ручной golden-path (как в предыдущем плане): собрать двухпроходной сборкой
  реальный `vkturn-desktop` под linux/amd64, убедиться, что рядом больше не
  нужны `client`/`xray`, запустить все три режима меню, подтвердить, что
  `~/.vkturn/bin/` наполняется и подпроцессы стартуют оттуда.

## Открытые риски

- Первый запуск после установки будет чуть медленнее (запись ~50-80MB на
  диск при каждом старте, не только при первом) — не измерено, вероятно
  не критично на SSD, но стоит замерить на реальной машине перед тем как
  объявлять фичу готовой; если заметно — тривиально добавить дешёвый
  version-check позже (не сейчас, YAGNI).
- Antivirus/SmartScreen на Windows иногда более подозрительно относится к
  программам, которые распаковывают и запускают скрытые встроенные
  бинарники в рантайме — не новый риск специфично для этого дизайна (обычная
  практика для self-contained CLI-инструментов), но стоит держать в уме при
  живом тесте на Windows.
- CI-порядок сборки (клиент+xray → embedded/ → desktop) должен быть жёстко
  зафиксирован в `release.yml` — если кто-то переставит шаги местами,
  goreleaser соберёт `vkturn-desktop` со старыми/пустыми embedded-файлами
  молча (embed директива на пустой файл не ошибка сборки). Стоит добавить
  явную проверку размера файла (`[ -s cmd/desktop/embedded/... ]`) перед
  шагом 2 как дешёвую защиту.
