package notify

import (
	"net/url"
	"slices"
	"strings"
)

// forgeURL reports whether target is an https URL on one of the forge hosts: the only URLs a
// click may open.
func forgeURL(target string, hosts []string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	return slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, u.Host) })
}
