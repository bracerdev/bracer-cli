package drill

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// kubeRuntime runs the temporary Postgres as a pod created through kubectl, with
// a deny-all NetworkPolicy created before it (spec drill § Data model "Pod tạm").
type kubeRuntime struct {
	kube pg.Kube
	ns   string
}

const (
	// RoleLabel marks drill resources so that orphan cleanup never touches the
	// user's own Postgres pods, which may carry the app label too.
	RoleLabel = "dev.bracer.role"
	roleDrill = "drill"
	// podDataDir is where the emptyDir is mounted: the parent of PGDATA for every
	// major of the official image (/var/lib/postgresql/data up to 17,
	// /var/lib/postgresql/18/docker from 18); the entrypoint creates PGDATA in it.
	podDataDir = "/var/lib/postgresql"
	podGrace   = 10
	// defaultStartWait is how long the pod may take to reach Running (image pull on the node).
	defaultStartWait = 10 * time.Minute
)

func (r *kubeRuntime) kind() string { return config.DrillRuntimeKube }

func (r *kubeRuntime) selector(app string) string {
	return Label + "=" + app + "," + RoleLabel + "=" + roleDrill
}

func (r *kubeRuntime) cleanupOrphans(ctx context.Context, app string) (string, error) {
	names, err := r.kube.DeleteByLabel(ctx, r.ns, r.selector(app))
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", nil
	}
	return fmt.Sprintf("leftover drill %s in namespace %s", strings.Join(names, ", "), r.ns), nil
}

// manifest builds the List kubectl creates: the NetworkPolicy first, so the pod
// never runs without it, then the pod.
func (r *kubeRuntime) manifest(cfg *config.App, name, image, password string) ([]byte, error) {
	labels := map[string]string{Label: cfg.App, RoleLabel: roleDrill}
	meta := func() map[string]interface{} {
		return map[string]interface{}{"name": name, "namespace": r.ns, "labels": labels}
	}
	policy := map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   meta(),
		"spec": map[string]interface{}{
			"podSelector": map[string]interface{}{"matchLabels": labels},
			"policyTypes": []string{"Ingress", "Egress"},
		},
	}
	container := map[string]interface{}{
		"name":  "postgres",
		"image": image,
		// POSTGRES_PASSWORD is required by the official image and never used (socket
		// connection); it is random and ends up in the pod spec, which is acceptable.
		"env":          []map[string]string{{"name": "POSTGRES_PASSWORD", "value": password}},
		"volumeMounts": []map[string]string{{"name": "data", "mountPath": podDataDir}},
	}
	if limits := kubeLimits(cfg.Drill); len(limits) > 0 {
		container["resources"] = map[string]interface{}{"limits": limits}
	}
	pod := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   meta(),
		"spec": map[string]interface{}{
			"restartPolicy":                 "Never",
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"terminationGracePeriodSeconds": podGrace,
			"containers":                    []interface{}{container},
			"volumes":                       []map[string]interface{}{{"name": "data", "emptyDir": map[string]interface{}{}}},
		},
	}
	return json.Marshal(map[string]interface{}{"apiVersion": "v1", "kind": "List", "items": []interface{}{policy, pod}})
}

var reDockerMemory = regexp.MustCompile(`^([0-9]+)([bkmgBKMG]?)$`)

// kubeLimits turns drill.memory/drill.cpus (Docker spelling: 2g, 512m) into pod resource limits (2Gi, 512Mi).
func kubeLimits(dr *config.Drill) map[string]string {
	limits := map[string]string{}
	if dr == nil {
		return limits
	}
	if m := reDockerMemory.FindStringSubmatch(dr.Memory); m != nil {
		suffix := map[string]string{"": "", "b": "", "k": "Ki", "m": "Mi", "g": "Gi"}[strings.ToLower(m[2])]
		limits["memory"] = m[1] + suffix
	}
	if dr.CPUs != "" {
		limits["cpu"] = dr.CPUs
	}
	return limits
}

func (r *kubeRuntime) start(ctx context.Context, cfg *config.App, in config.Instance, image string, needBytes int64, ir *InstanceResult, res *Result, d Deps) (*tempPG, error) {
	name := containerPrefix + cfg.App + "-" + d.Rand(3)
	ref := pg.PodRef{Namespace: r.ns, Name: name, Container: "postgres"}
	t := &tempPG{run: pg.KubeExecRunner{Kube: r.kube, Pod: ref}, image: image, name: r.ns + "/" + name, pod: ref}
	manifest, err := r.manifest(cfg, name, image, d.Rand(16))
	if err != nil {
		return nil, err
	}
	start := d.Now()
	if err := r.kube.Create(ctx, manifest); err != nil {
		// kubectl may have created the policy before failing on the pod.
		r.remove(ctx, t, res, in.Name)
		return nil, fmt.Errorf("create pod %s with %s: %v", t.name, image, err)
	}
	d.Log.Info("temp postgres pod created", "instance", in.Name, "pod", t.name, "image", image)
	if err := r.waitRunning(ctx, t, d); err != nil {
		r.remove(ctx, t, res, in.Name)
		return nil, err
	}
	if took := d.Now().Sub(start); took > slowPull {
		res.warn("%s: starting pod %s took %.0f s (image pull?)", in.Name, t.name, took.Seconds())
	}
	// The emptyDir exists only once the pod runs: measure disk now, before restoring anything.
	if err := reportDisk(r.checkDisk(ctx, t, needBytes), in, ir, res, d); err != nil {
		r.remove(ctx, t, res, in.Name)
		return nil, err
	}
	if err := waitReady(ctx, t, d, func(ctx context.Context) []byte { return r.kube.Logs(ctx, r.ns, name, 20) }); err != nil {
		r.remove(ctx, t, res, in.Name)
		return nil, err
	}
	if msg := r.checkIsolation(ctx, ref); msg != "" {
		res.warn("%s: %s", in.Name, msg)
	}
	return t, nil
}

// waitRunning waits for the pod's container to run: image pull errors and a
// stopped container fail at once, Pending beyond StartTimeout fails with the
// scheduler's message (spec drill § Edge cases).
func (r *kubeRuntime) waitRunning(ctx context.Context, t *tempPG, d Deps) error {
	deadline := d.Now().Add(d.StartTimeout)
	var last pg.PodStatus
	for {
		st, err := r.kube.GetPodStatus(ctx, t.pod.Namespace, t.pod.Name)
		if err == nil {
			last = st
			switch {
			case st.Running:
				return nil
			case st.ImagePullFailed():
				return fmt.Errorf("pod %s cannot start with image %s: %s; check that the node can pull it, or set drill.image in app.yaml", t.name, t.image, st.Describe())
			case st.TerminatedReason != "" || st.Phase == "Failed" || st.Phase == "Succeeded":
				return fmt.Errorf("pod %s stopped before Postgres was ready: %s; last log lines:\n%s", t.name, st.Describe(), restore.Tail(r.kube.Logs(ctx, t.pod.Namespace, t.pod.Name, 20), restore.StderrTail))
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted while waiting for pod %s: %v", t.name, ctx.Err())
		}
		if !d.Now().Before(deadline) {
			return fmt.Errorf("pod %s not running after %.0f s: %s; free resources in the cluster (drill.memory/drill.cpus) or check why the image does not pull", t.name, d.StartTimeout.Seconds(), last.Describe())
		}
		select {
		case <-ctx.Done():
		case <-time.After(d.Poll):
		}
	}
}

// checkDisk: df on the emptyDir inside the pod (the node's disk under kubelet).
func (r *kubeRuntime) checkDisk(ctx context.Context, t *tempPG, needBytes int64) restore.DiskCheck {
	dc := restore.DiskCheck{NeedBytes: needBytes, DataDirectory: podDataDir + " in pod " + t.name}
	free, err := pg.KubeExecRunner{Kube: r.kube, Pod: t.pod}.DiskFree(ctx, podDataDir)
	if err != nil {
		dc.Error = fmt.Sprintf("could not check free disk space at %s (%v)", dc.DataDirectory, err)
		return dc
	}
	dc.Checked, dc.FreeBytes = true, free
	return dc
}

// isolationProbe connects from inside the pod to the API server — the one address
// every pod knows (kubelet always injects KUBERNETES_SERVICE_HOST, even with
// enableServiceLinks false), which always accepts TCP, with the one client every
// Postgres image has. Under a deny-all NetworkPolicy the connection times out
// (Calico, Cilium: DROP) or is refused (k3s kube-router: REJECT with tcp-reset —
// the API server itself never refuses its port, so a refusal is the policy).
// Any other outcome (TLS garbage, connection closed) means the packets got through.
const isolationProbe = `if [ -z "$KUBERNETES_SERVICE_HOST" ]; then echo bracer-no-host; exit 0; fi; PGCONNECT_TIMEOUT=3 psql -X -h "$KUBERNETES_SERVICE_HOST" -p "${KUBERNETES_SERVICE_PORT:-443}" -U postgres -d postgres -c 'select 1' 2>&1; echo "bracer-exit=$?"`

// checkIsolation returns a warning when the NetworkPolicy cannot be shown to be
// enforced; "" when the probe timed out as it should (spec drill § Data model).
func (r *kubeRuntime) checkIsolation(ctx context.Context, ref pg.PodRef) string {
	res, err := r.kube.Exec(ctx, ref, isolationProbe)
	out := string(res.Stdout) + string(res.Stderr)
	switch {
	case err != nil:
		return fmt.Sprintf("could not verify that the NetworkPolicy isolates the temporary Postgres (%v)", err)
	case strings.Contains(out, "bracer-no-host"), strings.Contains(out, "psql: not found"):
		return "could not verify that the NetworkPolicy isolates the temporary Postgres (no API server address or no psql in the image)"
	case isolatedOutput(out):
		return ""
	}
	return "the temporary Postgres is not isolated from the network: it could open a TCP connection to the API server although a deny-all NetworkPolicy is in place, so this cluster's CNI does not enforce NetworkPolicy (plain Flannel, or k3s with --disable-network-policy); the restored data is reachable from other pods while the drill runs"
}

// isolatedOutput: libpq never got a TCP connection to the API server.
func isolatedOutput(out string) bool {
	for _, s := range []string{"timeout expired", "timed out", "Connection refused", "No route to host", "Network is unreachable", "Network unreachable"} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

func (r *kubeRuntime) remove(ctx context.Context, t *tempPG, res *Result, instance string) {
	rmCtx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := r.kube.Delete(rmCtx, t.pod.Namespace, podGrace, "pod/"+t.pod.Name, "networkpolicy/"+t.pod.Name); err != nil {
		res.warn("%s: could not remove pod %s: %v; run: %s delete -n %s pod/%s networkpolicy/%s", instance, t.name, err, r.kube.Describe(), t.pod.Namespace, t.pod.Name, t.pod.Name)
	}
}

func (r *kubeRuntime) keep(t *tempPG, ir *InstanceResult) {
	ir.Pod = t.name
	ir.Kubectl = r.kube.Describe()
}
