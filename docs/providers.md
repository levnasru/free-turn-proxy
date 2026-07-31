# Providers

Источник TURN-реквизитов выбирается флагом `-provider` (default `vk`). Реализации удовлетворяют интерфейс `internal/provider.Provider` и подключаются через `buildProvider` — в `cmd/client/main.go` (desktop) и `mobile/mobile.go` (сборка под `gomobile bind`). Новый провайдер нужно врезать в обе копии, иначе он не доедет до нативных мобильных клиентов.

## Доступные провайдеры

### `vk` (default)

VK Calls API. Перебирает встроенные `app_id/app_secret`, получает короткоживущие (≈10 мин) TURN-creds через 4-шаговый token chain. Solver captcha auto+manual.

**Обязательные флаги:**
- `-link` - VK callroom URL вида `https://vk.ru/call/join/<code>` (нормализуется до join-кода).

**Опциональные:**
- `-streams-per-cred` (default 10) - сколько TURN-стримов делят один кеш креденшалов.
- `-manual-captcha` - пропустить auto-solver, сразу открыть браузер.
- `-platform` (default `desktop`) - класс устройства персоны auth (UA + TLS JA3 + client hints + device; семейство всегда Chrome): `desktop` \| `mobile`.

### `hub`

Готовые TURN-creds с доверенного HTTP-эндпоинта. Клиент в VK API не ходит вовсе — captcha он не встречает, и лимит попыток captcha на него не распространяется. Креды добывает один узел («хаб») и раздаёт всем клиентам.

Доверие к эндпоинту — через SPKI-пин, а не через цепочку CA: у хаба обычно нет публичного имени и он терминирует TLS самоподписанным сертификатом. Авторизация — Bearer-токен.

Ожидаемый ответ:

```json
{"username":"<unix-expiry>:<userId>","password":"...","turn":"host:port"}
```

`turn` принимается и строкой, и массивом (либо отдельным полем `turns`). Все адреса попадают в `Credentials.ServerAddrs`: pipeline перебирает их при неудачном allocate, поэтому список сохраняется целиком, а не схлопывается в первый элемент. Схема `turn:`/`turns:` и хвост `?transport` срезаются.

Срок жизни берётся из `username` (`<unix>:<userId>`); если разобрать не удалось — креды кешируются на 10 минут.

**Обязательные флаги:**
- `-hub-url` - URL эндпоинта, напр. `https://1.2.3.4:8444/turn-creds`.
- `-hub-pin` - base64 SHA-256 отпечатка SubjectPublicKeyInfo сертификата хаба.

**Обязательная переменная окружения:**
- `VKTURN_HUB_TOKEN` - Bearer-токен. Именно env, а не флаг: флаги видны в `ps` любому пользователю системы.

VK-специфичные флаги (`-link`/`-links`, `-streams-per-cred`, `-platform`, `-manual-captcha`) при `-provider hub` не требуются и игнорируются.

Получить пин с работающего хаба:

```bash
openssl s_client -connect 1.2.3.4:8444 </dev/null 2>/dev/null \
  | openssl x509 -pubkey -noout \
  | openssl pkey -pubin -outform der \
  | openssl dgst -sha256 -binary | base64
```

> [!NOTE]
> Хаб не умеет переминчивать креды сам. Если источник на хабе умер, а хаб продолжает отдавать last-good, TURN будет отвечать 401: провайдер сбросит кеш несколько раз и напишет в лог, что хаб раздаёт креды, которые TURN не принимает. Дальше нужен человек.