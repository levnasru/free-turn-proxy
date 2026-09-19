// cmd/desktop/main.go
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

var portalBaseURL = "https://lft.levnas.ru"

// debugMode, when set via VKTURN_DEBUG, turns on verbose logging across every
// spawned component (client gets -debug, the xray bridge gets loglevel=debug)
// and duplicates all of their combined output into a log file the user can
// hand back for troubleshooting, instead of relying on them to know to
// redirect/tee the terminal themselves.
var debugMode = false

func init() {
	if v := os.Getenv("VKTURN_PORTAL_URL"); v != "" {
		portalBaseURL = v
	}
	if v := os.Getenv("VKTURN_DEBUG"); v != "" && v != "0" {
		debugMode = true
	}
}

// debugLogPath is where the debug log ends up when VKTURN_DEBUG is set —
// same directory convention as CachePath's config.json.
func debugLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".vkturn", "debug.log"), nil
}

// openOutputs returns the writers RunClient/RunXray should log to. Under
// VKTURN_DEBUG it also appends everything to debugLogPath() (truncated fresh
// each run, so it always holds just the latest attempt) via io.MultiWriter,
// and returns a close func to flush it; without VKTURN_DEBUG it's a no-op
// passthrough to stdout/stderr.
func openOutputs() (stdout, stderr io.Writer, closeFn func(), err error) {
	if !debugMode {
		return os.Stdout, os.Stderr, func() {}, nil
	}
	path, perr := debugLogPath()
	if perr != nil {
		return nil, nil, nil, perr
	}
	if merr := os.MkdirAll(filepath.Dir(path), 0o700); merr != nil {
		return nil, nil, nil, merr
	}
	chownToOriginalUserIfElevated(filepath.Dir(path))
	f, oerr := os.Create(path)
	if oerr != nil {
		return nil, nil, nil, oerr
	}
	chownToOriginalUserIfElevated(path)
	fmt.Println("VKTURN_DEBUG включён — подробный лог пишется в", path)
	return io.MultiWriter(os.Stdout, f), io.MultiWriter(os.Stderr, f), func() { _ = f.Close() }, nil
}

// version is set via -ldflags "-X main.version=..." by the goreleaser
// desktop build entry (same convention as cmd/client/main.go); "dev" for
// local/docker builds that don't pass it.
var version = "dev"

// maxLoginAttempts caps the first-run login retry loop so a typo doesn't
// require restarting the whole program, but a truly wrong password doesn't
// loop forever either.
const maxLoginAttempts = 3

// tunElevated is set by relaunchElevated (elevate_linux.go/elevate_windows.go)
// when re-execing this binary with a UAC/pkexec prompt already granted —
// skips straight to vk-turn (tun) instead of showing the interactive menu
// again in the elevated process.
var tunElevated = flag.Bool("tun-elevated", false,
	"внутренний флаг: пропустить меню, сразу поднять tun (используется relaunchElevated)")

var tunModeFlag = flag.String("tun-mode", "wg",
	"тип tun: wg | xray (используется с -tun-elevated или -mode tun)")

var modeFlag = flag.String("mode", "",
	"режим работы: socks | wg-tun | xray-tun | xray (пропускает меню)")

var configPathFlag = flag.String("config", "",
	"путь к файлу config.json (используется при повышении прав или ручном запуске)")

var debugFlag = flag.Bool("debug", false,
	"включить подробную отладку (verbose debug log)")

func main() {
	flag.Parse()
	if *debugFlag {
		debugMode = true
	}

	var cfg *DesktopConfig
	var err error
	if *configPathFlag != "" {
		cfg, err = LoadCacheFrom(*configPathFlag)
	} else {
		cfg, err = LoadCache()
	}
	if err != nil {
		cfg, err = loginWithRetries()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось войти:", err)
			os.Exit(1)
		}
	}

	if *tunElevated {
		if *tunModeFlag == "xray" {
			runMode(cfg, "vk-turn-xray-tun")
		} else {
			runMode(cfg, "vk-turn-wg-tun")
		}
		return
	}

	if *modeFlag != "" {
		switch *modeFlag {
		case "socks", "vk-turn":
			runMode(cfg, "vk-turn")
			return
		case "wg", "wg-tun", "vk-turn-wg-tun":
			runMode(cfg, "vk-turn-wg-tun")
			return
		case "xray-tun", "vk-turn-xray-tun":
			runMode(cfg, "vk-turn-xray-tun")
			return
		case "tun", "vk-turn-tun":
			if *tunModeFlag == "xray" {
				runMode(cfg, "vk-turn-xray-tun")
			} else {
				runMode(cfg, "vk-turn-wg-tun")
			}
			return
		case "xray":
			runMode(cfg, "xray")
			return
		default:
			fmt.Fprintf(os.Stderr, "Неизвестный режим %q (доступны: socks, wg-tun, xray-tun, xray)\n", *modeFlag)
			os.Exit(1)
		}
	}

	for {
		debugLabel := "подробная отладка (debug: выкл)"
		if debugMode {
			debugLabel = "подробная отладка (debug: ВКЛ)"
		}
		choice, err := RunMenu([]string{
			"vk-turn (wg tun)",
			"vk-turn (xray tun)",
			"vk-turn (socks)",
			"xray-подписка",
			"сайты мимо туннеля (direct)",
			debugLabel,
			"обновить конфиг",
			"выход",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nМеню прервано:", err)
			return
		}
		switch choice {
		case "vk-turn (wg tun)":
			runMode(cfg, "vk-turn-wg-tun")
		case "vk-turn (xray tun)":
			runMode(cfg, "vk-turn-xray-tun")
		case "vk-turn (socks)":
			runMode(cfg, "vk-turn")
		case "xray-подписка":
			runMode(cfg, "xray")
		case "сайты мимо туннеля (direct)":
			manageBypassRules(cfg)
		case "подробная отладка (debug: выкл)":
			debugMode = true
			fmt.Println("\nРежим подробной отладки ВКЛЮЧЕН (логи дублируются на экран и в ~/.vkturn/debug.log).")
		case "подробная отладка (debug: ВКЛ)":
			debugMode = false
			fmt.Println("\nРежим отладки ВЫКЛЮЧЕН.")
		case "обновить конфиг":
			refreshed, err := loginFlow()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Не удалось обновить конфиг:", err)
				continue
			}
			cfg = refreshed
		case "выход":
			return
		}
	}
}

func manageBypassRules(cfg *DesktopConfig) {
	reader := bufio.NewReader(os.Stdin)
	for {
		entries, _ := readRawUserBypassEntries()
		domains, ips := collectBypassRules(cfg)
		path, _ := bypassFilePath()

		fmt.Println("\n=== Настройка сайтов и IP мимо туннеля (Whitelist / Direct) ===")
		fmt.Printf("Файл правил: %s\n", path)
		fmt.Printf("Всего активно: %d доменов, %d IP/подсетей (включая дефолтные сервисы РФ: Госуслуги, VK, банки, Яндекс)\n", len(domains), len(ips))
		fmt.Printf("Пользовательских правил в файле (%d):\n", len(entries))
		if len(entries) == 0 {
			fmt.Println("  (список пуст — действуют только встроенные правила)")
		} else {
			for i, e := range entries {
				fmt.Printf("  %d. %s\n", i+1, e)
			}
		}

		choice, err := RunMenu([]string{
			"добавить сайт или IP/подсеть",
			"удалить правило",
			"открыть файл в текстовом редакторе",
			"очистить пользовательский список",
			"назад в главное меню",
		})
		if err != nil {
			return
		}

		switch choice {
		case "добавить сайт или IP/подсеть":
			fmt.Print("\nВведите домен (например, ozon.ru) или IP/CIDR (например, 195.82.146.120 или 95.163.0.0/16): ")
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				if err := addUserBypassEntry(line); err != nil {
					fmt.Fprintln(os.Stderr, "Ошибка добавления:", err)
				} else {
					fmt.Printf("✓ Добавлено: %s\n", line)
				}
			}
		case "удалить правило":
			if len(entries) == 0 {
				fmt.Println("\nСписок пуст, нечего удалять.")
				continue
			}
			fmt.Printf("\nВведите номер правила для удаления (1..%d): ", len(entries))
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line)
			var idx int
			if _, err := fmt.Sscanf(line, "%d", &idx); err == nil && idx >= 1 && idx <= len(entries) {
				if err := removeUserBypassEntry(idx); err != nil {
					fmt.Fprintln(os.Stderr, "Ошибка удаления:", err)
				} else {
					fmt.Printf("✓ Удалено правило #%d (%s)\n", idx, entries[idx-1])
				}
			} else {
				fmt.Println("Неверный номер.")
			}
		case "открыть файл в текстовом редакторе":
			if err := openInSystemEditor(path); err != nil {
				fmt.Fprintf(os.Stderr, "Не удалось открыть редактор: %v. Вы можете отредактировать %s вручную.\n", err, path)
			} else {
				fmt.Println("✓ Файл открыт в системном редакторе.")
			}
		case "очистить пользовательский список":
			fmt.Print("\nТочно очистить все пользовательские правила? (y/N): ")
			line, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) == "y" {
				if err := clearUserBypassList(); err != nil {
					fmt.Fprintln(os.Stderr, "Ошибка очистки:", err)
				} else {
					fmt.Println("✓ Пользовательский список очищен.")
				}
			}
		case "назад в главное меню":
			return
		}
	}
}


// loginWithRetries runs loginFlow up to maxLoginAttempts times, looping
// back to the prompt on failure (e.g. a mistyped password) instead of
// exiting the whole program after a single bad attempt.
func loginWithRetries() (*DesktopConfig, error) {
	var lastErr error
	for attempt := 1; attempt <= maxLoginAttempts; attempt++ {
		cfg, err := loginFlow()
		if err == nil {
			return cfg, nil
		}
		lastErr = err
		fmt.Fprintln(os.Stderr, "Ошибка входа:", err)
		if attempt < maxLoginAttempts {
			fmt.Println("Попробуйте ещё раз.")
		}
	}
	return nil, lastErr
}

func loginFlow() (*DesktopConfig, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Логин: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)

	fmt.Print("Пароль: ")
	passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println() // ReadPassword doesn't echo a newline itself
	if err != nil {
		return nil, fmt.Errorf("чтение пароля: %w", err)
	}
	password := strings.TrimSpace(string(passwordBytes))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	token, err := Login(ctx, portalBaseURL, username, password)
	if err != nil {
		return nil, err
	}
	cfg, err := FetchConfig(ctx, portalBaseURL, token)
	if err != nil {
		return nil, err
	}
	if err := SaveCache(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "Внимание: не удалось сохранить кеш конфига:", err)
	}
	_ = SaveSession(&SessionInfo{
		BaseURL:   portalBaseURL,
		Token:     token,
		ExpiresAt: time.Now().Add(29 * 24 * time.Hour).Unix(),
	})
	return cfg, nil
}

// ensureFreshConfig silently checks if the hub has updated endpoints or tokens,
// updating cache in background with a 5s timeout.
// Preserves local user settings (direct domains, direct IPs, subscription URL).
func ensureFreshConfig(ctx context.Context, currentCfg *DesktopConfig) *DesktopConfig {
	sess, err := LoadSession()
	if err != nil || sess.Token == "" {
		return currentCfg
	}
	syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	fresh, err := FetchConfig(syncCtx, sess.BaseURL, sess.Token)
	if err != nil {
		return currentCfg
	}
	if len(currentCfg.DirectDomains) > 0 && len(fresh.DirectDomains) == 0 {
		fresh.DirectDomains = currentCfg.DirectDomains
	}
	if len(currentCfg.DirectIPs) > 0 && len(fresh.DirectIPs) == 0 {
		fresh.DirectIPs = currentCfg.DirectIPs
	}
	if currentCfg.XraySubscriptionURL != "" && fresh.XraySubscriptionURL == "" {
		fresh.XraySubscriptionURL = currentCfg.XraySubscriptionURL
	}
	if currentCfg.WgConfig != "" && fresh.WgConfig == "" {
		fresh.WgConfig = currentCfg.WgConfig
		fresh.WgPeer = currentCfg.WgPeer
	}
	if currentCfg.Streams > fresh.Streams {
		fresh.Streams = currentCfg.Streams
	}
	_ = SaveCache(fresh)
	return fresh
}

// promptXraySubscriptionURL asks for a subscription link when the portal
// profile doesn't already have one cached (cfg.XraySubscriptionURL empty) —
// the "нет плашки добавления" gap: previously this mode just dead-ended
// with an error instead of offering any way to attach one. Saved into the
// same local cache loginFlow writes to, so it only needs entering once.
func promptXraySubscriptionURL() (string, error) {
	fmt.Print("URL xray-подписки (Enter — пропустить): ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// reportModeExit prints the outcome of a subprocess that just exited. A
// normal Ctrl+C / menu-driven stop cancels ctx and then kills the child,
// so ctx.Err() != nil at that point means "we asked for this" — print a
// plain "Остановлено." instead of the underlying "signal: killed" style
// error, which reads like a crash to a non-technical user. Only a
// genuinely unexpected exit (ctx still live) prints the raw error.
func reportModeExit(ctx context.Context, label string, err error) {
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		fmt.Println("Остановлено.")
		return
	}
	fmt.Fprintf(os.Stderr, "%s завершился с ошибкой: %v\n", label, err)
}

func runMode(cfg *DesktopConfig, mode string) {
	cfg = ensureFreshConfig(context.Background(), cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	stopSig := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-stopSig:
		}
	}()
	defer func() {
		signal.Stop(sigCh)
		close(stopSig)
	}()

	switch mode {
	case "vk-turn":
		runVKTurnMode(ctx, cancel, cfg)
	case "vk-turn-wg", "vk-turn-wg-tun":
		runVKTurnTunMode(ctx, cancel, cfg, "wg")
	case "vk-turn-tun", "vk-turn-xray-tun":
		runVKTurnTunMode(ctx, cancel, cfg, "xray")
	case "xray":
		runXraySubscriptionMode(ctx, cancel, cfg)
	}
}

// runVKTurnMode wires up the full vk-turn path: spawn cmd/client in the
// background, wait for its raw-TCP listener to accept, spawn the local
// xray SOCKS<->VLESS bridge (buildVKTurnBridgeConfig) against it, wait for
// the bridge's SOCKS port, then run a positive-control IP-echo check
// through it before telling the user they're connected. Without the
// bridge, client's listener has nothing speaking VLESS to it and the user
// has no usable proxy — see buildVKTurnBridgeConfig's doc comment.
func runVKTurnMode(ctx context.Context, cancel context.CancelFunc, cfg *DesktopConfig) {
	if err := checkEnvironment(false, []int{vkTurnLocalSocksPort, vkTurnClientListenPort}); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка запуска:", err)
		return
	}

	bypassDomains, bypassIPs := collectBypassRules(cfg)

	clientBin, err := resolveClientBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден client:", err)
		return
	}
	xrayBin, err := resolveXrayBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден xray:", err)
		return
	}

	stdout, stderr, closeOutputs, err := openOutputs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось открыть debug-лог:", err)
		return
	}
	defer closeOutputs()

	clientDone := make(chan error, 1)
	go func() {
		clientDone <- RunClient(ctx, clientBin, cfg, stdout, stderr)
	}()

	fmt.Println("Поднимаю туннель VK-TURN...")
	// clientListenTimeout покрывает реальный бюджет первой TURN-сессии:
	// hub.go httpTimeout=15s (получение кредов) + dtlsdial HandshakeTimeout=30s
	// для tcp+bond (cmd/client/main.go) — до этого client вообще не открывает
	// -listen (см. tcpfwd.Run: Listen идёт только после pool.Ready(), т.е.
	// после первой успешной сессии). 5s отваливался раньше, чем успевал
	// пройти даже штатный первый коннект — не диагностика, а баг таймаута.
	const clientListenTimeout = 60 * time.Second
	if err := waitForListening(ctx, fmt.Sprintf("127.0.0.1:%d", vkTurnClientListenPort), clientListenTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "Туннель не поднялся:", err)
		cancel()
		<-clientDone
		return
	}

	xrayDone := make(chan error, 1)
	go func() {
		xrayDone <- RunXray(ctx, xrayBin, buildVKTurnBridgeConfig(bypassDomains, bypassIPs), stdout, stderr)
	}()

	socksAddr := fmt.Sprintf("127.0.0.1:%d", vkTurnLocalSocksPort)
	if err := waitForListening(ctx, socksAddr, 5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "Локальный SOCKS-мост не поднялся:", err)
		cancel()
		<-clientDone
		<-xrayDone
		return
	}

	checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
	ip, err := checkVKTurnConnectivity(checkCtx, socksAddr)
	checkCancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось проверить соединение (туннель может не работать):", err)
	} else {
		fmt.Println("Подключено, выходной IP:", ip)
	}
	fmt.Printf("Прокси: socks5://%s — укажите его в браузере или приложении (исключено доменов: %d, IP/подсетей: %d). Ctrl+C — остановить.\n", socksAddr, len(bypassDomains), len(bypassIPs))

	startTray(ctx, cancel, "Подключено")
	defer restoreConsole()

	select {
	case err := <-clientDone:
		reportModeExit(ctx, "client", err)
		cancel()
		<-xrayDone
	case err := <-xrayDone:
		reportModeExit(ctx, "xray", err)
		cancel()
		<-clientDone
	}
}

func runXraySubscriptionMode(ctx context.Context, cancel context.CancelFunc, cfg *DesktopConfig) {
	if cfg.XraySubscriptionURL == "" {
		url, err := promptXraySubscriptionURL()
		if err != nil || url == "" {
			fmt.Fprintln(os.Stderr, "Нет xray-подписки для этого профиля")
			return
		}
		cfg.XraySubscriptionURL = url
		if err := SaveCache(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "Внимание: не удалось сохранить подписку в кеш:", err)
		}
	}

	fetchCtx, fetchCancel := context.WithTimeout(ctx, 20*time.Second)
	defer fetchCancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, cfg.XraySubscriptionURL, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось собрать запрос подписки:", err)
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось загрузить подписку:", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Не удалось загрузить подписку: неожиданный статус %d\n", resp.StatusCode)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB cap
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось прочитать подписку:", err)
		return
	}

	configs, err := convertSubscription(string(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось разобрать конфиги из подписки:", err)
		return
	}
	if len(configs) == 0 {
		fmt.Fprintln(os.Stderr, "В подписке нет пригодных xray-конфигов")
		return
	}
	fmt.Printf("Выбран конфиг 1 из %d.\n", len(configs))

	xrayBin, err := resolveXrayBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден xray:", err)
		return
	}

	stdout, stderr, closeOutputs, err := openOutputs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось открыть debug-лог:", err)
		return
	}
	defer closeOutputs()

	startTray(ctx, cancel, "Запущено")
	defer restoreConsole()

	if err := RunXray(ctx, xrayBin, configs[0], stdout, stderr); err != nil {
		reportModeExit(ctx, "xray", err)
	}
}
