package pg

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Conn is the connection info for a Postgres. All of it reaches the child
// process through libpq environment variables (PGHOST…), never through argv.
type Conn struct {
	Host     string // empty: default Unix socket (used with docker exec)
	Port     int
	User     string
	Password string
	Database string
	Params   map[string]string // sslmode… keyed by libpq parameter name
}

// WithDatabase returns a copy connecting to another database.
func (c Conn) WithDatabase(db string) Conn {
	c.Database = db
	return c
}

// paramEnv maps URL parameters to libpq environment variables.
var paramEnv = map[string]string{
	"sslmode":              "PGSSLMODE",
	"sslrootcert":          "PGSSLROOTCERT",
	"sslcert":              "PGSSLCERT",
	"sslkey":               "PGSSLKEY",
	"sslsni":               "PGSSLSNI",
	"application_name":     "PGAPPNAME",
	"connect_timeout":      "PGCONNECT_TIMEOUT",
	"options":              "PGOPTIONS",
	"target_session_attrs": "PGTARGETSESSIONATTRS",
	"channel_binding":      "PGCHANNELBINDING",
	"gssencmode":           "PGGSSENCMODE",
	"require_auth":         "PGREQUIREAUTH",
}

// Env returns KEY=VALUE pairs for libpq. Always sets PGCONNECT_TIMEOUT if absent.
func (c Conn) Env() []string {
	var env []string
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("PGHOST", c.Host)
	if c.Port != 0 {
		add("PGPORT", strconv.Itoa(c.Port))
	}
	add("PGUSER", c.User)
	add("PGPASSWORD", c.Password)
	add("PGDATABASE", c.Database)
	keys := make([]string, 0, len(c.Params))
	for k := range c.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hasTimeout := false
	for _, k := range keys {
		add(paramEnv[k], c.Params[k])
		if k == "connect_timeout" {
			hasTimeout = true
		}
	}
	if !hasTimeout {
		add("PGCONNECT_TIMEOUT", "10")
	}
	return env
}

// EnvNames is the names of the variables in Env, for passing `-e NAME` to docker.
func (c Conn) EnvNames() []string {
	return envNames(c.Env())
}

// IsLocalHost reports whether Host points at this machine: empty or a socket
// directory (Unix socket), localhost, 127.0.0.0/8, ::1.
func IsLocalHost(host string) bool {
	switch {
	case host == "", strings.HasPrefix(host, "/"), host == "localhost", host == "::1", host == "[::1]":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ParseURL parses postgres://user:pass@host:port/db?sslmode=… into a Conn.
// Unknown parameters are an error so a misspelled sslmode is not silently ignored.
func ParseURL(raw string) (Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Conn{}, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return Conn{}, fmt.Errorf("URL must start with postgres:// or postgresql://")
	}
	if strings.Contains(u.Host, ",") {
		return Conn{}, fmt.Errorf("multi-host URLs are not supported")
	}
	c := Conn{Port: 5432, Params: map[string]string{}}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
	} else if portStr != "" {
		if c.Port, err = strconv.Atoi(portStr); err != nil {
			return Conn{}, fmt.Errorf("invalid port in URL")
		}
	}
	c.Host = host
	if c.Host == "" {
		c.Host = "localhost"
	}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	c.Database = strings.TrimPrefix(u.Path, "/")
	if c.Database == "" {
		c.Database = "postgres"
	}
	for k, vs := range u.Query() {
		k = strings.ToLower(k)
		if len(vs) == 0 {
			continue
		}
		switch k {
		case "host":
			c.Host = vs[0]
		case "port":
			if c.Port, err = strconv.Atoi(vs[0]); err != nil {
				return Conn{}, fmt.Errorf("invalid port parameter in URL")
			}
		case "user":
			c.User = vs[0]
		case "password":
			c.Password = vs[0]
		case "dbname":
			c.Database = vs[0]
		default:
			if _, ok := paramEnv[k]; !ok {
				return Conn{}, fmt.Errorf("unsupported URL parameter: %s", k)
			}
			c.Params[k] = vs[0]
		}
	}
	return c, nil
}
