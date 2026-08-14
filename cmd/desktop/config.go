// cmd/desktop/config.go
package main

// DesktopConfig mirrors vkturn-ios-portal's api.go DesktopConfig byte-for-byte
// (JSON field names must match — two separate Go modules, no shared package).
type DesktopConfig struct {
	HubURLs             []string `json:"hubUrls"`
	HubPin              string   `json:"hubPin"`
	HubToken            string   `json:"hubToken"`
	Peer                string   `json:"peer"`
	ObfProfile          string   `json:"obfProfile"`
	ObfKey              string   `json:"obfKey"`
	Streams             int      `json:"streams"`
	SplitMode           string   `json:"splitMode"`
	XraySubscriptionURL string   `json:"xraySubscriptionUrl"`
}
