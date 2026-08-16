// cmd/desktop/main.go
package main

import (
	"bufio"
	"context"
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

func init() {
	if v := os.Getenv("VKTURN_PORTAL_URL"); v != "" {
		portalBaseURL = v
	}
}

// version is set via -ldflags "-X main.version=..." by the goreleaser
// desktop build entry (same convention as cmd/client/main.go); "dev" for
// local/docker builds that don't pass it.
var version = "dev"

// maxLoginAttempts caps the first-run login retry loop so a typo doesn't
// require restarting the whole program, but a truly wrong password doesn't
// loop forever either.
const maxLoginAttempts = 3

func main() {
	cfg, err := LoadCache()
	if err != nil {
		cfg, err = loginWithRetries()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось войти:", err)
			os.Exit(1)
		}
	}

	for {
		choice, err := RunMenu([]string{"vk-turn", "xray-подписка", "обновить конфиг", "выход"})
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nМеню прервано:", err)
			return
		}
		switch choice {
		case "vk-turn":
			runMode(cfg, "vk-turn")
		case "xray-подписка":
			runMode(cfg, "xray")
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
	return cfg, nil
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
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось определить путь к исполняемому файлу:", err)
		return
	}
	dir := filepath.Dir(exePath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()
	defer signal.Stop(sigCh)

	switch mode {
	case "vk-turn":
		runVKTurnMode(ctx, cancel, dir, cfg)
	case "xray":
		runXraySubscriptionMode(ctx, cancel, dir, cfg)
	}
}

// runVKTurnMode wires up the full vk-turn path: spawn cmd/client in the
// background, wait for its raw-TCP listener to accept, spawn the local
// xray SOCKS<->VLESS bridge (buildVKTurnBridgeConfig) against it, wait for
// the bridge's SOCKS port, then run a positive-control IP-echo check
// through it before telling the user they're connected. Without the
// bridge, client's listener has nothing speaking VLESS to it and the user
// has no usable proxy — see buildVKTurnBridgeConfig's doc comment.
func runVKTurnMode(ctx context.Context, cancel context.CancelFunc, dir string, cfg *DesktopConfig) {
	clientBin, err := resolveClientBin(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден client:", err)
		return
	}
	xrayBin, err := resolveXrayBin(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден xray:", err)
		return
	}

	clientDone := make(chan error, 1)
	go func() {
		clientDone <- RunClient(ctx, clientBin, cfg, os.Stdout, os.Stderr)
	}()

	fmt.Println("Поднимаю туннель VK-TURN...")
	if err := waitForListening(ctx, "127.0.0.1:9000", 5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "Туннель не поднялся:", err)
		cancel()
		<-clientDone
		return
	}

	xrayDone := make(chan error, 1)
	go func() {
		xrayDone <- RunXray(ctx, xrayBin, buildVKTurnBridgeConfig(), os.Stdout, os.Stderr)
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
	fmt.Printf("Прокси: socks5://%s — укажите его в браузере или приложении. Ctrl+C — остановить.\n", socksAddr)

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

func runXraySubscriptionMode(ctx context.Context, cancel context.CancelFunc, dir string, cfg *DesktopConfig) {
	if cfg.XraySubscriptionURL == "" {
		fmt.Fprintln(os.Stderr, "Нет xray-подписки для этого профиля")
		return
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

	xrayBin, err := resolveXrayBin(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден xray:", err)
		return
	}

	startTray(ctx, cancel, "Запущено")
	defer restoreConsole()

	if err := RunXray(ctx, xrayBin, configs[0], os.Stdout, os.Stderr); err != nil {
		reportModeExit(ctx, "xray", err)
	}
}
