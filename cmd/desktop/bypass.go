package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// bypassFilePath returns ~/.vkturn/bypass.txt — a plain-text file where the user
// can list domains and IPs to bypass the tunnel (direct routing).
func bypassFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".vkturn", "bypass.txt"), nil
}

// ensureBypassFileExists creates a helpful template bypass.txt on first access
// if none exists yet.
func ensureBypassFileExists() (string, error) {
	path, err := bypassFilePath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	template := `# LFT: Сайты и IP-адреса для прямого доступа (мимо туннеля / direct)
# Добавляйте по одному на строку. Поддерживаются домены, IP и CIDR-подсети.
#
# Примеры доменов:
# kinopoisk.ru
# ozon.ru
# wildberries.ru
#
# Примеры IP / подсетей:
# 195.82.146.120
# 95.163.0.0/16
#
`
	if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
		return "", err
	}
	chownToOriginalUserIfElevated(path)
	return path, nil
}

// loadUserBypassList reads ~/.vkturn/bypass.txt and parses domains and IP CIDRs.
func loadUserBypassList() (domains []string, ips []string) {
	path, err := ensureBypassFileExists()
	if err != nil {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip protocol/path if user accidentally pasted full URL
		if idx := strings.Index(line, "://"); idx != -1 {
			line = line[idx+3:]
		}
		if idx := strings.Index(line, "/"); idx != -1 && !isCIDR(line) {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.Contains(line, "/") {
			if _, _, err := net.ParseCIDR(line); err == nil {
				ips = append(ips, line)
				continue
			}
		}
		if ip := net.ParseIP(line); ip != nil {
			if ip.To4() != nil {
				ips = append(ips, line+"/32")
			} else {
				ips = append(ips, line+"/128")
			}
			continue
		}
		d := line
		if !strings.HasPrefix(d, "domain:") && !strings.HasPrefix(d, "full:") && !strings.HasPrefix(d, "geosite:") {
			d = "domain:" + d
		}
		domains = append(domains, d)
	}
	return domains, ips
}

func isCIDR(s string) bool {
	_, _, err := net.ParseCIDR(s)
	return err == nil
}

// collectBypassRules aggregates bypass domains and IPs from:
// 1. Built-in defaults (Russian services, VK infrastructure)
// 2. Portal/server config (cfg.DirectDomains, cfg.DirectIPs)
// 3. User local ~/.vkturn/bypass.txt
func collectBypassRules(cfg *DesktopConfig) (domains []string, ips []string) {
	domainSet := make(map[string]bool)
	ipSet := make(map[string]bool)

	for _, d := range defaultDirectDomains {
		domainSet[d] = true
	}
	if cfg != nil {
		for _, d := range cfg.DirectDomains {
			if !strings.HasPrefix(d, "domain:") && !strings.HasPrefix(d, "full:") {
				d = "domain:" + d
			}
			domainSet[d] = true
		}
		for _, ip := range cfg.DirectIPs {
			ipSet[ip] = true
		}
	}

	userDomains, userIPs := loadUserBypassList()
	for _, d := range userDomains {
		domainSet[d] = true
	}
	for _, ip := range userIPs {
		ipSet[ip] = true
	}

	for d := range domainSet {
		domains = append(domains, d)
	}
	for ip := range ipSet {
		ips = append(ips, ip)
	}
	return domains, ips
}

// readRawUserBypassEntries returns all non-empty, non-comment user entries from bypass.txt.
func readRawUserBypassEntries() ([]string, error) {
	path, err := ensureBypassFileExists()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}
	return entries, scanner.Err()
}

// addUserBypassEntry appends a new rule to ~/.vkturn/bypass.txt.
func addUserBypassEntry(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil
	}
	existing, err := readRawUserBypassEntries()
	if err == nil {
		for _, e := range existing {
			if strings.EqualFold(e, entry) {
				return nil // already exists
			}
		}
	}

	path, err := ensureBypassFileExists()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := fmt.Fprintf(f, "%s\n", entry); err != nil {
		return err
	}
	chownToOriginalUserIfElevated(path)
	return nil
}

// removeUserBypassEntry removes the entry at 1-based index from ~/.vkturn/bypass.txt.
func removeUserBypassEntry(targetIdx int) error {
	path, err := ensureBypassFileExists()
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	lines := strings.Split(string(content), "\n")
	var newLines []string
	currentIdx := 0
	found := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			currentIdx++
			if currentIdx == targetIdx {
				found = true
				continue // skip this line
			}
		}
		newLines = append(newLines, line)
	}

	if !found {
		return fmt.Errorf("правило #%d не найдено", targetIdx)
	}

	if err := os.WriteFile(path, []byte(strings.Join(newLines, "\n")), 0o644); err != nil {
		return err
	}
	chownToOriginalUserIfElevated(path)
	return nil
}

// clearUserBypassList resets bypass.txt to default template comments without user entries.
func clearUserBypassList() error {
	path, err := bypassFilePath()
	if err != nil {
		return err
	}
	template := `# LFT: Сайты и IP-адреса для прямого доступа (мимо туннеля / direct)
# Добавляйте по одному на строку. Поддерживаются домены, IP и CIDR-подсети.
#
# Примеры доменов:
# kinopoisk.ru
# ozon.ru
# wildberries.ru
#
# Примеры IP / подсетей:
# 195.82.146.120
# 95.163.0.0/16
#
`
	if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
		return err
	}
	chownToOriginalUserIfElevated(path)
	return nil
}

// openInSystemEditor launches the file in the user's default text editor.
func openInSystemEditor(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("notepad.exe", path)
	case "darwin":
		cmd = exec.Command("open", "-t", path)
	default:
		// Linux/Unix
		if ed := os.Getenv("VISUAL"); ed != "" {
			cmd = exec.Command(ed, path)
		} else if ed := os.Getenv("EDITOR"); ed != "" {
			cmd = exec.Command(ed, path)
		} else {
			cmd = exec.Command("xdg-open", path)
		}
	}
	return cmd.Start()
}

