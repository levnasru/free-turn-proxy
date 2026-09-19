// cmd/desktop/config.go
package main

// DesktopConfig mirrors vkturn-ios-portal's api.go DesktopConfig byte-for-byte
// (JSON field names must match — two separate Go modules, no shared package).
type DesktopConfig struct {
	HubURLs             []string `json:"hubUrls"`
	HubPin              string   `json:"hubPin"`
	HubToken            string   `json:"hubToken"`
	Peer                string   `json:"peer"`
	WgPeer              string   `json:"wgPeer,omitempty"`
	ObfProfile          string   `json:"obfProfile"`
	ObfKey              string   `json:"obfKey"`
	Streams             int      `json:"streams"`
	SplitMode           string   `json:"splitMode"`
	XraySubscriptionURL string   `json:"xraySubscriptionUrl"`
	DirectDomains       []string `json:"directDomains,omitempty"`
	DirectIPs           []string `json:"directIps,omitempty"`
	WgConfig            string   `json:"wgConfig,omitempty"`
}

// defaultDirectDomains contains essential domains that should always bypass the tunnel
// to avoid breaking local services, Russian government portals, and VK infrastructure itself.
var defaultDirectDomains = []string{
	"domain:vk.com",
	"domain:vk.ru",
	"domain:userapi.com",
	"domain:vk-portal.net",
	"domain:vk-apps.com",
	"domain:gosuslugi.ru",
	"domain:nalog.gov.ru",
	"domain:mos.ru",
	"domain:yandex.ru",
	"domain:ya.ru",
	"domain:kinopoisk.ru",
	"domain:sberbank.ru",
	"domain:sber.ru",
	"domain:tbank.ru",
	"domain:tinkoff.ru",
	"domain:vtb.ru",
	"domain:alfa-bank.ru",
	"domain:alfabank.ru",
	"domain:ozon.ru",
	"domain:wildberries.ru",
	"domain:avito.ru",
	"domain:2ip.ru",
}

