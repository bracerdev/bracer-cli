package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// PodRef identifies one container in one Kubernetes pod.
type PodRef struct {
	Namespace string
	Name      string
	Container string // empty: the pod's default container
}

// String is the form written in app.yaml: ns/pod[/container].
func (p PodRef) String() string {
	s := p.Namespace + "/" + p.Name
	if p.Container != "" {
		s += "/" + p.Container
	}
	return s
}

// ParsePodRef parses ns/pod[/container].
func ParsePodRef(s string) (PodRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return PodRef{}, fmt.Errorf("pod %q: expected namespace/pod or namespace/pod/container", s)
	}
	for _, p := range parts {
		if p == "" {
			return PodRef{}, fmt.Errorf("pod %q: empty segment", s)
		}
	}
	ref := PodRef{Namespace: parts[0], Name: parts[1]}
	if len(parts) == 3 {
		ref.Container = parts[2]
	}
	return ref, nil
}

// Pod is a Postgres container in a running pod.
type Pod struct {
	Ref        PodRef
	Image      string
	Env        map[string]string // literal values; env taken from a Secret has only its name
	Ready      bool
	HasPgProc  bool   // kubectl exec saw a postgres process (evidence)
	PgPID      string // PID of that process inside the container
	Unverified bool   // could not check the process (no sh, not ready yet)
	EnvOnly    bool   // the only hint is env variables (no image, port 5432 or PGDATA)
}

// DefaultUser is like Container.DefaultUser.
func (p *Pod) DefaultUser() string {
	return defaultUser(p.Image, p.Env)
}

// Kube calls kubectl through Shell. Cmd is ["kubectl"] or ["k3s", "kubectl"].
type Kube struct {
	Shell Shell
	Cmd   []string
}

// FindKubectl looks for `kubectl` on PATH, else `k3s kubectl`. Returns
// (nil, nil) when neither exists. A nil lookPath means exec.LookPath.
func FindKubectl(sh Shell, lookPath func(string) (string, error)) (*Kube, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath("kubectl"); err == nil {
		return &Kube{Shell: sh, Cmd: []string{"kubectl"}}, nil
	}
	if _, err := lookPath("k3s"); err == nil {
		return &Kube{Shell: sh, Cmd: []string{"k3s", "kubectl"}}, nil
	}
	return nil, nil
}

// Describe is the command name to print for the user.
func (k Kube) Describe() string { return strings.Join(k.Cmd, " ") }

func (k Kube) run(ctx context.Context, c Cmd) (Result, error) {
	c.Name = k.Cmd[0]
	c.Args = append(append([]string{}, k.Cmd[1:]...), c.Args...)
	return k.Shell.Run(ctx, c)
}

// podList is the needed part of `kubectl get pods -A -o json`.
type podList struct {
	Items []struct {
		Metadata struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
				Ports []struct {
					ContainerPort int `json:"containerPort"`
				} `json:"ports"`
				Env []struct {
					Name      string      `json:"name"`
					Value     string      `json:"value"`
					ValueFrom interface{} `json:"valueFrom"`
				} `json:"env"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name  string `json:"name"`
				Ready bool   `json:"ready"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// ListPostgresPods lists Postgres containers in Running pods across every
// namespace. Image/env/port only pick which containers are worth checking; the
// verdict is a `postgres` process seen via kubectl exec (an app pod that carries
// POSTGRES_USER to reach the DB has none). Cannot check → Unverified, still listed.
func (k Kube) ListPostgresPods(ctx context.Context) ([]*Pod, error) {
	res, err := k.run(ctx, Cmd{Args: []string{"get", "pods", "-A", "-o", "json"}})
	if err != nil {
		return nil, fmt.Errorf("%s get pods: %w", k.Describe(), err)
	}
	var list podList
	if err := json.Unmarshal(res.Stdout, &list); err != nil {
		return nil, fmt.Errorf("%s get pods: parse JSON: %w", k.Describe(), err)
	}
	var out []*Pod
	for _, item := range list.Items {
		if item.Status.Phase != "Running" {
			continue
		}
		ready := map[string]bool{}
		for _, cs := range item.Status.ContainerStatuses {
			ready[cs.Name] = cs.Ready
		}
		for _, c := range item.Spec.Containers {
			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			port5432 := false
			for _, p := range c.Ports {
				port5432 = port5432 || p.ContainerPort == 5432
			}
			_, pgdata := env["PGDATA"]
			strong := imageLooksLikePostgres(c.Image) || port5432 || pgdata
			if !strong && !envLooksLikePostgres(env) {
				continue
			}
			p := &Pod{
				Ref:     PodRef{Namespace: item.Metadata.Namespace, Name: item.Metadata.Name, Container: c.Name},
				Image:   c.Image,
				Env:     env,
				Ready:   ready[c.Name],
				EnvOnly: !strong,
			}
			if !p.Ready {
				p.Unverified = true
			} else if pid, err := k.PostgresProcess(ctx, p.Ref); err != nil {
				p.Unverified = true
			} else if pid == "" {
				continue
			} else {
				p.HasPgProc, p.PgPID = true, pid
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.String() < out[j].Ref.String() })
	return out, nil
}

// pgProcScript finds a process named postgres via /proc (no ps needed): prints
// the first PID found, or "none". A shell error (no sh) → exit ≠ 0.
const pgProcScript = `for f in /proc/[0-9]*/comm; do read -r n < "$f" 2>/dev/null || continue; if [ "$n" = postgres ]; then p=${f#/proc/}; echo ${p%/comm}; exit 0; fi; done; echo none`

// PostgresProcess asks whether the container has a postgres process: the PID if
// so, "" if not; an error when it cannot check (no sh, container not running).
func (k Kube) PostgresProcess(ctx context.Context, pod PodRef) (string, error) {
	res, err := k.run(ctx, Cmd{Args: k.execArgs(pod, pgProcScript)})
	if err != nil {
		return "", err
	}
	pid := strings.TrimSpace(string(res.Stdout))
	if pid == "none" || pid == "" {
		return "", nil
	}
	for _, c := range pid {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("unexpected output %q", pid)
		}
	}
	return pid, nil
}

// ContainerUser reads the user name worth trying from inside the container, for
// pods whose spec has no env values (Secret via envFrom/valueFrom): the resolved
// POSTGRES_USER or POSTGRESQL_USERNAME, else the OS user owning the postgres
// process (pid; self-built images usually give the user the same name). Reads
// only the name, never the password; error, empty or not name-shaped → "".
func (k Kube) ContainerUser(ctx context.Context, pod PodRef, pid string) string {
	script := `u="${POSTGRES_USER:-$POSTGRESQL_USERNAME}"; if [ -n "$u" ]; then printf %s "$u"; exit 0; fi`
	if pid != "" {
		script += `; set -- $(ls -ld /proc/` + pid + ` 2>/dev/null); printf %s "$3"`
	}
	res, err := k.run(ctx, Cmd{Args: k.execArgs(pod, script)})
	if err != nil {
		return ""
	}
	u := strings.TrimSpace(string(res.Stdout))
	if u == "" || u == "root" || strings.ContainsAny(u, " \t\n\"'") || strings.Trim(u, "0123456789") == "" {
		return "" // root is never the Postgres user; digits are a uid with no name mapped
	}
	return u
}

func (k Kube) execArgs(pod PodRef, script string) []string {
	args := []string{"exec", "-n", pod.Namespace, pod.Name}
	if pod.Container != "" {
		args = append(args, "-c", pod.Container)
	}
	return append(args, "--", "sh", "-c", script)
}

// HasTool checks that the container in the pod has a command (pg_dump…).
func (k Kube) HasTool(ctx context.Context, pod PodRef, tool string) bool {
	args := []string{"exec", "-n", pod.Namespace, pod.Name}
	if pod.Container != "" {
		args = append(args, "-c", pod.Container)
	}
	args = append(args, "--", "sh", "-c", "command -v "+tool)
	_, err := k.run(ctx, Cmd{Args: args})
	return err == nil
}

// KubeHint suggests how to fix a familiar kubectl error; empty if not recognized.
func KubeHint(err error) string {
	var ee *ExitError
	if !errors.As(err, &ee) {
		return ""
	}
	s := strings.ToLower(string(ee.Stderr))
	switch {
	case strings.Contains(s, "permission denied") && strings.Contains(s, "k3s.yaml"):
		return "k3s keeps its kubeconfig at /etc/rancher/k3s/k3s.yaml, readable by root only; run with sudo, or set KUBECONFIG to a readable copy"
	case strings.Contains(s, "permission denied"):
		return "the kubeconfig is not readable by this user; run with sudo or set KUBECONFIG"
	case strings.Contains(s, "connection refused"), strings.Contains(s, "no such host"), strings.Contains(s, "unable to connect"):
		return "kubectl cannot reach the cluster; check `kubectl config current-context`"
	}
	return ""
}

// ErrKubePassword: kubectl exec cannot pass a password without exposing it in argv.
var ErrKubePassword = errors.New("password authentication is not supported over kubectl exec; pods are reached over their Unix socket without a password, or point bracer at the service with a postgres:// URL")

// KubeExecRunner: `kubectl exec -i` into the pod's Postgres container, connecting
// over the Unix socket inside the container. Non-secret connection variables go
// through `env` in argv; a password is refused (ADR-0009).
type KubeExecRunner struct {
	Kube Kube
	Pod  PodRef
}

func (r KubeExecRunner) Describe() string {
	return r.Kube.Describe() + " exec " + r.Pod.String()
}

func (r KubeExecRunner) Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	_, err := r.RunStderr(ctx, conn, tool, args, stdin, stdout)
	return err
}

// RunStderr is like Run but also returns stderr, even when the command succeeds.
func (r KubeExecRunner) RunStderr(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	if conn.Password != "" {
		return nil, ErrKubePassword
	}
	kargs := []string{"exec", "-i", "-n", r.Pod.Namespace, r.Pod.Name}
	if r.Pod.Container != "" {
		kargs = append(kargs, "-c", r.Pod.Container)
	}
	kargs = append(kargs, "--", "env")
	kargs = append(kargs, conn.Env()...)
	kargs = append(kargs, tool)
	kargs = append(kargs, args...)
	res, err := r.Kube.run(ctx, Cmd{Args: kargs, Stdin: stdin, Stdout: stdout})
	return res.Stderr, err
}

// PodImage reads the image of the container in the pod (pod spec, evidence for
// init to fill drill.image — spec init § 11b); empty Container → first container.
func (k Kube) PodImage(ctx context.Context, ref PodRef) (string, error) {
	res, err := k.run(ctx, Cmd{Args: []string{"get", "pod", "-n", ref.Namespace, ref.Name, "-o", "json"}})
	if err != nil {
		return "", err
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(res.Stdout, &pod); err != nil {
		return "", fmt.Errorf("kubectl get pod: parse JSON: %w", err)
	}
	for _, c := range pod.Spec.Containers {
		if ref.Container == "" || c.Name == ref.Container {
			return c.Image, nil
		}
	}
	return "", fmt.Errorf("pod %s has no container %q", ref.Namespace+"/"+ref.Name, ref.Container)
}

// CanI asks `kubectl auth can-i <verb> <resource> -n <namespace>`: (true, nil)
// on "yes", (false, nil) on "no" (exit 1 with "no" on stdout), and an error when
// kubectl could not ask the cluster at all (no kubeconfig, unreachable…).
func (k Kube) CanI(ctx context.Context, verb, resource, namespace string) (bool, error) {
	res, err := k.run(ctx, Cmd{Args: []string{"auth", "can-i", verb, resource, "-n", namespace}})
	answer := strings.TrimSpace(string(res.Stdout))
	switch {
	case err == nil && answer == "yes":
		return true, nil
	case answer == "no":
		return false, nil
	case err != nil:
		return false, fmt.Errorf("%s auth can-i: %w", k.Describe(), err)
	}
	return false, fmt.Errorf("%s auth can-i: unexpected answer %q", k.Describe(), answer)
}

// PodStatus is what drill needs to know about its temporary pod while waiting
// for it (spec drill § Data model): the phase, why it is not scheduled, and the
// waiting/terminated state of its container.
type PodStatus struct {
	Phase             string // Pending, Running, Succeeded, Failed
	Scheduled         bool
	SchedulingMessage string // conditions[PodScheduled].message when not scheduled
	Running           bool   // the (first) container is running
	WaitingReason     string // containerStatuses[0].state.waiting.reason: ContainerCreating, ErrImagePull…
	WaitingMessage    string
	TerminatedReason  string // state.terminated.reason: Error, Completed…
	TerminatedMessage string
	TerminatedExit    int
}

// Describe is a one-line summary for an error message.
func (s PodStatus) Describe() string {
	switch {
	case s.TerminatedReason != "":
		return fmt.Sprintf("container exited (%s, exit %d): %s", s.TerminatedReason, s.TerminatedExit, orUnknownMsg(s.TerminatedMessage))
	case s.WaitingReason != "":
		return fmt.Sprintf("container is %s: %s", s.WaitingReason, orUnknownMsg(s.WaitingMessage))
	case !s.Scheduled && s.SchedulingMessage != "":
		return "pod is not scheduled: " + s.SchedulingMessage
	}
	return "pod is " + s.Phase
}

func orUnknownMsg(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no message)"
	}
	return strings.TrimSpace(s)
}

// ImagePullFailed reports a waiting reason that will not resolve by waiting longer.
func (s PodStatus) ImagePullFailed() bool {
	switch s.WaitingReason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
		return true
	}
	return false
}

// GetPodStatus reads the pod with kubectl get -o json.
func (k Kube) GetPodStatus(ctx context.Context, namespace, name string) (PodStatus, error) {
	res, err := k.run(ctx, Cmd{Args: []string{"get", "pod", "-n", namespace, name, "-o", "json"}})
	if err != nil {
		return PodStatus{}, err
	}
	var pod struct {
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				State struct {
					Running *struct{} `json:"running"`
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
					Terminated *struct {
						Reason   string `json:"reason"`
						Message  string `json:"message"`
						ExitCode int    `json:"exitCode"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(res.Stdout, &pod); err != nil {
		return PodStatus{}, fmt.Errorf("kubectl get pod: parse JSON: %w", err)
	}
	st := PodStatus{Phase: pod.Status.Phase}
	for _, c := range pod.Status.Conditions {
		if c.Type == "PodScheduled" {
			st.Scheduled = c.Status == "True"
			st.SchedulingMessage = c.Message
		}
	}
	if len(pod.Status.ContainerStatuses) > 0 {
		s := pod.Status.ContainerStatuses[0].State
		st.Running = s.Running != nil
		if s.Waiting != nil {
			st.WaitingReason, st.WaitingMessage = s.Waiting.Reason, s.Waiting.Message
		}
		if s.Terminated != nil {
			st.TerminatedReason, st.TerminatedMessage, st.TerminatedExit = s.Terminated.Reason, s.Terminated.Message, s.Terminated.ExitCode
		}
	}
	return st, nil
}

// Exec runs `sh -c script` in the pod's (first or named) container and returns its stdout.
func (k Kube) Exec(ctx context.Context, pod PodRef, script string) (Result, error) {
	return k.run(ctx, Cmd{Args: k.execArgs(pod, script)})
}

// Create sends a manifest (JSON or YAML) to `kubectl create -f -`.
func (k Kube) Create(ctx context.Context, manifest []byte) error {
	_, err := k.run(ctx, Cmd{Args: []string{"create", "-f", "-"}, Stdin: bytes.NewReader(manifest)})
	return err
}

// Delete removes the named resources (pod/x, networkpolicy/x) in the namespace,
// ignoring ones that are already gone; grace is the pod's termination grace period in seconds.
func (k Kube) Delete(ctx context.Context, namespace string, grace int, resources ...string) error {
	args := append([]string{"delete", "-n", namespace, "--ignore-not-found", "--grace-period=" + strconv.Itoa(grace)}, resources...)
	_, err := k.run(ctx, Cmd{Args: args})
	return err
}

// DeleteByLabel removes every pod and networkpolicy in the namespace matching
// the selector and returns the names deleted (kubectl -o name).
func (k Kube) DeleteByLabel(ctx context.Context, namespace, selector string) ([]string, error) {
	res, err := k.run(ctx, Cmd{Args: []string{"delete", "pod,networkpolicy", "-n", namespace, "-l", selector, "--ignore-not-found", "--grace-period=10", "-o", "name"}})
	if err != nil {
		return nil, err
	}
	// Only kind/name lines: with nothing to delete kubectl prints "No resources found".
	var names []string
	for _, f := range strings.Fields(string(res.Stdout)) {
		if strings.Contains(f, "/") {
			names = append(names, f)
		}
	}
	return names, nil
}

// Logs returns the last n lines of the pod's log.
func (k Kube) Logs(ctx context.Context, namespace, name string, n int) []byte {
	res, _ := k.run(ctx, Cmd{Args: []string{"logs", "-n", namespace, name, "--tail", strconv.Itoa(n)}})
	return append(res.Stdout, res.Stderr...)
}
