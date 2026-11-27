// Package guards holds the summon arm guards: the Tailscale route-off gate
// and the geo helpers.
package guards

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultTailscaleSock is the tailscaled LocalAPI socket on the car.
const DefaultTailscaleSock = "/var/run/tailscale/tailscaled.sock"

const (
	routeBudget  = 500 * time.Millisecond
	maxPrefsBody = 1 << 20
)

// RouteOff reads the tailscaled prefs over the LocalAPI unix socket and
// returns nil only when AdvertiseRoutes holds nothing but the exit-node pair
// 0.0.0.0/0 and ::/0. Every failure is an error (fail closed). Errors name
// the reason only; the prefs body (owner login, VIN) is never echoed.
func RouteOff(ctx context.Context, sock string) error {
	ctx, cancel := context.WithTimeout(ctx, routeBudget)
	defer cancel()

	var d net.Dialer
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/prefs", nil)
	if err != nil {
		return errors.New("route gate: cannot build request")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("route gate: tailscaled prefs unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("route gate: tailscaled prefs returned non-200 status")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPrefsBody+1))
	if err != nil {
		return errors.New("route gate: cannot read tailscaled prefs")
	}
	if len(body) > maxPrefsBody {
		return errors.New("route gate: tailscaled prefs too large")
	}
	var p struct {
		AdvertiseRoutes json.RawMessage
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return errors.New("route gate: tailscaled prefs not valid JSON")
	}
	var routes []string
	if p.AdvertiseRoutes == nil {
		return errors.New("route gate: AdvertiseRoutes missing from prefs")
	}
	if err := json.Unmarshal(p.AdvertiseRoutes, &routes); err != nil {
		return errors.New("route gate: AdvertiseRoutes is not a string array")
	}
	for _, r := range routes {
		if r != "0.0.0.0/0" && r != "::/0" {
			return errors.New("route gate: a non-exit-node route is advertised")
		}
	}
	return nil
}
