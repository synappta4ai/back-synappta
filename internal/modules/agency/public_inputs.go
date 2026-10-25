package agency

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// requirePublicInputs rejects media inputs whose URL a remote provider cannot
// reach (loopback / private hosts, e.g. URL_PUBLIC=http://localhost:8099).
// Failing here avoids submitting a task that the provider rejects afterwards.
func requirePublicInputs(items []ContentItem) error {
	for _, item := range items {
		if item.Type == "text" || item.DataURL == "" {
			continue
		}
		if isNonPublicURL(item.DataURL) {
			return fmt.Errorf("el proveedor de video no puede descargar %q desde %s: "+
				"configura URL_PUBLIC con una URL pública (túnel o despliegue) o un bucket "+
				"con public_base, y vuelve a intentar", item.Name, hostOf(item.DataURL))
		}
	}
	return nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}

// isNonPublicURL reports http(s) URLs that point at this machine or a private
// network. Non-http schemes (asset://, data:) are left to the provider.
func isNonPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}
