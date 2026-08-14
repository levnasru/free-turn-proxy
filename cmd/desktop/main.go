// cmd/desktop/main.go
package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const portalBaseURL = "https://lft.levnas.ru"

// version is set via -ldflags "-X main.version=..." by the goreleaser
// desktop build entry (same convention as cmd/client/main.go); "dev" for
// local/docker builds that don't pass it.
var version = "dev"

func main() {
	cfg, err := LoadCache()
	if err != nil {
		cfg, err = loginFlow()
		if err != nil {
			fmt.Fprintln(os.Stderr, "login failed:", err)
			os.Exit(1)
		}
	}

	for {
		choice, err := RunMenu([]string{"vk-turn", "xray-подписка", "обновить конфиг", "выход"})
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nmenu:", err)
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
				fmt.Fprintln(os.Stderr, "refresh failed:", err)
				continue
			}
			cfg = refreshed
		case "выход":
			return
		}
	}
}

func loginFlow() (*DesktopConfig, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Логин: ")
	username, _ := reader.ReadString('\n')
	fmt.Print("Пароль: ")
	password, _ := reader.ReadString('\n') // Milestone A: echoed; term.ReadPassword can replace this later
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)

	ctx := context.Background()
	token, err := Login(ctx, portalBaseURL, username, password)
	if err != nil {
		return nil, err
	}
	cfg, err := FetchConfig(ctx, portalBaseURL, token)
	if err != nil {
		return nil, err
	}
	if err := SaveCache(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not cache config:", err)
	}
	return cfg, nil
}

func runMode(cfg *DesktopConfig, mode string) {
	exeDir, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve executable path:", err)
		return
	}
	dir := filepath.Dir(exeDir)

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()
	defer signal.Stop(sigCh)

	switch mode {
	case "vk-turn":
		binPath := filepath.Join(dir, clientBinName())
		if err := RunClient(ctx, binPath, cfg, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "client exited:", err)
		}
	case "xray":
		if cfg.XraySubscriptionURL == "" {
			fmt.Fprintln(os.Stderr, "нет xray-подписки для этого профиля")
			return
		}
		resp, err := http.Get(cfg.XraySubscriptionURL) //nolint:noctx // short-lived CLI tool, ctx wiring deferred to Milestone B
		if err != nil {
			fmt.Fprintln(os.Stderr, "fetch subscription:", err)
			return
		}
		defer resp.Body.Close()
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			body = append(body, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		configs, err := convertSubscription(string(body))
		if err != nil || len(configs) == 0 {
			fmt.Fprintln(os.Stderr, "no usable xray configs in subscription:", err)
			return
		}
		binPath := filepath.Join(dir, xrayBinName())
		if err := RunXray(ctx, binPath, configs[0], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "xray exited:", err)
		}
	}
}
