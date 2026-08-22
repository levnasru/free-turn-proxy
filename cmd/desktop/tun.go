// cmd/desktop/tun.go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// runVKTurnTunMode mirrors runVKTurnMode (main.go) but replaces the local
// SOCKS5 bridge with a system-wide TUN interface: once this reaches the
// "Подключено" state, the OS itself routes all traffic (minus the LAN
// exclusion list from lanexclude.go) through xray — no per-app proxy
// configuration needed. Requires admin/root, requested here (not earlier)
// so picking any other menu item never triggers a UAC/pkexec prompt.
func runVKTurnTunMode(ctx context.Context, cancel context.CancelFunc, cfg *DesktopConfig) {
	if err := checkEnvironment(true); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка запуска:", err)
		return
	}

	if !isElevated() {
		if *tunElevated {
			// Already went through one elevation attempt and still not
			// elevated — a real UAC/pkexec grant would have this process
			// running as admin/root by now. Stop instead of relaunching
			// again, which would otherwise chain into an unbounded sequence
			// of prompts (see also the *tunElevated guard this mirrors).
			fmt.Fprintln(os.Stderr, "Не удалось получить права администратора/root даже после запроса — прекращаю попытки.")
			return
		}
		fmt.Println("Режиму 'vk-turn (tun)' нужны права администратора/root — запрашиваю...")
		if err := relaunchElevated([]string{"-tun-elevated"}); err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось получить права:", err)
			return
		}
		if runtime.GOOS == "windows" {
			// Windows: relaunchElevated returns as soon as the elevated
			// process is launched (fire-and-forget) — it's now running
			// independently in its own console. Falling back to this
			// process's menu would let the user start a second, conflicting
			// session (e.g. vk-turn (socks)) fighting the elevated instance
			// for 127.0.0.1:9000. Exit entirely instead.
			fmt.Println("Запущен отдельный процесс с правами администратора.")
			os.Exit(0)
		}
		// Linux: relaunchElevated already blocked until the elevated child
		// finished — nothing left to do, return to the menu normally.
		return
	}

	iface, err := defaultRouteInterface()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось определить сетевой интерфейс:", err)
		return
	}
	routes := publicRoutes()

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
	go func() { clientDone <- RunClient(ctx, clientBin, cfg, stdout, stderr, "-bind-iface", iface) }()

	fmt.Println("Поднимаю туннель VK-TURN...")
	const clientListenTimeout = 60 * time.Second
	if err := waitForListening(ctx, "127.0.0.1:9000", clientListenTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "Туннель не поднялся:", err)
		cancel()
		<-clientDone
		return
	}

	tunConfig, err := buildVKTurnTunConfig(iface, routes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось собрать конфиг tun:", err)
		cancel()
		<-clientDone
		return
	}

	xrayDone := make(chan error, 1)
	go func() { xrayDone <- RunXray(ctx, xrayBin, tunConfig, stdout, stderr) }()

	// tun mode opens no local port to poll (unlike the SOCKS bridge's
	// waitForListening) — give xray a fixed grace period to create the
	// interface and apply routes before running the connectivity check.
	select {
	case <-time.After(3 * time.Second):
	case err := <-xrayDone:
		fmt.Fprintln(os.Stderr, "xray завершился при поднятии tun:", err)
		cancel()
		<-clientDone
		return
	case <-ctx.Done():
		<-clientDone
		return
	}

	checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
	ip, err := checkDirectConnectivity(checkCtx)
	checkCancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось проверить соединение (туннель может не работать):", err)
	} else {
		fmt.Println("Подключено, выходной IP:", ip)
	}
	fmt.Println("Весь трафик машины теперь идёт через туннель. Ctrl+C — остановить.")

	startTray(ctx, cancel, "Подключено (tun)")
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

// checkDirectConnectivity is checkVKTurnConnectivity's tun-mode counterpart:
// tun mode has no local SOCKS port to dial through deliberately — the OS
// itself now routes a plain HTTP client's connection through the tunnel, so
// this just uses a bare *http.Client with a timeout instead of a SOCKS5
// dialer (no custom Transport/DialContext needed).
func checkDirectConnectivity(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vkTurnIPCheckURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("проверка IP: неожиданный статус %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", errors.New("проверка IP: пустой ответ")
	}
	return ip, nil
}
