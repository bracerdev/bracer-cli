package pg

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Container is a Docker container that may be Postgres.
type Container struct {
	ID         string
	Name       string
	Image      string
	Running    bool
	Status     string
	Env        map[string]string
	Port       int   // Postgres port inside the container, default 5432
	HostPorts  []int // ports published on the host for Port
	Networks   []string
	Kubernetes bool // name prefixed k8s_: cri-dockerd; not listed, only counted for the Kubernetes notice (ADR-0013)
	HasPgProc  bool // docker top saw a postgres process (evidence)
	Unverified bool // running but docker top failed: could not check
}

// IsPostgres: a running container must have a postgres process (image/env are
// only hints, app pods carry POSTGRES_USER too); if it cannot be checked it is
// still listed with Unverified. Stopped or k8s_ containers (only counted) go by image/env.
func (c *Container) IsPostgres() bool {
	if c.Running && !c.Kubernetes {
		return c.HasPgProc || c.Unverified && looksLikePostgres(c.Image, c.Env)
	}
	return looksLikePostgres(c.Image, c.Env)
}

// looksLikePostgres: image or env hints at Postgres — only to pick which
// containers are worth checking, not a verdict; shared by Docker and pods.
func looksLikePostgres(image string, env map[string]string) bool {
	return imageLooksLikePostgres(image) || envLooksLikePostgres(env)
}

func imageLooksLikePostgres(image string) bool {
	img := strings.ToLower(image)
	for _, k := range []string{"postgres", "postgis", "timescale", "pgvector", "supabase/postgres", "bitnami/postgresql"} {
		if strings.Contains(img, k) {
			return true
		}
	}
	return false
}

// envLooksLikePostgres: only the Postgres/Bitnami image variables. The POSTGRES_
// prefix is not used because Kubernetes injects POSTGRES_SERVICE_HOST… into every
// pod in the namespace. App pods reaching the DB carry these too: the weakest hint.
func envLooksLikePostgres(env map[string]string) bool {
	for _, k := range []string{"POSTGRES_PASSWORD", "POSTGRES_USER", "POSTGRES_DB", "POSTGRESQL_PASSWORD", "POSTGRESQL_USERNAME", "POSTGRESQL_DATABASE", "PGDATA"} {
		if _, ok := env[k]; ok {
			return true
		}
	}
	return false
}

// DefaultUser is the bootstrap superuser of the container (ADR-0017):
// POSTGRES_USER, supabase_admin for Supabase images, otherwise postgres.
// Bitnami's POSTGRESQL_USERNAME is an ordinary user owning the app's
// database; its superuser is postgres.
func (c *Container) DefaultUser() string {
	return defaultUser(c.Image, c.Env)
}

func defaultUser(image string, env map[string]string) string {
	if u := env["POSTGRES_USER"]; u != "" {
		return u
	}
	if strings.Contains(strings.ToLower(image), "supabase/postgres") {
		return "supabase_admin"
	}
	return "postgres"
}

// IsBitnami: a Bitnami PostgreSQL image, or one configured with its variables.
func (c *Container) IsBitnami() bool {
	if strings.Contains(strings.ToLower(c.Image), "bitnami") {
		return true
	}
	for _, k := range []string{"POSTGRESQL_USERNAME", "POSTGRESQL_PASSWORD", "POSTGRESQL_DATABASE", "POSTGRESQL_POSTGRES_PASSWORD", "POSTGRESQL_POSTGRES_PASSWORD_FILE"} {
		if _, ok := c.Env[k]; ok {
			return true
		}
	}
	return false
}

// SuperuserPassword is the password Bitnami was told to give postgres:
// POSTGRESQL_POSTGRES_PASSWORD, or the file POSTGRESQL_POSTGRES_PASSWORD_FILE
// read inside the container. from names the variable; both empty when the
// container sets none. Bitnami applies it only when the volume is created, so
// it may no longer be the password in use.
func (d Docker) SuperuserPassword(ctx context.Context, c *Container) (pw, from string, err error) {
	if v := c.Env["POSTGRESQL_POSTGRES_PASSWORD"]; v != "" {
		return v, "POSTGRESQL_POSTGRES_PASSWORD", nil
	}
	file := c.Env["POSTGRESQL_POSTGRES_PASSWORD_FILE"]
	if file == "" {
		return "", "", nil
	}
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"exec", c.Name, "cat", file}})
	if err != nil {
		return "", "POSTGRESQL_POSTGRES_PASSWORD_FILE", fmt.Errorf("read %s in %s: %w", file, c.Name, err)
	}
	return strings.TrimRight(string(res.Stdout), "\r\n"), "POSTGRESQL_POSTGRES_PASSWORD_FILE", nil
}

// IsSupabase reports whether the image is supabase/postgres (its pg_hba differs from the official image).
func (c *Container) IsSupabase() bool {
	return strings.Contains(strings.ToLower(c.Image), "supabase/postgres")
}

// Docker calls the docker CLI through Shell.
type Docker struct {
	Shell Shell
}

// Version checks that docker works (client and daemon).
func (d Docker) Version(ctx context.Context) error {
	_, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}})
	return err
}

type psLine struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

type inspectEntry struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	} `json:"State"`
	Config struct {
		Image string   `json:"Image"`
		Env   []string `json:"Env"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIP, HostPort string } `json:"Ports"`
		Networks map[string]struct{ IPAddress string }          `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ListPostgresContainers lists every container (stopped ones too) that looks like
// Postgres: docker ps -a → docker inspect → docker top for running containers.
func (d Docker) ListPostgresContainers(ctx context.Context) ([]*Container, error) {
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"ps", "-a", "--no-trunc", "--format", "{{json .}}"}})
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	var ids []string
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var l psLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.ID == "" {
			continue
		}
		ids = append(ids, l.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	res, err = d.Shell.Run(ctx, Cmd{Name: "docker", Args: append([]string{"inspect"}, ids...)})
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var entries []inspectEntry
	if err := json.Unmarshal(res.Stdout, &entries); err != nil {
		return nil, fmt.Errorf("docker inspect: parse JSON: %w", err)
	}
	var out []*Container
	for _, e := range entries {
		c := containerFromInspect(e)
		if c.Running && !c.Kubernetes {
			has, err := d.hasPostgresProcess(ctx, c.ID)
			c.HasPgProc, c.Unverified = has, err != nil
		}
		if c.IsPostgres() {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func containerFromInspect(e inspectEntry) *Container {
	c := &Container{
		ID:      e.ID,
		Name:    strings.TrimPrefix(e.Name, "/"),
		Image:   e.Config.Image,
		Running: e.State.Running,
		Status:  e.State.Status,
		Env:     map[string]string{},
		Port:    5432,
	}
	c.Kubernetes = strings.HasPrefix(c.Name, "k8s_")
	for _, kv := range e.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		c.Env[k] = v
	}
	if p := c.Env["PGPORT"]; p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.Port = n
		}
	}
	for portProto, binds := range e.NetworkSettings.Ports {
		inner, _, _ := strings.Cut(portProto, "/")
		n, err := strconv.Atoi(inner)
		if err != nil || n != c.Port {
			continue
		}
		seen := map[int]bool{}
		for _, b := range binds {
			hp, err := strconv.Atoi(b.HostPort)
			if err == nil && !seen[hp] {
				seen[hp] = true
				c.HostPorts = append(c.HostPorts, hp)
			}
		}
	}
	sort.Ints(c.HostPorts)
	for n := range e.NetworkSettings.Networks {
		c.Networks = append(c.Networks, n)
	}
	sort.Strings(c.Networks)
	return c
}

// hasPostgresProcess reads docker top; `postgres` in the last column (CMD) is enough.
// A docker top error → could not check (distinct from "none").
func (d Docker) hasPostgresProcess(ctx context.Context, id string) (bool, error) {
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"top", id}})
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(res.Stdout), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[len(f)-1], "postgres") {
			return true, nil
		}
		// `postgres: checkpointer` splits into two fields; also search the whole line.
		if strings.Contains(line, " postgres") {
			return true, nil
		}
	}
	return false, nil
}

// HasTool checks that the container has a command (pg_dump…) to pick the docker exec runner.
func (d Docker) HasTool(ctx context.Context, container, tool string) bool {
	_, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"exec", container, "sh", "-c", "command -v " + tool}})
	return err == nil
}

// ContainerState returns the container's status (running, exited…); a missing
// container is an error that says so.
func (d Docker) ContainerState(ctx context.Context, container string) (string, error) {
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"inspect", "--format", "{{.State.Status}}", container}})
	if err != nil {
		if strings.Contains(err.Error(), "No such") {
			return "", fmt.Errorf("container %s does not exist\nFix: docker ps -a to find it; if it was renamed, bracer remove <app>, then bracer init <app>", container)
		}
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// ContainerUser is the superuser to connect as inside a running container: its
// POSTGRES_USER (or Bitnami/Supabase equivalent), otherwise postgres.
func (d Docker) ContainerUser(ctx context.Context, container string) (string, error) {
	state, err := d.ContainerState(ctx, container)
	if err != nil {
		return "", err
	}
	if state != "running" {
		return "", fmt.Errorf("container %s is not running (%s)\nFix: docker start %s", container, state, container)
	}
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"inspect", "--format", "{{.Config.Image}}\t{{json .Config.Env}}", container}})
	if err != nil {
		return "", err
	}
	image, envJSON, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\t")
	var list []string
	_ = json.Unmarshal([]byte(envJSON), &list)
	env := map[string]string{}
	for _, kv := range list {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return defaultUser(image, env), nil
}

// ImageOfficialFamily: the image is the official `postgres` image or built on it
// (postgis, timescaledb, pgvector, a user's own image built FROM postgres) — the
// evidence is Entrypoint `docker-entrypoint.sh` and Cmd `postgres` in the image
// config (spec init § Flow 11b). Only such images work with the way drill runs
// the container (the temporary server during initdb opens only the socket,
// POSTGRES_PASSWORD, trust over the socket). Image not present locally → error.
func (d Docker) ImageOfficialFamily(ctx context.Context, image string) (bool, error) {
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"image", "inspect", "--format", "{{json .Config.Entrypoint}}\t{{json .Config.Cmd}}", image}})
	if err != nil {
		return false, fmt.Errorf("docker image inspect %s: %w", image, err)
	}
	ep, cmd, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\t")
	var entrypoint, command []string
	if err := json.Unmarshal([]byte(ep), &entrypoint); err != nil || json.Unmarshal([]byte(cmd), &command) != nil {
		return false, fmt.Errorf("docker image inspect %s: unexpected output %q", image, res.Stdout)
	}
	return len(entrypoint) > 0 && strings.HasSuffix(entrypoint[0], "docker-entrypoint.sh") && len(command) == 1 && command[0] == "postgres", nil
}

// ImageExtensions lists the extensions an image has (*.control files in the
// image's sharedir, no server needed), pulling the image if absent. Evidence for
// init to know whether drill's default image has all the extensions (spec init § 11b).
func (d Docker) ImageExtensions(ctx context.Context, image string) (map[string]bool, error) {
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"run", "--rm", "--network", "none", image, "sh", "-c", `ls "$(pg_config --sharedir)/extension"`}})
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %w", image, err)
	}
	out := map[string]bool{}
	for _, f := range strings.Fields(string(res.Stdout)) {
		if name, ok := strings.CutSuffix(f, ".control"); ok {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("docker run %s: no extension control files found in pg_config --sharedir", image)
	}
	return out, nil
}

// Endpoint is the Docker endpoint the CLI talks to: DOCKER_HOST when set,
// otherwise the endpoint of the current context (DOCKER_CONTEXT or the
// configured one), as `docker context inspect` reports it.
func (d Docker) Endpoint(ctx context.Context, dockerHost string) (string, error) {
	if dockerHost != "" {
		return dockerHost, nil
	}
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// Info checks that the daemon answers (`docker info`); the error holds docker's own message.
func (d Docker) Info(ctx context.Context) error {
	_, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"info", "--format", "{{.ServerVersion}}"}})
	return err
}

// Pull pulls an image unless it is present already; init pulls the images the
// runner and drill will need while someone is at the screen (spec init § Flow 3).
func (d Docker) Pull(ctx context.Context, image string) error {
	if _, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"image", "inspect", "--format", "{{.Id}}", image}}); err == nil {
		return nil
	}
	if _, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"pull", "--quiet", image}}); err != nil {
		return fmt.Errorf("docker pull %s: %w", image, err)
	}
	return nil
}
