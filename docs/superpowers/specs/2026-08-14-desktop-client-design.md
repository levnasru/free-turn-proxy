# VK-TURN Desktop Client — дизайн

Дата: 2026-08-14. Статус: одобрено на словах пользователем в чате, ждёт review этого файла.

## Контекст

Windows/Linux родня сейчас получает доступ через `win-kit2`/`vkturn-linux-kit` —
папки, которые собираются и раскладываются вручную: `ftp-client.exe`/`ftp-client`
(наше ядро) + `xray.exe`/`xray` (SOCKS-фронт) + `.bat`/`.sh`-лаунчеры +
`config.bat`/`config.env` с секретами конкретного родственника, вписанными руками.
Смоук-тест Linux прошёл 2026-08-03, но раздача — целиком ручной труд, и обновление
секретов (ротация hub-token, новый obf-key) требует переслать новый файл каждому.

Формальный релиз (P2) в его буквальной форме — тег + бинарники в GitHub Release —
уже закрыт автоматически `.goreleaser.yaml` (см. `v2.1.1`, ассеты
`client-windows-amd64.exe`/`client-linux-amd64` и т.д.). Реальный запрошенный
результат другой: убрать ручную раскладку конфигов вообще, дать родне
self-service вход по логину/паролю (как уже работает для iOS через
`lft.levnas.ru`), и в перспективе — полноценный перехват трафика адаптером
(WireGuard), а не только системный SOCKS-прокси.

### Что уже есть и переиспользуется

- **`vkturn-ios-portal`** (`/home/lev/vkturn-ios-portal/main.go`, деплоится на
  VPS-NL, публично на `lft.levnas.ru:443` через HAProxy SNI-passthrough на
  `127.0.0.1:8449`) — единственный уже живой публичный self-service сервис с
  реальной аутентификацией: bcrypt-пароли, HMAC-подписанные сессии
  (`signSession`/`verifySession`), `users.json`, CLI `adduser`/`deluser`,
  управление WG-пирами на интерфейсе `wgcl` (10.13.13.0/24 — тот же диапазон,
  что уже использует Android через `panel.js`).
- **`panel.js`** (`/home/lev/vkturn-android-kit/`, ai-server) — админ-инструмент
  создания профилей, loopback-only намеренно (без своей аутентификации). Остаётся
  им; получает пятый `device`.
- **`x-ui` (3x-ui)** — уже развёрнут и работает на VPS-NL (`systemctl` подтвердил
  `x-ui.service active running`). Subscription-инфраструктура для xray готова,
  ничего нового поднимать не нужно — только привязать per-user `subId`.
- **`internal/uri`/`internal/sub`/`docs/sub.md`** (апстрим) — механизм
  `-sub`/`freeturn://` для провайдера `vk` **рассмотрен и отклонён** как транспорт
  для vk-turn-конфига: схема `wire` в `internal/uri/uri.go` не несёт
  `hub-url`/`hub-pin`/`hub-token` (специфика форка), а этот файл не входит в три
  санкционированных точки врезки форка (`config.go`/`cmd/client/main.go`/
  `mobile/mobile.go` — см. CLAUDE.md). Добавлять туда hub-поля значит заводить
  четвёртую точку патча, которую `wire_hub.py` не восстановит после rebase.
  Вместо этого — отдельный, чисто форковый JSON API на портале (ниже).
- **`turn-proxy-android`'s `XraySubscriptionFetcher.kt`** — уже парсит 3x-ui-style
  base64 subscription (список `vless://...` share-ссылок) через
  `libXray.invoke(convertShareLinksToXrayJson)`. На Android это JNI-мост; в Go
  desktop-клиенте та же логика доступна напрямую как `github.com/xtls/libXray` —
  без моста, чистый Go. URL подписки сейчас вставляется в приложении руками —
  автоматизация со стороны Android только запланирована, не сделана; в этом
  дизайне автоматизация делается для десктопа, не переиспользуется с Android.

## Цели

1. Родня логинится (логин/пароль) вместо получения файла конфига руками.
2. Один кросс-платформенный (Windows/Linux) бинарник, без GUI — терминальное меню
   стрелками+Enter.
3. Три режима на выбор пользователя за один процесс: **vk-turn** (наше ядро
   напрямую), **xray по подписке** (3x-ui), **WG full-tunnel** (адаптер,
   перехватывает весь трафик, как на Android/iOS).
4. WG-режим требует прав администратора/root — это принято пользователем как
   разумная цена за полный перехват трафика и как задел под будущую точечную
   фильтрацию/маршрутизацию по приложениям. Права запрашиваются **один раз** при
   установке (ставится фоновый сервис), не при каждом подключении.

## Не цели (явно вне скоупа этой итерации)

- GUI/трей-иконка — пользователь явно отказался, долго писать.
- Автоматизация xray-подписки на Android — на словах "планирую", не в этом дизайне.
- Точечная фильтрация/маршрутизация трафика по приложениям — только фундамент
  (наличие адаптера), сама фича не проектируется здесь.
- Миграция существующих `win-kit2`/`vkturn-linux-kit` профилей — параллельно
  работающий путь, не выключается этой фичей автоматически.

## Архитектура

```
Родственник                      VPS-NL (89.124.71.77)
┌─────────────────┐    HTTPS     ┌───────────────────────────┐
│ vkturn-desktop   │─────login───▶│ vkturn-ios-portal          │
│ (терминал, Go)   │◀───config────│  /api/v1/login              │
│                  │              │  /api/v1/config             │
│  ┌────────────┐  │              │  (bcrypt + HMAC-сессия,     │
│  │ меню:      │  │              │   уже есть; +2 роута)       │
│  │ 1 vk-turn  │──┼──in-process──▶ internal/provider/hub …      │
│  │ 2 xray-sub │──┼──spawn xray──▶ x-ui subscription (:443)     │
│  │ 3 WG full  │──┼──IPC─────────▶ фоновый сервис (см. ниже)    │
│  └────────────┘  │              └───────────────────────────┘
└──────┬───────────┘
       │ (режим 3 only)
       ▼
┌──────────────────┐
│ vkturn-desktopd    │  ставится once через инсталлятор с правами
│ (Windows Service / │  (UAC/sudo один раз), владеет WG-адаптером
│  systemd unit)      │  (wireguard-go + wintun/tun), IPC-сокет
└──────────────────┘
```

`panel.js` пополняется `device=desktop`: по SSH вызывает расширенный
`vkturn-ios-portal adduser` с новыми полями (аккаунты, streams, splitMode,
xray subId), как уже делает для iOS.

## Сервер: `vkturn-ios-portal`

### `Config` (новые поля)
```go
HubToken string `json:"hub_token"` // то же значение, что S.hubToken в panel-secrets.json
HubPin   string `json:"hub_pin"`
```
Единые на все профили (как сейчас в panel.js) — не за-пользователя.

### `User` (новые опциональные поля, заполняются только при `device=desktop`)
```go
DesktopAccounts []int  `json:"desktop_accounts,omitempty"` // hub-порты (8445/8446/…)
DesktopStreams  int    `json:"desktop_streams,omitempty"`
SplitMode       string `json:"split_mode,omitempty"`
XraySubID       string `json:"xray_sub_id,omitempty"` // subId уже существующего x-ui client
```

### Новые роуты
- `POST /api/v1/login` — `{username, password}` → `{token, expires_at}`. Тот же
  bcrypt-чек, что у веб-логина; токен — тот же HMAC-механизм
  (`signSession`/`verifySession`), просто отдаётся как bearer вместо cookie.
- `GET /api/v1/config` (заголовок `Authorization: Bearer <token>`) → JSON:
  ```json
  {
    "hubUrls": ["https://89.124.71.77:8446/turn-creds"],
    "hubPin": "...", "hubToken": "...",
    "peer": "89.124.71.77:56005",
    "obfProfile": "rtpopus3", "obfKey": "...",
    "streams": 10, "splitMode": "exclude",
    "xraySubscriptionUrl": "https://89.124.71.77:2096/sub/<subId>"
  }
  ```
  Собирается из `User.DesktopAccounts`/`DesktopStreams`/`SplitMode`/`XraySubID` +
  глобальных `cfg.HubToken`/`cfg.HubPin`/`cfg.ObfKeyHex`/`cfg.PeerAddress` — то же
  наполнение, что сегодня руками пишет `panel.js`'s `genArtifact()` для
  `device=linux`, просто отдаётся по сети вместо файла.
- Существующие `/login`, `/generate` (веб-форма для iOS) не трогаются.

### `cliAddUser` / `panel.js`
`cliAddUser` получает необязательные флаги для desktop-полей; `panel.js`
добавляет `device=desktop` в `<select>`, вызывает `vkturn-ios-portal adduser
<slug> <account-label> -desktop-accounts=... -desktop-streams=... -xray-subid=...`
по SSH, как уже делает для iOS. Значение `xraySubId` для первой версии
проставляется вручную (создание клиента в 3x-ui — отдельный ручной шаг, не
автоматизируется в этой итерации — как и на Android, "автоматика" отдельно).

## Клиент: `cmd/desktop` (новый, в этом репозитории)

Один Go-бинарник, кросс-компилируется наравне с `cmd/client`/`cmd/server` через
существующий `.goreleaser.yaml` (добавить `id: desktop`, таргеты
`windows_amd64`/`linux_amd64`).

### Логин и кеш конфига
Первый запуск — приглашение логин/пароль в терминале, `POST /api/v1/login` →
`GET /api/v1/config`. Результат кешируется в `~/.vkturn/config.json` (`0600`,
без шифрования — тот же уровень, что панель сейчас даёт `config.bat`/`.env` на
диске родственника). Последующие запуски используют кеш; пункт меню
"обновить конфиг" форсирует повторный логин.

### Терминальное меню
Стрелки + Enter, без стороннего TUI-фреймворка — сырой режим терминала
(`golang.org/x/term`, уже наверняка есть в go.sum транзитивно, либо тривиальный
raw-mode свитч на \~30 строк). Пункты: **vk-turn** / **xray-подписка** / **WG
full-tunnel** (если сервис установлен) / **обновить конфиг** / **выход**.

### Режим 1 — vk-turn
Никакого отдельного процесса: вызывает тот же код, что `cmd/client` строит из
`internal/provider/hub`, в этом же процессе, с параметрами из закешированного
конфига (`hubUrls`/`hubPin`/`hubToken`/`peer`/`obfProfile`/`obfKey`/`streams`).
Ctrl+C — как сейчас у `cmd/client`, штатное завершение по контексту.

### Режим 2 — xray по подписке
`github.com/xtls/libXray`'s конвертер share-ссылок (тот же алгоритм, что
`XraySubscriptionFetcher.kt` на Android, без JNI-обвязки — чистый Go-вызов) на
тело `xraySubscriptionUrl`; полученный xray JSON пишется во временный файл;
клиент спавнит `xray run -c <tmp>` подпроцессом (бинарь `xray` рядом с
`vkturn-desktop`, как сегодня `xray.exe` рядом с `ftp-client.exe`), включает
системный SOCKS-прокси (тот же реестровый твик на Windows, что в `Connect.bat`;
на Linux — переменные `http_proxy`/`all_proxy` окружения для дочерних процессов
плюс `gsettings`/`nmcli` там, где применимо — точный механизм уточняется в плане
реализации, не критичен для дизайна). Выключается при выборе другого пункта меню
или выходе — `taskkill`/`SIGTERM` дочернего `xray`, откат прокси-настроек.

### Режим 3 — WG full-tunnel
Требует, чтобы `vkturn-desktopd` (фоновый сервис) уже был установлен —
если нет, меню предлагает команду установки (`vkturn-desktop install`, спрашивает
права один раз: `UAC` на Windows / `sudo` на Linux). Сервис:
- Владеет постоянным WG-адаптером (`wireguard-go` + `wintun.dll` на Windows,
  `/dev/net/tun` на Linux — тот же движок `golang.zx2c4.com/wireguard`, что уже
  используется в мобильной сборке `mobile/mobile.go` как `libwg-go.so`, теперь
  напрямую как Go-модуль, без cgo-моста).
- IPC — Unix-сокет (Linux) / named pipe (Windows), только `connect`/
  `disconnect`/`status` — минимальный протокол, не общий "универсальный RPC".
- Peer/адрес выдаёт тот же `/api/v1/config` (добавить `wgServerPubkey`/`wgPeer`
  поля — сервис делает `swapPeer`-эквивалент на портале при первом WG-connect,
  переиспользуя уже существующие `wgGenKeypair`/`swapPeer` из
  `vkturn-ios-portal`, только дергает их из нового API, не из веб-формы).
- Обычный (непривилегированный) `vkturn-desktop` процесс шлёт команды сервису по
  IPC — сам никогда не трогает адаптер напрямую, прав не просит.

Эта архитектура (непривилегированный передний план + привилегированный процесс
с адаптером, тонкий IPC) — тот же паттерн, что только что закрыл P1 на Android
(`RealityVpnService` в отдельном `:reality`-процессе + Messenger IPC к основному
процессу) — не новое изобретение, повтор уже проверенного на практике решения.

## Фазы поставки

1. **Milestone A** — логин, кеш конфига, меню, режимы vk-turn + xray-подписка.
   Без прав администратора, без сервиса — можно раздать родне сразу после
   готовности серверной части.
2. **Milestone B** — WG full-tunnel: `vkturn-desktopd`, инсталлятор,
   `swapPeer`-путь через API, третий пункт меню.

## Безопасность

- `/api/v1/*` — тот же bcrypt/HMAC барьер, что уже проверен на iOS-портале живым
  трафиком; не новый непроверенный механизм.
- `hubToken`/`hubPin` теперь физически покидают `panel-secrets.json` через сеть
  (пусть и по TLS, за аутентификацией) — раньше это был только SSH-доступ к
  loopback-панели. Приемлемо: тот же секрет уже сегодня разъезжается по родне в
  виде `config.bat`/`.env`-файлов на их дисках; сетевая доставка не расширяет
  экспозицию, только убирает шаг ручного копирования.
- `~/.vkturn/config.json` на машине родственника хранит те же секреты, что
  сейчас лежат в `config.bat`/`config.env` — не новый прецедент.
- WG-сервис (Milestone B) слушает IPC только на loopback/локальном
  сокете+ACL — не поднимает сетевой порт.

## Тестирование

- Сервер: `POST /api/v1/login` с неверным паролем → 401; с верным → токен;
  `GET /api/v1/config` без токена → 401; с токеном → ожидаемый JSON, значения
  сверены с тем, что сегодня генерит `panel.js` для того же аккаунта.
- Клиент, Milestone A: одноразовый тест-профиль (как P5) — логин → меню →
  vk-turn режим поднимает туннель (сверить исходящий IP) → xray-режим поднимает
  SOCKS (сверить исходящий IP через SOCKS) → выход гасит оба чисто (`ps`/
  `netstat` после выхода пуст).
- Клиент, Milestone B: живой цикл connect/disconnect WG-режима на тестовом
  устройстве, аналогично живой проверке P1 на Android — нет зависших
  WG-пиров на сервере после disconnect (`wg show`, как в P5).
