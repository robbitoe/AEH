// Package egress is the network side of an agent sandbox (stage 6, S3): a proxy that
// allows CONNECT only to declared host:port pairs, a forwarder that relays a container's
// loopback port to the proxy's Unix socket, and a launcher that runs an agent's CLI
// behind that forwarder. It uses only the standard library and is built into the agent
// image as the aeh-egress program.
package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Policy is what one proxy allows. aeh writes it per launch; the proxy reads it from a
// read-only mount.
type Policy struct {
	Allow          []string `json:"allow"`           // host:port pairs, exact
	MaxConnections int      `json:"max_connections"` // open tunnels at the same time
	MaxBytes       int64    `json:"max_bytes"`       // bytes in both directions, whole launch
	IdleSeconds    int      `json:"idle_seconds"`    // a tunnel with no traffic this long is closed
	ConnectSeconds int      `json:"connect_seconds"` // request, resolution and connection deadline
}

// MaxPairs bounds a profile's egress list.
const MaxPairs = 64

// DefaultPolicy has aeh's limits and the given pairs.
func DefaultPolicy(allow []string) Policy {
	return Policy{Allow: allow, MaxConnections: 64, MaxBytes: 4 << 30, IdleSeconds: 900, ConnectSeconds: 15}
}

// Validate checks every pair and limit.
func (p Policy) Validate() error {
	if err := CheckPairs(p.Allow); err != nil {
		return err
	}
	if p.MaxConnections < 1 || p.MaxBytes < 1 || p.IdleSeconds < 1 || p.ConnectSeconds < 1 {
		return errors.New("egress policy: every limit must be positive")
	}
	return nil
}

// ParsePolicy reads a policy and validates it. Unknown fields are refused.
func ParsePolicy(data []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("egress policy: %w", err)
	}
	return p, p.Validate()
}

// CheckPairs validates a list of host:port pairs: each canonical, no duplicates, at most
// MaxPairs.
func CheckPairs(pairs []string) error {
	if len(pairs) > MaxPairs {
		return fmt.Errorf("egress: at most %d host:port pairs", MaxPairs)
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		if err := CheckPair(p); err != nil {
			return err
		}
		if seen[p] {
			return fmt.Errorf("egress: %s is listed twice", p)
		}
		seen[p] = true
	}
	return nil
}

// CheckPair validates one host:port pair in its canonical form: a lowercase DNS name or
// an IP address (IPv6 in brackets), and a decimal port from 1 to 65535. Wildcards are not
// supported: each host is named exactly.
func CheckPair(pair string) error {
	host, port, err := net.SplitHostPort(pair)
	if err != nil {
		return fmt.Errorf("egress: %q is not host:port", pair)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return fmt.Errorf("egress: %q has an invalid port", pair)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" || net.JoinHostPort(a.String(), port) != pair {
			return fmt.Errorf("egress: %q: write the address in its canonical form, %s", pair, net.JoinHostPort(a.String(), port))
		}
		return nil
	}
	if err := checkName(host); err != nil {
		return fmt.Errorf("egress: %q: %w", pair, err)
	}
	return nil
}

func checkName(host string) error {
	if host == "" || len(host) > 253 {
		return errors.New("the host name is empty or too long")
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return errors.New("the host name has an empty, long or hyphen-edged label")
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("the host name must use lowercase letters, digits, hyphens and dots")
			}
		}
	}
	if last := labels[len(labels)-1]; last[0] >= '0' && last[0] <= '9' {
		return errors.New("the host name's last label must start with a letter")
	}
	return nil
}

// normalizeTarget turns a CONNECT target into the form pairs are written in. A target
// whose host is neither a valid name nor an address is refused, so nothing else (a path,
// user information, a long string) ever reaches the log.
func normalizeTarget(target string) (host string, port uint16, key string, err error) {
	if len(target) > 300 {
		return "", 0, "", errors.New("target too long")
	}
	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, "", err
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, "", errors.New("invalid port")
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", 0, "", errors.New("zoned address")
		}
		h = a.String()
	} else if err := checkName(h); err != nil {
		return "", 0, "", err
	}
	return h, uint16(n), net.JoinHostPort(h, strconv.Itoa(n)), nil
}
