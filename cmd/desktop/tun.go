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
// configuration needed. Supports both "wg" (WireGuard UDP) and "xray" (VLESS TCP).
func runVKTurnTunMode(ctx context.Context, cancel context.CancelFunc, cfg *DesktopConfig, tunType string) {
	if tunType == "" {
		tunType = "wg"
	}

	if err := checkEnvironment(true, []int{vkTurnClientListenPort}); err != nil {
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
		modeLabel := "vk-turn (wg tun)"
		if tunType == "xray" {
			modeLabel = "vk-turn (xray tun)"
		}
		fmt.Printf("Режиму '%s' нужны права администратора/root — запрашиваю...\n", modeLabel)
		if err := relaunchElevated([]string{"-tun-elevated", "-tun-mode=" + tunType}); err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось получить права:", err)
			return
		}
		if runtime.GOOS == "windows" {
			fmt.Println("Запущен отдельный процесс с правами администратора.")
			os.Exit(0)
		}
		return
	}

	var wgParsed *WGConfigParsed
	if tunType == "wg" {
		if cfg.WgConfig == "" {
			if sess, serr := LoadSession(); serr == nil && sess.Token != "" {
				syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
				if fresh, ferr := FetchConfig(syncCtx, sess.BaseURL, sess.Token); ferr == nil && fresh != nil && fresh.WgConfig != "" {
					cfg.WgConfig = fresh.WgConfig
					cfg.WgPeer = fresh.Peer
					_ = SaveCache(cfg)
				}
				syncCancel()
			}
		}
		if cfg.WgConfig == "" {
			fmt.Fprintln(os.Stderr, "WireGuard-конфиг не найден на портале для этого пользователя. Переключаюсь на xray tun...")
			tunType = "xray"
		} else {
			parsed, perr := parseWGConfig(cfg.WgConfig)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "Ошибка WireGuard-конфига: %v. Переключаюсь на xray tun...\n", perr)
				tunType = "xray"
			} else {
				wgParsed = parsed
				if *mtuFlag > 0 {
					wgParsed.MTU = *mtuFlag
				}
			}
		}
	}

	iface, err := defaultRouteInterface()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось определить сетевой интерфейс:", err)
		return
	}
	bypassDomains, bypassIPs := collectBypassRules(cfg)
	if tunType == "wg" {
		if domainIPs := resolveBypassDomainsToIPs(bypassDomains); len(domainIPs) > 0 {
			bypassIPs = append(bypassIPs, domainIPs...)
		}
	}
	routes := publicRoutesWithExclusions(bypassIPs)

	clientBin, err := resolveClientBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден client:", err)
		return
	}

	stdout, stderr, closeOutputs, err := openOutputs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось открыть debug-лог:", err)
		return
	}
	defer closeOutputs()

	clientDone := make(chan error, 1)
	const clientListenTimeout = 60 * time.Second

	if tunType == "wg" {
		go func() { clientDone <- RunClientWG(ctx, clientBin, cfg, stdout, stderr, "-bind-iface", iface) }()
		fmt.Println("Поднимаю нативный WireGuard туннель LFT (UDP)...")
		if err := waitForVKTurnUDPReady(ctx, vkTurnClientListenPort, clientListenTimeout); err != nil {
			fmt.Fprintln(os.Stderr, "Туннель LFT не поднялся:", err)
			cancel()
			<-clientDone
			return
		}

		stopWG, err := startNativeWGTunnel(ctx, wgParsed, routes, iface)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось поднять интерфейс WireGuard:", err)
			cancel()
			<-clientDone
			return
		}
		defer stopWG()

		checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
		ip, err := checkDirectConnectivity(checkCtx)
		checkCancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось проверить соединение (туннель может не работать):", err)
		} else {
			fmt.Println("Подключено, выходной IP:", ip)
		}
		fmt.Printf("Весь трафик машины теперь идёт через нативный WireGuard туннель (исключено доменов: %d, IP/подсетей: %d). Ctrl+C — остановить.\n", len(bypassDomains), len(bypassIPs))

		startTray(ctx, cancel, "Подключено (wg tun)")
		defer restoreConsole()

		select {
		case err := <-clientDone:
			reportModeExit(ctx, "client", err)
			cancel()
		case <-ctx.Done():
			reportModeExit(ctx, "client", nil)
			<-clientDone
		}
		return
	}

	xrayBin, err := resolveXrayBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден xray:", err)
		return
	}

	go func() { clientDone <- RunClient(ctx, clientBin, cfg, stdout, stderr, "-bind-iface", iface) }()
	fmt.Println("Поднимаю Xray туннель LFT (TCP)...")
	if err := waitForListening(ctx, fmt.Sprintf("127.0.0.1:%d", vkTurnClientListenPort), clientListenTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "Туннель не поднялся:", err)
		cancel()
		<-clientDone
		return
	}
	tc, err := buildVKTurnTunConfig(iface, routes, bypassDomains, bypassIPs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось собрать конфиг tun:", err)
		cancel()
		<-clientDone
		return
	}
	tunConfig := tc

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
		cancel()
		<-clientDone
		<-xrayDone
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
	fmt.Printf("Весь трафик машины теперь идёт через туннель (исключено доменов: %d, IP/подсетей: %d). Ctrl+C — остановить.\n", len(bypassDomains), len(bypassIPs))

	startTray(ctx, cancel, "Подключено (xray tun)")
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
