package drill

import (
	"context"
	"fmt"
	"io"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// runtime is where the temporary Postgres runs (spec drill § Data model): a
// Docker container, or a Kubernetes pod when the machine has kubectl but no Docker.
type runtime interface {
	// kind is the name written in the result: docker, kube.
	kind() string
	// cleanupOrphans removes what a previous run of this app left behind and
	// describes it ("2 container(s) and 1 volume(s)"); "" when there was nothing.
	cleanupOrphans(ctx context.Context, app string) (string, error)
	// start creates the temporary Postgres with the image, checks free disk
	// (needBytes), waits until pg_isready answers, and returns it; whatever it
	// created is removed on error. Disk and isolation findings go to ir and res.
	start(ctx context.Context, cfg *config.App, in config.Instance, image string, needBytes int64, ir *InstanceResult, res *Result, d Deps) (*tempPG, error)
	// remove tears the temporary Postgres down; failures only warn, with the manual command.
	remove(ctx context.Context, t *tempPG, res *Result, instance string)
	// keep records in ir how to reach the kept Postgres (--keep).
	keep(t *tempPG, ir *InstanceResult)
}

// UsageError is a drill.runtime/drill.namespace setting that cannot be honoured (exit 2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// tempPG is the temporary Postgres, whichever runtime runs it: tools run through
// run, connecting over the socket as user postgres.
type tempPG struct {
	run   pg.Runner
	image string
	name  string // container name, or namespace/pod

	volumes []string  // Docker: named volumes created for it
	pod     pg.PodRef // Kubernetes
}

func (t *tempPG) runner() pg.Runner { return t.run }

func (t *tempPG) conn(db string) pg.Conn { return pg.Conn{User: "postgres", Database: db} }

// client returns a pg.Client connected to database db of the temporary Postgres.
func (t *tempPG) client(db string) *pg.Client {
	return &pg.Client{Runner: t.runner(), Conn: t.conn(db)}
}

// pickRuntime (spec drill § Data model "Chọn runtime"): drill.runtime when set
// (not honourable → UsageError); otherwise Docker if `docker version` works, else
// kubectl if it reaches the cluster and may create pods and network policies in
// drill.namespace, else an error naming both.
func pickRuntime(ctx context.Context, cfg *config.App, d Deps) (runtime, error) {
	ns := cfg.DrillNamespace()
	switch cfg.DrillRuntime() {
	case config.DrillRuntimeDocker:
		if err := d.Tools.Docker().Version(ctx); err != nil {
			return nil, &UsageError{fmt.Sprintf("drill.runtime is docker, but Docker is not available: %v", err)}
		}
		return &dockerRuntime{sh: d.Shell}, nil
	case config.DrillRuntimeKube:
		k, why := kubeAvailable(ctx, d.Tools, ns)
		if k == nil {
			return nil, &UsageError{"drill.runtime is kube, but " + why}
		}
		return &kubeRuntime{kube: *k, ns: ns}, nil
	}
	dockerErr := d.Tools.Docker().Version(ctx)
	if dockerErr == nil {
		return &dockerRuntime{sh: d.Shell}, nil
	}
	if k, why := kubeAvailable(ctx, d.Tools, ns); k != nil {
		return &kubeRuntime{kube: *k, ns: ns}, nil
	} else {
		return nil, fmt.Errorf("a temporary Postgres needs Docker or kubectl: Docker is not available (%v), and %s; run drill on a machine that has one of them, with the same app.yaml and key file", dockerErr, why)
	}
}

// kubeAvailable finds kubectl and checks it may create pods and network policies
// in the namespace; nil with the reason otherwise.
func kubeAvailable(ctx context.Context, tools *pg.Tools, ns string) (*pg.Kube, string) {
	k := tools.Kubectl()
	if k == nil {
		return nil, "kubectl was not found (looked for kubectl and k3s on PATH)"
	}
	for _, resource := range []string{"pods", "networkpolicies.networking.k8s.io"} {
		ok, err := k.CanI(ctx, "create", resource, ns)
		if err != nil {
			msg := fmt.Sprintf("%s cannot reach the cluster: %v", k.Describe(), err)
			if hint := pg.KubeHint(err); hint != "" {
				msg += " (" + hint + ")"
			}
			return nil, msg
		}
		if !ok {
			if resource == "pods" {
				return nil, fmt.Sprintf("this kubeconfig may not create pods in namespace %q (`%s auth can-i create pods -n %s` says no); set drill.namespace in app.yaml to a namespace where it can", ns, k.Describe(), ns)
			}
			return nil, fmt.Sprintf("this kubeconfig may not create network policies in namespace %q (`%s auth can-i create networkpolicies -n %s` says no); a NetworkPolicy is what isolates the temporary Postgres from the network, so drill does not run without it", ns, k.Describe(), ns)
		}
	}
	return k, ""
}

// waitReady polls pg_isready over TCP loopback inside the temporary Postgres: the
// entrypoint's temporary server (during initdb) opens only the socket, so TCP being
// up means the real server runs. On timeout the last log lines are appended.
func waitReady(ctx context.Context, t *tempPG, d Deps, logs func(ctx context.Context) []byte) error {
	deadline := d.Now().Add(d.ReadyTimeout)
	for {
		if err := t.runner().Run(ctx, t.conn("postgres"), "pg_isready", []string{"-h", "127.0.0.1", "-U", "postgres"}, nil, io.Discard); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted while waiting for Postgres: %v", ctx.Err())
		}
		if !d.Now().Before(deadline) {
			return fmt.Errorf("Postgres in %s not ready after %.0f s; last log lines:\n%s", t.name, d.ReadyTimeout.Seconds(), restore.Tail(logs(ctx), restore.StderrTail))
		}
		select {
		case <-ctx.Done():
		case <-time.After(d.Poll):
		}
	}
}

// cleanupCtx is a fresh context for tearing down: Ctrl-C/timeout must still clean up.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
}

// reportDisk records the disk check and decides whether to go on (spec drill § Data model).
func reportDisk(dc restore.DiskCheck, in config.Instance, ir *InstanceResult, res *Result, d Deps) error {
	ir.Disk = &dc
	switch {
	case !dc.Checked:
		res.warn("%s: %s; the drill may fill the disk", in.Name, dc.Error)
	case !dc.Enough() && !d.SkipDiskCheck:
		return fmt.Errorf("not enough disk space for a drill: %s free at %s, about %s may be needed; free space or pass --skip-disk-check", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	case !dc.Enough():
		res.warn("%s: only %s free at %s, about %s may be needed (--skip-disk-check)", in.Name, humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	}
	return nil
}
