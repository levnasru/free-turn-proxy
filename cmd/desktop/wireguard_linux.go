// cmd/desktop/wireguard_linux.go
//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const wgInterfaceName = "vkturn0"

// startNativeWGTunnel brings up a kernel WireGuard network interface (vkturn0)
// using wg-quick and routes all traffic except private LAN ranges and user exclusions
// into the local client UDP listener (127.0.0.1:9000).
func startNativeWGTunnel(ctx context.Context, parsed *WGConfigParsed, routes []string, physicalIface string) (func(), error) {
	if !commandAvailable("wg-quick") {
		return nil, fmt.Errorf("утилита wg-quick не найдена в PATH — установите wireguard-tools (sudo apt install wireguard-tools)")
	}

	confPath := filepath.Join(os.TempDir(), wgInterfaceName+".conf")

	// Check if DNS management via resolvconf/resolvectl is available
	withDNS := commandAvailable("resolvconf") || commandAvailable("resolvectl")
	confContent := buildNativeWGConf(parsed, routes, withDNS)
	if err := os.WriteFile(confPath, []byte(confContent), 0o600); err != nil {
		return nil, fmt.Errorf("запись конфига WireGuard %s: %w", confPath, err)
	}

	// Clean up any stale interface from a previous aborted run
	_ = exec.Command("wg-quick", "down", confPath).Run()
	_ = exec.Command("ip", "link", "delete", "dev", wgInterfaceName).Run()

	out, err := exec.Command("wg-quick", "up", confPath).CombinedOutput()
	if err != nil {
		// If failed with DNS, retry without DNS directive in case resolvconf is missing/broken
		if withDNS {
			confContentNoDNS := buildNativeWGConf(parsed, routes, false)
			_ = os.WriteFile(confPath, []byte(confContentNoDNS), 0o600)
			out, err = exec.Command("wg-quick", "up", confPath).CombinedOutput()
		}
		if err != nil {
			_ = os.Remove(confPath)
			return nil, fmt.Errorf("ошибка wg-quick up: %w (вывод: %s)", err, strings.TrimSpace(string(out)))
		}
	}

	monCtx, monCancel := context.WithCancel(ctx)
	go monitorWireGuardInterface(monCtx)

	cleanup := func() {
		monCancel()
		fmt.Println("\nОтключаю WireGuard туннель...")
		_ = exec.Command("wg-quick", "down", confPath).Run()
		_ = exec.Command("ip", "link", "delete", "dev", wgInterfaceName).Run()
		_ = os.Remove(confPath)
	}

	return cleanup, nil
}

func monitorWireGuardInterface(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	var warnedStale bool
	var prevRxBytes, prevTxBytes uint64
	var firstSeen time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			out, err := exec.CommandContext(ctx, "wg", "show", wgInterfaceName, "dump").Output()
			if err != nil {
				continue
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) < 2 {
				continue
			}
			fields := strings.Split(lines[1], "\t")
			if len(fields) < 7 {
				continue
			}
			handshakeEpoch, _ := strconv.ParseInt(fields[4], 10, 64)
			rxBytes, _ := strconv.ParseUint(fields[5], 10, 64)
			txBytes, _ := strconv.ParseUint(fields[6], 10, 64)

			now := time.Now().Unix()
			if firstSeen.IsZero() {
				firstSeen = time.Now()
			}

			if handshakeEpoch == 0 {
				if time.Since(firstSeen) > 15*time.Second {
					fmt.Fprintf(os.Stderr, "[WG-WARN] ⚠️ WireGuard не может установить первый хэндшейк с сервером (tx: %d B, rx: 0 B)\n", txBytes)
				} else if debugMode {
					fmt.Printf("[WG-STATUS] ⏳ Ожидание первого хэндшейка WireGuard... (tx: %d B, rx: %d B)\n", txBytes, rxBytes)
				}
				continue
			}

			handshakeAge := now - handshakeEpoch
			rxDelta := rxBytes - prevRxBytes
			txDelta := txBytes - prevTxBytes
			prevRxBytes, prevTxBytes = rxBytes, txBytes

			// WireGuard пересогласует криптографические ключи (rekey) раз в 120 секунд.
			// Между хэндшейками возраст хэндшейка штатно растёт от 0 до 120-140 секунд.
			// Реальное зависание: хэндшейк старше 140с, ЛИБО хэндшейк старше 45с при активных
			// безответных попытках передачи (txDelta > 50KB без входящих rxDelta == 0).
			isStale := handshakeAge > 140 || (handshakeAge > 45 && txDelta > 50000 && rxDelta == 0)

			if isStale {
				if !warnedStale || handshakeAge%30 == 0 {
					fmt.Fprintf(os.Stderr, "[WG-WARN] ⚠️ Хэндшейк WireGuard устарел (%d сек назад)! (rx: %d B, tx: %d B) — связь с сервером может быть заблокирована или зависла!\n",
						handshakeAge, rxBytes, txBytes)
					warnedStale = true
				}
			} else {
				if warnedStale && handshakeAge < 10 {
					fmt.Printf("[WG-STATUS] ✅ Хэндшейк WireGuard обновлен (%dс назад)! Связь в норме.\n", handshakeAge)
					warnedStale = false
				}
				if debugMode {
					fmt.Printf("[WG-STATUS] vkturn0: хэндшейк %dс назад | RX: %d B, TX: %d B\n", handshakeAge, rxBytes, txBytes)
				}
			}
		}
	}
}
