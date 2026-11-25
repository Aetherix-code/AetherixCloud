package ingress

import (
	"strconv"
	"strings"
)

// OptionsFromEnv builds Options from AX_* environment variables:
//
//	AX_DEV=1                       dev mode: Caddy internal CA, *.localhost
//	AX_CADDY_ADMIN                 admin address (default 127.0.0.1:2019)
//	AX_INGRESS_HTTP_PORT/HTTPS_PORT
//	AX_ACME_EMAIL, AX_ACME_CA      ACME account email / directory URL
//	AX_DNS_CLOUDFLARE_TOKEN        enable DNS-01 through Cloudflare
//	AX_DNS_WILDCARD=a.com,b.com    zones for wildcard certs (default: cluster domain)
func OptionsFromEnv(getenv func(string) string, clusterDomain string) Options {
	o := Options{
		Dev:       getenv("AX_DEV") == "1" || strings.EqualFold(getenv("AX_DEV"), "true"),
		AdminAddr: getenv("AX_CADDY_ADMIN"),
		ACMEEmail: getenv("AX_ACME_EMAIL"),
		ACMECA:    getenv("AX_ACME_CA"),
	}
	o.HTTPPort, _ = strconv.Atoi(getenv("AX_INGRESS_HTTP_PORT"))
	o.HTTPSPort, _ = strconv.Atoi(getenv("AX_INGRESS_HTTPS_PORT"))
	if tok := getenv("AX_DNS_CLOUDFLARE_TOKEN"); tok != "" {
		d := &DNS01{Provider: "cloudflare", APIToken: tok}
		for _, z := range strings.Split(getenv("AX_DNS_WILDCARD"), ",") {
			if z = strings.TrimSpace(z); z != "" {
				d.Wildcards = append(d.Wildcards, z)
			}
		}
		if len(d.Wildcards) == 0 && clusterDomain != "" {
			d.Wildcards = []string{clusterDomain}
		}
		o.DNS01 = d
	}
	return o
}
