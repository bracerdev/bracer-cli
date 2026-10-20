package pg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Candidate is a discovered Postgres instance, not necessarily reachable.
type Candidate struct {
	Container *Container // Docker container
	Local     *Local     // Postgres installed on this host, reached over its socket (ADR-0015)
	Host      string     // for Postgres on this host, reached over TCP
	Port      int
	Process   string
	Confirmed *bool // SSLRequest: nil not tried, true is Postgres, false did not answer correctly
}

// Label is a one-line display for the user to choose from.
func (c Candidate) Label() string {
	if c.Container != nil {
		ct := c.Container
		var parts []string
		parts = append(parts, ct.Image)
		if !ct.Running {
			parts = append(parts, "stopped: "+ct.Status)
		}
		if len(ct.HostPorts) > 0 {
			parts = append(parts, "published on localhost:"+joinInts(ct.HostPorts))
		}
		if ct.Unverified {
			parts = append(parts, "not verified: docker top failed")
		}
		return fmt.Sprintf("%s (container; %s)", ct.Name, strings.Join(parts, "; "))
	}
	if c.Local != nil {
		return fmt.Sprintf("local Postgres on port %d (socket %s)", c.Local.Port, c.Local.Dir)
	}
	var parts []string
	if c.Process != "" {
		parts = append(parts, c.Process)
	}
	if c.Confirmed != nil && !*c.Confirmed {
		parts = append(parts, "did not answer like Postgres")
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s:%d (on this host)", c.DisplayHost(), c.Port)
	}
	return fmt.Sprintf("%s:%d (on this host; %s)", c.DisplayHost(), c.Port, strings.Join(parts, "; "))
}

// DisplayHost is "localhost" for any loopback or wildcard address, else the address.
func (c Candidate) DisplayHost() string {
	switch c.Host {
	case "0.0.0.0", "::", "::1", "127.0.0.1", "localhost":
		return "localhost"
	}
	return c.Host
}

// URLHost is the address init puts in the URL for a host candidate: 127.0.0.1
// when Postgres listens on loopback or every address, otherwise the one address
// it listens on (spec init § Flow 1b).
func (c Candidate) URLHost() string {
	switch c.Host {
	case "", "0.0.0.0", "::", "127.0.0.1", "localhost":
		return "127.0.0.1"
	case "::1":
		return "[::1]"
	}
	if strings.Contains(c.Host, ":") {
		return "[" + c.Host + "]"
	}
	return c.Host
}

// SuggestName suggests an instance name: the container name, or local-<port>.
func (c Candidate) SuggestName() string {
	if c.Container != nil {
		return sanitizeName(c.Container.Name)
	}
	if c.Local != nil {
		return sanitizeName(fmt.Sprintf("local-%d", c.Local.Port))
	}
	return sanitizeName(fmt.Sprintf("local-%d", c.Port))
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = fmt.Sprint(n)
	}
	return strings.Join(s, ",")
}

// Prober confirms that a TCP port is Postgres; can be swapped in tests.
type Prober func(ctx context.Context, host string, port int) (bool, error)

// Discovery is what init shows after looking around (spec init § Flow 1).
type Discovery struct {
	Candidates []Candidate
	// Kubernetes holds the signs of Kubernetes on this machine (k8s_ containers,
	// k3s or kubelet directories): not supported, shown as one notice.
	Kubernetes []string
	Warnings   []string
}

// Discover merges Docker containers with the host scan: a host port matching a
// container's HostPort is folded into that container (spec init step 1b), the
// remaining ports of postgres processes, or of processes this user cannot see,
// become host candidates confirmed with SSLRequest (unseen ones must answer). `k8s_` containers (cri-dockerd) are not listed.
func Discover(ctx context.Context, containers []*Container, hs HostScan, probe Prober) Discovery {
	var out Discovery
	mapped := map[int]bool{}
	k8s := 0
	for _, c := range containers {
		for _, hp := range c.HostPorts {
			mapped[hp] = true
		}
		if c.Kubernetes {
			k8s++
			continue
		}
		out.Candidates = append(out.Candidates, Candidate{Container: c})
	}
	if k8s > 0 {
		out.Kubernetes = append(out.Kubernetes, fmt.Sprintf("%d Kubernetes container(s) seen through Docker", k8s))
	}
	byPort := map[int]*Candidate{}
	var hosts []*Candidate
	for _, l := range hs.Listeners {
		// No process name: the port belongs to another user (Postgres from apt runs
		// as postgres). Kept only if SSLRequest answers below.
		if mapped[l.Port] || (l.Process != "" && !strings.Contains(strings.ToLower(l.Process), "postgres")) {
			continue
		}
		if ex := byPort[l.Port]; ex != nil {
			if preferAddr(l.Addr, ex.Host) {
				ex.Host = l.Addr
			}
			continue
		}
		c := &Candidate{Host: l.Addr, Port: l.Port, Process: l.Process}
		byPort[l.Port] = c
		hosts = append(hosts, c)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Port < hosts[j].Port })
	if probe != nil {
		var wg sync.WaitGroup
		for _, h := range hosts {
			wg.Add(1)
			go func(h *Candidate) {
				defer wg.Done()
				host := h.Host
				if host == "0.0.0.0" || host == "::" {
					host = "127.0.0.1"
				}
				ok, _ := probe(ctx, host, h.Port)
				h.Confirmed = &ok
			}(h)
		}
		wg.Wait()
	}
	// A socket is the cluster itself: one local candidate per port, and the
	// TCP port of the same number is folded into it (ADR-0015).
	ports := make([]int, 0, len(hs.Sockets))
	for port := range hs.Sockets {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	local := map[int]bool{}
	for _, port := range ports {
		if mapped[port] {
			continue
		}
		local[port] = true
		out.Candidates = append(out.Candidates, Candidate{Local: &Local{Dir: filepath.Dir(hs.Sockets[port]), Port: port}})
	}
	for _, h := range hosts {
		if local[h.Port] {
			continue
		}
		if h.Process == "" && (h.Confirmed == nil || !*h.Confirmed) {
			continue // not Postgres, or not provably so
		}
		out.Candidates = append(out.Candidates, *h)
	}
	out.Warnings = hs.Warnings
	return out
}

// KubernetesDirs are directories whose presence means this machine is a
// Kubernetes node (spec init § Flow 1d). A variable so tests can point elsewhere.
var KubernetesDirs = []string{"/etc/rancher/k3s", "/var/lib/rancher/k3s", "/var/lib/kubelet"}

// KubernetesSigns returns the Kubernetes directories present on this machine.
func KubernetesSigns() []string {
	var out []string
	for _, d := range KubernetesDirs {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// preferAddr picks IPv4 loopback as the display address when a port listens on several addresses.
func preferAddr(candidate, current string) bool {
	rank := func(a string) int {
		switch a {
		case "127.0.0.1":
			return 0
		case "localhost":
			return 1
		case "0.0.0.0":
			return 2
		case "::1":
			return 3
		default:
			return 4
		}
	}
	return rank(candidate) < rank(current)
}
