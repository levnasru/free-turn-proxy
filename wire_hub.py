#!/usr/bin/env python3
"""Вписывает provider=hub в free-turn-proxy. Идемпотентен.

Сам internal/provider/hub/hub.go живёт в репозитории как обычный новый файл -
он не конфликтует при rebase на upstream. Скрипт восстанавливает только врезки
в апстримные файлы, которые rebase затирает.

ТРИ точки врезки, не две: buildProvider существует в двух копиях -
десктопной (cmd/client/main.go) и мобильной (mobile/mobile.go, сборка под
gomobile bind -> dist/mobile.aar). Без второй -provider hub не доедет ни до
какого нативного Android-клиента.

После прогона: gofmt -l . && go build ./... && go test ./...
"""
import re
import sys

CFG = "internal/config/config.go"
DISPATCH = ("cmd/client/main.go", "mobile/mobile.go")


def patch(path, fn):
    with open(path, encoding="utf-8") as f:
        src = f.read()
    out = fn(src)
    if out is None:
        print(f"{path}: уже пропатчен, пропускаю")
        return
    with open(path, "w", encoding="utf-8") as f:
        f.write(out)
    print(f"{path}: OK")


def die(msg):
    sys.exit(f"ЯКОРЬ НЕ НАЙДЕН: {msg} — апстрим сдвинулся, поправь патч")


def cfg_patch(s):
    if "ProviderHub" in s:
        return None

    # 1) имена провайдеров
    a = 'const (\n\tProviderVK = "vk"\n)'
    if a not in s:
        die("const ProviderVK в config.go")
    s = s.replace(a, (
        "const (\n"
        '\tProviderVK  = "vk"\n'
        '\tProviderHub = "hub"\n'
        ")\n\n"
        "// EnvHubToken - имя переменной окружения с Bearer-токеном хаба. Через env, а\n"
        "// не флагом: флаги видны в ps любому пользователю системы.\n"
        'const EnvHubToken = "VKTURN_HUB_TOKEN"'), 1)

    # 2) структура опций
    a = "// ProviderOpts выбирает реализацию provider.Provider."
    if a not in s:
        die("комментарий ProviderOpts")
    s = s.replace(a, (
        '// HubOpts - опции провайдера "hub": готовые креды с доверенного эндпоинта\n'
        "// вместо самостоятельного похода в VK API (клиент не встречает captcha).\n"
        "type HubOpts struct {\n"
        "\tURL   string // -hub-url\n"
        "\tPin   string // -hub-pin: base64 SHA-256 SPKI сертификата хаба\n"
        "\tToken string // Bearer-токен из env VKTURN_HUB_TOKEN (не флаг: виден в ps)\n"
        "}\n\n" + a), 1)
    s = s.replace(
        "\tName string // -provider: vk (default)",
        "\tName string // -provider: vk (default) | hub", 1)

    # 3) поле в структуре Client
    m = re.search(r"^\tProvider\s+ProviderOpts.*$", s, re.M)
    if not m:
        die("поле Provider ProviderOpts в struct Client")
    s = s[:m.end()] + "\n\tHub      HubOpts" + s[m.end():]

    # 4) флаги. -hub-* рядом со -streams-per-cred: оба провайдер-специфичны.
    a = '\tprovider := fs.String("provider", ProviderVK, "источник TURN-creds: vk")'
    if a not in s:
        die("объявление флага -provider")
    s = s.replace(
        a, '\tprovider := fs.String("provider", ProviderVK, "источник TURN-creds: vk | hub")', 1)

    a = ('\tstreamsPerCred := fs.Int("streams-per-cred", defaultStreamsPerCache, '
         '"TURN-потоков на один кеш VK-creds; только -provider vk")')
    if a not in s:
        die("объявление флага -streams-per-cred")
    s = s.replace(a, a + (
        '\n\thubURL := fs.String("hub-url", "", '
        '"URL эндпоинта с готовыми TURN-creds; только -provider hub")'
        '\n\thubPin := fs.String("hub-pin", "", '
        '"base64 SHA-256 SPKI сертификата хаба; только -provider hub")'), 1)

    # 5) заполнение опций (токен из env, чтобы не светить в ps)
    a = ("\t\tVK: VKOpts{\n"
         "\t\t\tStreamsPerCred: *streamsPerCred,\n"
         "\t\t\tManualCaptcha:  *manualCaptcha,\n"
         "\t\t\tPlatform:       Platform(*platform),\n"
         "\t\t},")
    if a not in s:
        die("инициализация VKOpts")
    s = s.replace(a, a + (
        "\n\t\tHub: HubOpts{\n"
        "\t\t\tURL:   *hubURL,\n"
        "\t\t\tPin:   *hubPin,\n"
        "\t\t\tToken: os.Getenv(EnvHubToken),\n"
        "\t\t},"), 1)

    # 6) валидация. Пин и токен обязательны наравне с URL: без пина
    # самоподписанный серт хаба нечем проверить, и креды можно подменить.
    a = ('\tdefault:\n\t\treturn nil, fmt.Errorf("invalid -provider value %q: must be %s", '
         "c.Provider.Name, ProviderVK)")
    if a not in s:
        die("default-ветка валидации -provider")
    s = s.replace(a, (
        "\tcase ProviderHub:\n"
        "\t\t// VK-специфичные проверки (-links, -streams-per-cred, -platform)\n"
        "\t\t// намеренно пропускаются: креды приходят готовыми, в VK API клиент\n"
        "\t\t// не ходит.\n"
        '\t\tif c.Hub.URL == "" {\n'
        '\t\t\treturn nil, errors.New("need -hub-url (required for -provider hub)")\n'
        "\t\t}\n"
        '\t\tif c.Hub.Pin == "" {\n'
        '\t\t\treturn nil, errors.New("need -hub-pin (required for -provider hub)")\n'
        "\t\t}\n"
        '\t\tif c.Hub.Token == "" {\n'
        '\t\t\treturn nil, fmt.Errorf("need %s env var (required for -provider hub)", EnvHubToken)\n'
        "\t\t}\n"
        '\tdefault:\n\t\treturn nil, fmt.Errorf("invalid -provider value %q: must be %s | %s", '
        "c.Provider.Name, ProviderVK, ProviderHub)"), 1)

    # 7) импорт os в отсортированную позицию (иначе gofmt -l ругается)
    s = add_import(s, '"os"')

    # доккомментарий пакета перечисляет домены опций
    s = s.replace(
        "// Опции сгруппированы по доменам (TURN, Obf, Proxy, VK, DNS, Log) - структура\n"
        "// зеркалит концептуальные слои прокси.",
        "// Опции сгруппированы по доменам (TURN, Obf, Proxy, VK, Hub, DNS, Log) -\n"
        "// структура зеркалит концептуальные слои прокси.", 1)
    return s


def add_import(s, imp):
    """Вставляет imp в первую группу import-блока, сохраняя сортировку."""
    if re.search(r"^\t" + re.escape(imp) + r"$", s, re.M):
        return s
    m = re.search(r"^import \(\n((?:\t.*\n|\n)*?)\)\n", s, re.M)
    if not m:
        die("блок import")
    block = m.group(1)
    lines = block.split("\n")
    out, placed = [], False
    for line in lines:
        # Сортируем только внутри первой (stdlib) группы: до первой пустой строки.
        if not placed and line.startswith("\t") and line.strip() > imp:
            out.append("\t" + imp)
            placed = True
        if line == "" and not placed:
            out.append("\t" + imp)
            placed = True
        out.append(line)
    if not placed:
        die(f"не нашлось места для {imp}")
    return s[:m.start(1)] + "\n".join(out) + s[m.end(1):]


def dispatch_patch(s):
    """Врезка case ProviderHub в buildProvider. Текст default-ветки в
    cmd/client/main.go и mobile/mobile.go идентичен, поэтому обработчик один."""
    if "provider/hub" in s:
        return None

    a = '\t"github.com/samosvalishe/free-turn-proxy/internal/provider/multi"'
    if a not in s:
        die("импорт provider/multi")
    # hub сортируется перед multi
    s = s.replace(a, '\t"github.com/samosvalishe/free-turn-proxy/internal/provider/hub"\n' + a, 1)

    a = '\tdefault:\n\t\treturn nil, fmt.Errorf("unknown provider %q", cfg.Provider.Name)'
    if a not in s:
        die("default-ветка buildProvider")
    s = s.replace(a, (
        "\tcase config.ProviderHub:\n"
        "\t\treturn hub.New(hub.Config{\n"
        "\t\t\tURL:     cfg.Hub.URL,\n"
        "\t\t\tPinSPKI: cfg.Hub.Pin,\n"
        "\t\t\tToken:   cfg.Hub.Token,\n"
        "\t\t\tDialer:  dialer,\n"
        "\t\t\tLog:     logger,\n"
        "\t\t})\n" + a), 1)
    return s


patch(CFG, cfg_patch)
for path in DISPATCH:
    patch(path, dispatch_patch)
print("готово — дальше: gofmt -l . && go build ./... && go test ./...")
