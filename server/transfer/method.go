package transfer

import (
	"fmt"
	"strings"

	"github.com/0x7d8/wings/config"
)

// ResolveMethod returns the transport to use given panel + local config.
// Empty panel method falls back to system.transfers.method (default tus).
// Unsupported values (including legacy "rsync") return an error so misconfigured
// panels fail loudly instead of silently falling through.
func ResolveMethod(panelMethod string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(panelMethod))
	switch m {
	case "archive":
		return "archive", nil
	case "tus":
		return "tus", nil
	case "":
		cfg := strings.ToLower(strings.TrimSpace(config.Get().System.Transfers.Method))
		if cfg == "archive" {
			return "archive", nil
		}
		return "tus", nil
	case "rsync":
		return "", fmt.Errorf("transfer method %q is no longer supported; panel must send tus or archive", m)
	default:
		return "", fmt.Errorf("unsupported transfer method %q; use tus or archive", m)
	}
}

// ArchiveURLFromTusURL derives the legacy multipart archive endpoint from a TUS create URL.
// Example: https://node/api/transfers/tus → https://node/api/transfers
func ArchiveURLFromTusURL(tusURL string) string {
	u := strings.TrimRight(tusURL, "/")
	if strings.HasSuffix(strings.ToLower(u), "/tus") {
		return u[:len(u)-len("/tus")]
	}
	return u
}
