package drill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// fakeKube fakes kubectl for the pod runtime, reusing fakeDocker's Postgres
// tools (psql, pg_restore…) for what runs inside the pod. Docker is absent.
type fakeKube struct {
	*fakeDocker
	canI         map[string]string // "create pods" → yes | no | ERR
	orphanNames  []string          // what delete -l returns
	manifests    [][]byte          // what create -f - received
	pods         map[string]*fakePod
	statusAfter  int // get pod calls before the container is Running
	getCalls     int
	pullError    string // waiting reason reported instead of Running, e.g. ImagePullBackOff
	deleted      []string
	isolated     bool   // the probe times out (NetworkPolicy enforced)
	probeOut     string // overrides the probe output when set
	dfAvailKBPod int64  // -1: df fails
	failCreate   bool
}

type fakePod struct {
	ns, name string
	manifest map[string]interface{}
}

func newFakeKube() *fakeKube {
	f := newFakeDocker()
	f.noDocker = true
	return &fakeKube{fakeDocker: f, canI: map[string]string{"create pods": "yes", "create networkpolicies.networking.k8s.io": "yes"},
		pods: map[string]*fakePod{}, isolated: true, dfAvailKBPod: 1 << 30}
}

func (f *fakeKube) Run(ctx context.Context, c pg.Cmd) (pg.Result, error) {
	if c.Name == "docker" {
		return f.fakeDocker.Run(ctx, c)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "kubectl "+strings.Join(c.Args, " "))
	if c.Name != "kubectl" {
		return pg.Result{}, fmt.Errorf("fake: not kubectl: %s", c.Name)
	}
	a := c.Args
	switch a[0] {
	case "auth": // auth can-i <verb> <resource> -n <ns>
		switch ans := f.canI[a[2]+" "+a[3]]; ans {
		case "yes":
			return pg.Result{Stdout: []byte("yes\n")}, nil
		case "no":
			return pg.Result{Stdout: []byte("no\n")}, &pg.ExitError{Cmd: "kubectl auth", Code: 1}
		default:
			return pg.Result{}, &pg.ExitError{Cmd: "kubectl auth", Code: 1, Stderr: []byte("The connection to the server 127.0.0.1:6443 was refused")}
		}
	case "delete":
		joined := strings.Join(a, " ")
		if strings.Contains(joined, " -l ") {
			if !strings.Contains(joined, "pod,networkpolicy -n default -l "+Label+"=shop,"+RoleLabel+"=drill") {
				return pg.Result{}, fmt.Errorf("fake: orphan cleanup must select by app and role label in the namespace: %v", a)
			}
			f.deleted = append(f.deleted, f.orphanNames...)
			if len(f.orphanNames) == 0 {
				return pg.Result{Stdout: []byte("No resources found\n")}, nil
			}
			return pg.Result{Stdout: []byte(strings.Join(f.orphanNames, "\n"))}, nil
		}
		for _, r := range a {
			if kind, name, ok := strings.Cut(r, "/"); ok && (kind == "pod" || kind == "networkpolicy") {
				f.deleted = append(f.deleted, r)
				delete(f.pods, name)
				delete(f.containers, name)
			}
		}
		return pg.Result{}, nil
	case "create":
		if len(a) != 3 || a[1] != "-f" || a[2] != "-" {
			return pg.Result{}, fmt.Errorf("fake: create must read a manifest from stdin: %v", a)
		}
		var list struct {
			Items []map[string]interface{} `json:"items"`
		}
		data := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := c.Stdin.Read(buf)
			data = append(data, buf[:n]...)
			if err != nil {
				break
			}
		}
		f.manifests = append(f.manifests, data)
		if err := json.Unmarshal(data, &list); err != nil {
			return pg.Result{}, fmt.Errorf("fake: manifest is not JSON: %v", err)
		}
		if f.failCreate {
			return pg.Result{}, &pg.ExitError{Cmd: "kubectl create", Code: 1, Stderr: []byte(`pods "x" is forbidden: violates PodSecurity "restricted:latest"`)}
		}
		for _, item := range list.Items {
			meta := item["metadata"].(map[string]interface{})
			name := meta["name"].(string)
			if item["kind"] == "Pod" {
				f.pods[name] = &fakePod{ns: meta["namespace"].(string), name: name, manifest: item}
				f.containers[name] = &fakeContainer{image: "pod", dbs: map[string][]fakeTable{"postgres": nil}}
			}
		}
		return pg.Result{}, nil
	case "get": // get pod -n ns name -o json
		f.getCalls++
		name := a[4]
		if _, ok := f.pods[name]; !ok {
			return pg.Result{}, &pg.ExitError{Cmd: "kubectl get", Code: 1, Stderr: []byte("NotFound")}
		}
		state := `{"running":{}}`
		phase := "Running"
		switch {
		case f.pullError != "":
			state, phase = `{"waiting":{"reason":"`+f.pullError+`","message":"Back-off pulling image \"nope\""}}`, "Pending"
		case f.getCalls <= f.statusAfter:
			state, phase = `{"waiting":{"reason":"ContainerCreating"}}`, "Pending"
		}
		return pg.Result{Stdout: []byte(`{"status":{"phase":"` + phase + `","conditions":[{"type":"PodScheduled","status":"True"}],"containerStatuses":[{"name":"postgres","state":` + state + `}]}}`)}, nil
	case "logs":
		return pg.Result{Stdout: []byte("fake pod log line\n")}, nil
	case "exec":
		return f.exec(a[1:], c)
	}
	return pg.Result{}, fmt.Errorf("fake: unexpected kubectl %v", a)
}

// exec: [-i] -n ns pod -c ctr -- (env K=V... tool args... | sh -c script)
func (f *fakeKube) exec(a []string, c pg.Cmd) (pg.Result, error) {
	i := 0
	if a[i] == "-i" {
		i++
	}
	if a[i] != "-n" {
		return pg.Result{}, fmt.Errorf("fake: exec must name the namespace: %v", a)
	}
	ns, name := a[i+1], a[i+2]
	i += 3
	if a[i] != "-c" || a[i+1] != "postgres" {
		return pg.Result{}, fmt.Errorf("fake: exec must target the postgres container: %v", a)
	}
	i += 2
	if a[i] != "--" {
		return pg.Result{}, fmt.Errorf("fake: exec expects --: %v", a)
	}
	i++
	pod := f.pods[name]
	if pod == nil || pod.ns != ns {
		return pg.Result{}, &pg.ExitError{Cmd: "kubectl exec", Code: 1, Stderr: []byte(`Error from server (NotFound): pods "` + name + `" not found`)}
	}
	if f.getCalls <= f.statusAfter {
		return pg.Result{}, &pg.ExitError{Cmd: "kubectl exec", Code: 1, Stderr: []byte("error: unable to upgrade connection: container not found (\"postgres\")")}
	}
	if a[i] == "sh" {
		script := a[i+2]
		switch {
		case strings.Contains(script, "KUBERNETES_SERVICE_HOST"):
			if f.probeOut != "" {
				return pg.Result{Stdout: []byte(f.probeOut)}, nil
			}
			if f.isolated {
				// k3s (kube-router) rejects; Calico/Cilium drop → "timeout expired".
				return pg.Result{Stdout: []byte("psql: error: connection to server at \"10.43.0.1\", port 443 failed: Connection refused\n\tIs the server running on that host and accepting TCP/IP connections?\nbracer-exit=2\n")}, nil
			}
			return pg.Result{Stdout: []byte("psql: error: connection to server at \"10.43.0.1\", port 443 failed: server closed the connection unexpectedly\nbracer-exit=2\n")}, nil
		}
		return pg.Result{}, fmt.Errorf("fake: unexpected script %q", script)
	}
	if a[i] != "env" {
		return pg.Result{}, fmt.Errorf("fake: exec expects env: %v", a)
	}
	i++
	var db string
	for i < len(a) && strings.Contains(a[i], "=") {
		if k, v, _ := strings.Cut(a[i], "="); k == "PGDATABASE" {
			db = v
		} else if k == "PGPASSWORD" {
			return pg.Result{}, fmt.Errorf("fake: password in argv")
		}
		i++
	}
	tool, args := a[i], a[i+1:]
	if tool == "df" {
		if args[len(args)-1] != podDataDir {
			return pg.Result{}, fmt.Errorf("fake: df must measure %s: %v", podDataDir, args)
		}
		if f.dfAvailKBPod < 0 {
			return pg.Result{}, &pg.ExitError{Cmd: "df", Code: 1, Stderr: []byte("df: not found")}
		}
		if c.Stdout != nil {
			fmt.Fprintf(c.Stdout, "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 100 1 %d 1%% %s\n", f.dfAvailKBPod, podDataDir)
		}
		return pg.Result{}, nil
	}
	return f.tool(f.containers[name], tool, args, db, c)
}

func TestKubeDrillPassAndCleanup(t *testing.T) {
	f := newFakeKube()
	f.readyAfter = 2
	f.statusAfter = 2
	f.orphanNames = []string{"pod/bracer-drill-shop-old", "networkpolicy.networking.k8s.io/bracer-drill-shop-old"}
	f.checks["select 1 from orders limit 1"] = "1\n"
	st := newMemStore()
	seed(t, st, "")
	cfg := baseCfg()
	cfg.Drill = &config.Drill{Memory: "2g", CPUs: "0.5", Checks: []config.Check{{Name: "orders exist", SQL: "select 1 from orders limit 1"}}}

	res, err := Run(context.Background(), cfg, depsFor(f, st))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Runtime != "kube" || len(res.Instances) != 1 {
		t.Fatalf("result: %+v", res)
	}
	ir := res.Instances[0]
	if ir.Image != "postgres:17-alpine" || ir.Pod != "" || ir.Container != "" {
		t.Errorf("instance: %+v", ir)
	}
	dr := ir.Databases[0]
	if !dr.OK || dr.TablesFound != 3 || len(dr.Checks) != 1 || !dr.Checks[0].OK {
		t.Errorf("db: %+v", dr)
	}
	if ir.Disk == nil || !ir.Disk.Checked || !strings.Contains(ir.Disk.DataDirectory, podDataDir+" in pod default/bracer-drill-shop-") {
		t.Errorf("disk: %+v", ir.Disk)
	}
	// Manifest: NetworkPolicy deny-all first, then the pod with emptyDir, restartPolicy Never, limits, labels.
	if len(f.manifests) != 1 {
		t.Fatalf("manifests: %d", len(f.manifests))
	}
	var list struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(f.manifests[0], &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("manifest: %v %s", err, f.manifests[0])
	}
	if list.Items[0]["kind"] != "NetworkPolicy" || list.Items[1]["kind"] != "Pod" {
		t.Errorf("order: %v %v", list.Items[0]["kind"], list.Items[1]["kind"])
	}
	m := string(f.manifests[0])
	for _, want := range []string{
		`"policyTypes":["Ingress","Egress"]`, `"podSelector":{"matchLabels":{"dev.bracer.app":"shop","dev.bracer.role":"drill"}}`,
		`"restartPolicy":"Never"`, `"emptyDir":{}`, `"mountPath":"/var/lib/postgresql"`, `"limits":{"cpu":"0.5","memory":"2Gi"}`,
		`"automountServiceAccountToken":false`, `"image":"postgres:17-alpine"`, `"namespace":"default"`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %s:\n%s", want, m)
		}
	}
	if strings.Contains(m, `"ingress"`) || strings.Contains(m, `"egress"`) {
		t.Errorf("deny-all policy must have no rules:\n%s", m)
	}
	// Orphans first, then the pod and its policy are removed; nothing left.
	if len(f.deleted) != 4 || f.deleted[0] != "pod/bracer-drill-shop-old" || !strings.HasPrefix(f.deleted[2], "pod/bracer-drill-shop-") || !strings.HasPrefix(f.deleted[3], "networkpolicy/bracer-drill-shop-") {
		t.Errorf("deleted: %v", f.deleted)
	}
	if len(f.pods) != 0 {
		t.Errorf("pods left: %v", f.pods)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "leftover drill pod/bracer-drill-shop-old") {
		t.Errorf("warnings: %v", res.Warnings)
	}
	joined := strings.Join(f.calls, "\n")
	if strings.Contains(joined, "docker run") || !strings.Contains(joined, "pg_restore -d dbname='shop'") || !strings.Contains(joined, "pg_isready -h 127.0.0.1") {
		t.Errorf("calls:\n%s", joined)
	}
}

func TestKubeDrillNotIsolatedWarns(t *testing.T) {
	f := newFakeKube()
	f.isolated = false
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), depsFor(f, st))
	if err != nil || !res.OK {
		t.Fatalf("%v %+v", err, res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "not isolated from the network") {
		t.Errorf("warnings: %v", res.Warnings)
	}
	f = newFakeKube()
	f.probeOut = "bracer-no-host\n"
	res, _ = Run(context.Background(), baseCfg(), depsFor(f, st))
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "could not verify") {
		t.Errorf("warnings: %v", res.Warnings)
	}
}

func TestKubeDrillKeepPrintsPod(t *testing.T) {
	f := newFakeKube()
	st := newMemStore()
	seed(t, st, "")
	d := depsFor(f, st)
	d.Keep = true
	res, err := Run(context.Background(), baseCfg(), d)
	if err != nil {
		t.Fatal(err)
	}
	ir := res.Instances[0]
	if !strings.HasPrefix(ir.Pod, "default/bracer-drill-shop-") || ir.Kubectl != "kubectl" || len(f.pods) != 1 || len(f.deleted) != 0 {
		t.Errorf("--keep: %+v deleted=%v", ir, f.deleted)
	}
}

func TestKubeDrillPodFailures(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")

	// Image pull error: fails at once, pod removed.
	f := newFakeKube()
	f.pullError = "ImagePullBackOff"
	res, _ := Run(context.Background(), baseCfg(), depsFor(f, st))
	ir := res.Instances[0]
	if res.OK || !strings.Contains(ir.Error, "ImagePullBackOff") || !strings.Contains(ir.Error, "Back-off pulling image") || !strings.Contains(ir.Error, "drill.image") {
		t.Errorf("pull: %+v", ir)
	}
	if len(f.pods) != 0 || f.getCalls != 1 {
		t.Errorf("pod must be removed without waiting: pods=%v getCalls=%d", f.pods, f.getCalls)
	}

	// Pending too long: the status is in the message.
	f = newFakeKube()
	f.statusAfter = 1000
	d := depsFor(f, st)
	d.StartTimeout = 5 * time.Second // the fake clock advances 1 s per Now()
	res, _ = Run(context.Background(), baseCfg(), d)
	ir = res.Instances[0]
	if res.OK || !strings.Contains(ir.Error, "not running after 5 s") || !strings.Contains(ir.Error, "ContainerCreating") || len(f.pods) != 0 {
		t.Errorf("pending: %+v pods=%v", ir, f.pods)
	}

	// Running but Postgres never ready: pod logs in the message.
	f = newFakeKube()
	f.readyAfter = 1000
	d = depsFor(f, st)
	d.ReadyTimeout = 5 * time.Second
	res, _ = Run(context.Background(), baseCfg(), d)
	ir = res.Instances[0]
	if res.OK || !strings.Contains(ir.Error, "not ready after") || !strings.Contains(ir.Error, "fake pod log line") || len(f.pods) != 0 {
		t.Errorf("not ready: %+v", ir)
	}

	// Short on disk in the emptyDir.
	f = newFakeKube()
	f.dfAvailKBPod = 0
	res, _ = Run(context.Background(), baseCfg(), depsFor(f, st))
	ir = res.Instances[0]
	if res.OK || !strings.Contains(ir.Error, "not enough disk space") || len(f.pods) != 0 {
		t.Errorf("disk: %+v", ir)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "pg_restore") {
		t.Error("nothing may be restored when disk is short")
	}

	// kubectl create refused.
	f = newFakeKube()
	f.failCreate = true
	res, _ = Run(context.Background(), baseCfg(), depsFor(f, st))
	if res.OK || !strings.Contains(res.Instances[0].Error, "PodSecurity") || len(f.deleted) != 2 {
		t.Errorf("create: %+v deleted=%v", res.Instances[0], f.deleted)
	}
}

func TestPickRuntime(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	ctx := context.Background()

	// Docker works → docker, even with kubectl around.
	f := newFakeKube()
	f.noDocker = false
	res, err := Run(ctx, baseCfg(), depsFor(f, st))
	if err != nil || res.Runtime != "docker" {
		t.Errorf("docker first: %v %+v", err, res)
	}
	// drill.runtime: kube forces the pod although Docker works.
	cfg := baseCfg()
	cfg.Drill = &config.Drill{Runtime: "kube", Namespace: "default"}
	f = newFakeKube()
	f.noDocker = false
	res, err = Run(ctx, cfg, depsFor(f, st))
	if err != nil || res.Runtime != "kube" {
		t.Errorf("forced kube: %v %+v", err, res)
	}
	// Neither: both reasons in the error, exit 1 (not a UsageError).
	f = newFakeKube()
	f.canI["create pods"] = "ERR"
	_, err = Run(ctx, baseCfg(), depsFor(f, st))
	if err == nil || !strings.Contains(err.Error(), "Docker is not available") || !strings.Contains(err.Error(), "cannot reach the cluster") || !strings.Contains(err.Error(), "kubectl cannot reach the cluster") || isUsage(err) {
		t.Errorf("neither: %v", err)
	}
	_, err = Run(ctx, baseCfg(), deps(newFakeDocker2NoDocker(), st))
	if err == nil || !strings.Contains(err.Error(), "kubectl was not found") || isUsage(err) {
		t.Errorf("no kubectl: %v", err)
	}
	// No permission in the namespace: picked automatically → plain error naming the namespace;
	// forced with drill.runtime → usage error.
	f = newFakeKube()
	f.canI["create pods"] = "no"
	_, err = Run(ctx, baseCfg(), depsFor(f, st))
	if err == nil || !strings.Contains(err.Error(), `may not create pods in namespace "default"`) || isUsage(err) {
		t.Errorf("no permission: %v", err)
	}
	_, err = Run(ctx, cfg, depsFor(f, st))
	if err == nil || !isUsage(err) || !strings.Contains(err.Error(), "drill.runtime is kube, but") {
		t.Errorf("forced without permission: %v", err)
	}
	f = newFakeKube()
	f.canI["create networkpolicies.networking.k8s.io"] = "no"
	_, err = Run(ctx, cfg, depsFor(f, st))
	if err == nil || !strings.Contains(err.Error(), "network policies") || !isUsage(err) {
		t.Errorf("no netpol permission: %v", err)
	}
	// drill.runtime: docker without Docker → usage error.
	cfg.Drill.Runtime = "docker"
	_, err = Run(ctx, cfg, depsFor(newFakeKube(), st))
	if err == nil || !isUsage(err) || !strings.Contains(err.Error(), "drill.runtime is docker, but") {
		t.Errorf("forced docker: %v", err)
	}
	// Custom namespace is used for the permission check and the pod.
	cfg.Drill = &config.Drill{Namespace: "backups"}
	f = newFakeKube()
	res, err = Run(ctx, cfg, depsFor(f, st))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "auth can-i create pods -n backups") || !strings.Contains(string(f.manifests[0]), `"namespace":"backups"`) || !strings.Contains(joined, "delete -n backups --ignore-not-found") {
		t.Errorf("namespace:\n%s\n%s", joined, f.manifests[0])
	}
}

func isUsage(err error) bool {
	var ue *UsageError
	return errors.As(err, &ue)
}

// newFakeDocker2NoDocker: a machine with neither Docker nor kubectl.
func newFakeDocker2NoDocker() *fakeDocker {
	f := newFakeDocker()
	f.noDocker = true
	return f
}

func TestKubeLimits(t *testing.T) {
	cases := map[string]string{"2g": "2Gi", "512m": "512Mi", "1024k": "1024Ki", "100": "100", "4G": "4Gi", "": ""}
	for in, want := range cases {
		got := kubeLimits(&config.Drill{Memory: in})["memory"]
		if got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if l := kubeLimits(nil); len(l) != 0 {
		t.Error(l)
	}
}

func TestIsolatedOutput(t *testing.T) {
	for out, want := range map[string]bool{
		`connection to server at "10.43.0.1", port 443 failed: timeout expired`:    true,
		`connection to server at "10.43.0.1", port 443 failed: Connection refused`: true,
		`connection to server at "10.43.0.1", port 443 failed: No route to host`:   true,
		`server closed the connection unexpectedly`:                                false,
		`expected authentication request from server, but received S`:              false,
		`received invalid response to SSL negotiation: H`:                          false,
	} {
		if isolatedOutput(out) != want {
			t.Errorf("%q → %v", out, !want)
		}
	}
}
