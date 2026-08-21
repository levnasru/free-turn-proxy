# VK-TURN Desktop TUN-режим — дизайн

Дата: 2026-08-21. Статус: одобрено на словах пользователем в чате, ждёт review этого файла.

## Контекст

Сейчас `vkturn-desktop` в режиме `vk-turn` поднимает `client` (TURN+bond,
`127.0.0.1:9000`) и `xray` (VLESS-клиент с SOCKS5-инбаундом на `127.0.0.1:1085`).
Родне это неудобно: SOCKS5 нужно руками прописывать в браузере/приложениях, никто
из родственников с этим не разберётся. На Android аналогичная проблема решена
WireGuard-интерфейсом, который захватывает весь трафик устройства — на десктопе
такого не было (см. `docs/superpowers/specs/2026-08-14-desktop-client-design.md`,
где этот пробел уже отмечен как "в перспективе").

### Находка, определившая архитектуру

`xray-core` (уже бандлится в vkturn-desktop) содержит готовый inbound `proxy/tun`
(`github.com/xtls/xray-core/proxy/tun`) — полноценная L3 TUN-реализация:
`tun_windows.go` (через `wintun`, зависимость уже в `go.sum`), `tun_linux.go`,
`tun_darwin.go`, gVisor netstack под капотом. Это тот же механизм, что уже
используется в Android-приложении (`RealityVpnService.kt`, `env.xray.tun.fd`).
Конфиг (`proxy/tun/config.proto`) даёт два поля, которые снимают самую тяжёлую
часть работы:

- `auto_system_routing_table` (`repeated string`) — xray сам добавляет/убирает
  системные маршруты на Windows/Linux/macOS. Ручного `ip route`/`route add`
  (как в `scripts/routes.sh` для WG-сетапов) не нужно.
- `auto_outbounds_interface` (`string`) — xray сам привязывает свои исходящие
  сокеты к указанному физическому интерфейсу, что закрывает главный риск TUN
  (routing loop — интерфейс пытается достучаться до собственного аплинка через
  самого себя).

DNS xray на Linux/macOS не трогает (README `proxy/tun`) — DNS-пакеты пойдут как
обычный проксируемый L3-трафик через `auto_system_routing_table`, отдельной
настройки не требуется, пока используемый DNS-сервер не выпадает из покрытых
маршрутом диапазонов.

## Цель

Добавить в меню `vkturn-desktop` новый пункт `vk-turn (tun)`, который поднимает
xray с `tun`-инбаундом вместо SOCKS5 — весь трафик машины идёт через тоннель без
ручной настройки прокси. Текущий SOCKS5-режим остаётся нетронутым.

## Не-цели

- Не трогаем `internal/proxy/*` (Go-ядро) — client остаётся ровно таким же TCP+bond
  процессом, ничего не меняется ниже xray.
- Не добавляем TUN на mobile/iOS — там уже есть свой путь (Android WG /
  `RealityVpnService`).
- macOS явно не в первой итерации (пользователь подтвердил: Windows+Linux сразу,
  macOS отдельно из-за Apple-подписи/NetworkExtension).

## Архитектура

### Меню

`cmd/desktop/main.go`, `RunMenu`:
```
"vk-turn (socks)"   — текущий режим, переименован для ясности, поведение не меняется
"vk-turn (tun)"      — новый режим
"xray-подписка"
"обновить конфиг"
"выход"
```

### Новый файл `cmd/desktop/tun.go`

`runVKTurnTunMode(ctx, cancel, dir, cfg)` — параллель `runVKTurnMode`, отличия:

1. **Elevation-check.** Linux: `os.Geteuid() == 0`. Windows: проверка токена
   текущего процесса на членство в группе Administrators. Если прав нет —
   self-relaunch тем же бинарником с флагом `-tun-elevated`:
   - Linux: `pkexec <self> -tun-elevated <остальные текущие флаги>`
   - Windows: `ShellExecute` с verb `"runas"` на тот же `os.Executable()`
   Родительский (неэлевированный) процесс ждёт код возврата дочернего и
   завершается с тем же кодом; дочерний просто продолжает штатно с флагом.
   Отказ юзера от UAC/pkexec-диалога → понятная ошибка в консоль, откат в меню,
   остальные режимы не трогаются.

2. **Определение физического интерфейса** (для `auto_outbounds_interface`) —
   разбор дефолт-роута ОС один раз при старте:
   - Linux: `ip route show default` → поле `dev`
   - Windows: `Get-NetRoute -DestinationPrefix 0.0.0.0/0` (или эквивалент через
     `GetBestInterface`/`GetAdaptersAddresses` WinAPI) → имя адаптера

3. **Client** поднимается без изменений (тот же `runClientAndWait`/`waitForListening`
   на `127.0.0.1:9000`, что и в SOCKS-режиме).

4. **Xray-конфиг** — новая функция-конструктор рядом с существующей SOCKS-версией
   (`cmd/desktop/launcher.go` или новый `cmd/desktop/xrayconfig_tun.go`), инбаунд:
   ```json
   {
     "protocol": "tun",
     "settings": {
       "name": "vkturn0",
       "mtu": 1500,
       "autoOutboundsInterface": "<обнаруженный интерфейс>",
       "autoSystemRoutingTable": ["<см. ниже>"]
     }
   }
   ```
   `autoSystemRoutingTable` — не голый `0.0.0.0/0`: список с исключением
   RFC1918/link-local/loopback, той же политикой, что уже проверена в Android
   `RealityVpnService.excludeLanFromAllowedIps`/`PRIVATE_IPV4_CIDRS` — принтер,
   роутер, NAS в локалке остаются доступны напрямую.

## Поток данных

Не меняется ниже xray: client (TURN+bond) → xray VLESS-outbound → сервер, как
сейчас. Меняется только вход: raw IP-пакеты с TUN-интерфейса вместо SOCKS5-запросов
от конкретных приложений.

## Обработка ошибок

- Отказ elevation → сообщение в консоли, возврат в меню, ничего не поднято.
- Xray не смог создать интерфейс (занято имя, на Windows нет `wintun.dll` рядом с
  бинарником) → та же схема, что и в SOCKS-режиме сейчас (`RunXray` возвращает
  ошибку, `runXrayAndReport`/аналог логирует и завершает режим, не роняя весь
  процесс desktop).
- Не удалось определить физический интерфейс (нет дефолт-роута, экзотическая
  сеть) → явная ошибка до старта xray, не пытаемся угадывать.

## Тестирование

- Юнит-тесты на парсинг дефолт-роута (мок вывода `ip route show default` /
  `Get-NetRoute`) и на сборку `autoSystemRoutingTable`-списка (LAN-исключения).
- Elevation-check и relaunch — не юнит-тестируемо без реальных прав; ручная
  проверка на живой Windows/Linux машине (golden path: запуск от обычного юзера →
  UAC/pkexec → тоннель поднимается → интернет идёт через него → LAN-устройства
  остаются доступны).
- Позитив-контроль по методичке проекта: `curl ifconfig.me`/аналог до и после
  поднятия TUN, сравнить IP.

## Открытые риски

- Windows: `wintun.dll` должен лежать рядом с `xray.exe` — нужно добавить в
  сборку/дистрибутив desktop-кита (сейчас туда попадают `client`/`vkturn-desktop`/
  `xray`, дополнительный файл).
- IPv6 не в скоупе первой итерации (README `proxy/tun` требует отдельной
  ручной адресации для v6) — если у кого-то из родни IPv6-only сеть, TUN-режим
  не поможет, откатываться на SOCKS.
- `autoSystemRoutingTable`/`autoOutboundsInterface` — новые поля в довольно свежем
  xray-core, стоит явно проверить их поведение на живом Windows/Linux до релиза,
  README не гарантирует отсутствие багов в этой части (TUN официально помечен как
  "для сетевых профессионалов").
