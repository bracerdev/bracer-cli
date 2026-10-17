package pg

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Candidate is a discovered Postgres instance, not necessarily reachable.
type Candidate struct {
	Container *Container // Docker container
	Pod       *Pod       // container in a Kubernetes pod
	Host      string     // for Postgres on this host
	Port      int
	Socket    string // Unix socket path if found
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
		if ct.Kubernetes {
			parts = append(parts, "Kubernetes via cri-dockerd")
		}
		if ct.Unverified {
			parts = append(parts, "not verified: docker top failed")
		}
		return fmt.Sprintf("%s (container; %s)", ct.Name, strings.Join(parts, "; "))
	}
	if c.Pod != nil {
		p := c.Pod
		parts := []string{p.Image, "container " + p.Ref.Container}
		if !p.Ready {
			parts = append(parts, "not ready")
		}
		if p.Unverified {
			parts = append(parts, "not verified: could not look for a postgres process")
		}
		return fmt.Sprintf("%s/%s (pod; %s)", p.Ref.Namespace, p.Ref.Name, strings.Join(parts, "; "))
	}
	var parts []string
	if c.Process != "" {
		parts = append(parts, c.Process)
	}
	if c.Socket != "" {
		parts = append(parts, "socket "+c.Socket)
	}
	if c.Confirmed != nil && !*c.Confirmed {
		parts = append(parts, "did not answer like Postgres")
	}
	host := c.Host
	if host == "" {
		return fmt.Sprintf("unix socket %s (port %d)", filepath.Dir(c.Socket), c.Port)
	}
	if host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return fmt.Sprintf("%s:%d (on this host; %s)", host, c.Port, strings.Join(parts, "; "))
}

// SuggestName suggests an instance name: the container name, or host-port.
func (c Candidate) SuggestName() string {
	if c.Container != nil {
		return sanitizeName(c.Container.Name)
	}
	if c.Pod != nil {
		return sanitizeName(c.Pod.Ref.Name)
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

// Discover merges Docker containers and Kubernetes pods with the host scan: a
// host port matching a container's HostPort is folded into that container (spec
// init, step 2b), the remaining ports of postgres processes become host instances,
// then confirmed with SSLRequest. `k8s_` containers (cri-dockerd) are dropped:
// their pods are in pods.
func Discover(ctx context.Context, containers []*Container, pods []*Pod, hs HostScan, probe Prober) ([]Candidate, []string) {
	var out []Candidate
	mapped := map[int]bool{}
	for _, c := range containers {
		for _, hp := range c.HostPorts {
			mapped[hp] = true
		}
		if c.Kubernetes {
			continue
		}
		out = append(out, Candidate{Container: c})
	}
	for _, p := range pods {
		out = append(out, Candidate{Pod: p})
	}
	byPort := map[int]*Candidate{}
	var hosts []*Candidate
	for _, l := range hs.Listeners {
		if mapped[l.Port] || !strings.Contains(strings.ToLower(l.Process), "postgres") {
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
	for port, path := range hs.Sockets {
		if mapped[port] {
			continue
		}
		if ex := byPort[port]; ex != nil {
			ex.Socket = path
			continue
		}
		c := &Candidate{Port: port, Socket: path}
		byPort[port] = c
		hosts = append(hosts, c)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Port < hosts[j].Port })
	for _, h := range hosts {
		if h.Host != "" && probe != nil {
			host := h.Host
			if host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			ok, _ := probe(ctx, host, h.Port)
			h.Confirmed = &ok
		}
		out = append(out, *h)
	}
	return out, hs.Warnings
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
