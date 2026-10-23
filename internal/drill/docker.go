package drill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// dockerRuntime runs the temporary Postgres with `docker run --network none`
// and named, labeled volumes (spec drill § Data model).
type dockerRuntime struct {
	sh pg.Shell
}

// cleanupOrphans removes the app's drill containers and volumes left over from a
// previous run (same label; named volumes can be cleaned up, unlike anonymous ones).
func (r *dockerRuntime) cleanupOrphans(ctx context.Context, app string) (string, error) {
	containers, volumes := 0, 0
	out, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"ps", "-aq", "--filter", "label=" + Label + "=" + app, "--filter", "name=" + containerPrefix}})
	if err != nil {
		return "", err
	}
	if ids := strings.Fields(string(out.Stdout)); len(ids) > 0 {
		if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: append([]string{"rm", "-f", "-v"}, ids...)}); err != nil {
			return "", err
		}
		containers = len(ids)
	}
	out, err = r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"volume", "ls", "-q", "--filter", "label=" + Label + "=" + app, "--filter", "name=" + containerPrefix}})
	if err != nil {
		return "", err
	}
	if names := strings.Fields(string(out.Stdout)); len(names) > 0 {
		if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: append([]string{"volume", "rm"}, names...)}); err != nil {
			return "", err
		}
		volumes = len(names)
	}
	if containers+volumes == 0 {
		return "", nil
	}
	return fmt.Sprintf("%d leftover drill container(s) and %d volume(s)", containers, volumes), nil
}

// checkDisk measures the free disk where the drill will write (spec drill § Data model):
// df in a temporary container of the (already pulled) image with the drill volume
// mounted read-only — the very filesystem the restore will use, even when Docker
// runs in a VM, and without bind-mounting anything from the host into the image
// (Docker root holds the data of every other container). If that cannot run → df
// on the Docker root directly on the host; if both fail → Checked=false.
func (r *dockerRuntime) checkDisk(ctx context.Context, image, volume string, needBytes int64) restore.DiskCheck {
	dc := restore.DiskCheck{NeedBytes: needBytes}
	const mnt = "/mnt/bracer-disk"
	out, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"run", "--rm", "--network", "none", "-v", volume + ":" + mnt + ":ro", image, "df", "-Pk", mnt}})
	where := "volume " + volume
	if err != nil {
		info, ierr := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"info", "--format", "{{.DockerRootDir}}"}})
		dir := strings.TrimSpace(string(info.Stdout))
		if ierr != nil || dir == "" {
			dc.Error = fmt.Sprintf("could not check free disk space (df in %s: %v; docker info gave no root directory)", image, err)
			return dc
		}
		where = dir
		out, err = r.sh.Run(ctx, pg.Cmd{Name: "df", Args: []string{"-Pk", dir}})
	}
	dc.DataDirectory = where
	if err != nil {
		dc.Error = fmt.Sprintf("could not check free disk space at %s (%v)", where, err)
		return dc
	}
	free, err := pg.ParseDF(out.Stdout)
	if err != nil {
		dc.Error = fmt.Sprintf("could not read df output for %s: %v", where, err)
		return dc
	}
	dc.Checked, dc.FreeBytes = true, free
	return dc
}

// imageVolumes reads the image's VOLUME paths (pulling it if absent) so that
// named volumes can be mounted there, avoiding anonymous volumes.
func (r *dockerRuntime) imageVolumes(ctx context.Context, image string) ([]string, error) {
	inspect := func() ([]string, error) {
		out, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"image", "inspect", "-f", "{{json .Config.Volumes}}", image}})
		if err != nil {
			return nil, err
		}
		var vols map[string]struct{}
		if err := json.Unmarshal(bytes.TrimSpace(out.Stdout), &vols); err != nil {
			return nil, fmt.Errorf("parse image volumes: %w", err)
		}
		paths := make([]string, 0, len(vols))
		for p := range vols {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		return paths, nil
	}
	paths, err := inspect()
	if err == nil {
		return paths, nil
	}
	if _, perr := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"pull", "-q", image}}); perr != nil {
		return nil, fmt.Errorf("pull %s: %w", image, perr)
	}
	return inspect()
}

// start creates the temporary Postgres with image, bootstrap superuser user
// and the preload libraries, checks free disk (needBytes), waits until it
// answers, and returns it; whatever it created is removed on error.
func (r *dockerRuntime) start(ctx context.Context, cfg *config.App, in config.Instance, image, user string, preload []string, locksPer int, needBytes int64, ir *InstanceResult, res *Result, d Deps) (*tempPG, error) {
	// A named + labeled volume for each VOLUME of the image, so docker rm -v
	// leaves no copy of the data behind.
	paths, err := r.imageVolumes(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %v", image, err)
	}
	name := containerPrefix + cfg.App + "-" + d.Rand(3)
	t := &tempPG{run: pg.ExecRunner{Shell: r.sh, Container: name}, image: image, name: name, user: user}
	// POSTGRES_PASSWORD is required by the official image; never used (socket connection).
	// No --rm: a container that exits must stay for its logs.
	runArgs := []string{"run", "-d", "--network", "none", "--label", Label + "=" + cfg.App, "--name", name,
		"-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_USER=" + user, "-e", "POSTGRES_DB=postgres"}
	// Image declares no VOLUME: still create one volume (not mounted) to measure disk.
	if len(paths) == 0 {
		paths = []string{""}
	}
	for i, p := range paths {
		vol := name
		if i > 0 {
			vol = fmt.Sprintf("%s-%d", name, i)
		}
		if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"volume", "create", "--label", Label + "=" + cfg.App, vol}}); err != nil {
			r.removeVolumes(ctx, t.volumes, res, in.Name)
			return nil, fmt.Errorf("create volume %s: %v", vol, err)
		}
		t.volumes = append(t.volumes, vol)
		if p != "" {
			runArgs = append(runArgs, "-v", vol+":"+p)
		}
	}
	// Measure disk through the volume just created, before starting the container.
	if err := reportDisk(r.checkDisk(ctx, image, t.volumes[0], needBytes), ir); err != nil {
		r.removeVolumes(ctx, t.volumes, res, in.Name)
		return nil, err
	}
	start := d.Now()
	if cfg.Drill != nil {
		if cfg.Drill.Memory != "" {
			runArgs = append(runArgs, "--memory", cfg.Drill.Memory)
		}
	}
	runArgs = append(runArgs, image)
	if len(preload) > 0 {
		// The image entrypoint passes arguments starting with - to postgres.
		runArgs = append(runArgs, "-c", "shared_preload_libraries="+strings.Join(preload, ","))
	}
	if locksPer > 0 {
		runArgs = append(runArgs, "-c", fmt.Sprintf("max_locks_per_transaction=%d", locksPer))
	}
	if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: runArgs, Env: []string{"POSTGRES_PASSWORD=" + d.Rand(16)}}); err != nil {
		r.remove(ctx, t, res, in.Name)
		return nil, fmt.Errorf("start %s: %v", image, err)
	}
	if took := d.Now().Sub(start); took > slowPull {
		res.warn("%s: starting %s took %.0f s (image pull?)", in.Name, image, took.Seconds())
	}
	d.Log.Info("temp postgres started", "instance", in.Name, "container", name, "image", image)
	if err := r.waitReady(ctx, t, d); err != nil {
		r.remove(ctx, t, res, in.Name)
		return nil, err
	}
	return t, nil
}

// restart restarts the temporary Postgres (env-up changed a setting that
// only takes effect at start) and waits for it.
func (r *dockerRuntime) restart(ctx context.Context, t *tempPG, d Deps) error {
	if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"restart", t.name}}); err != nil {
		return fmt.Errorf("restart %s: %v", t.name, err)
	}
	return r.waitReady(ctx, t, d)
}

// waitReady polls pg_isready over TCP loopback inside the temporary Postgres: the
// entrypoint's temporary server (during initdb) opens only the socket, so TCP being
// up means the real server runs. A container that exited fails at once, with its logs.
func (r *dockerRuntime) waitReady(ctx context.Context, t *tempPG, d Deps) error {
	logs := func() string {
		out, _ := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"logs", "--tail", "20", t.name}})
		return restore.Tail(append(out.Stdout, out.Stderr...), restore.StderrTail)
	}
	deadline := d.Now().Add(d.ReadyTimeout)
	for {
		if err := t.runner().Run(ctx, t.conn("postgres"), "pg_isready", []string{"-h", "127.0.0.1", "-U", t.user}, nil, io.Discard); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted while waiting for Postgres: %v", ctx.Err())
		}
		out, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"inspect", "-f", "{{.State.Running}}", t.name}})
		if err != nil || strings.TrimSpace(string(out.Stdout)) != "true" {
			return fmt.Errorf("Postgres in %s (%s) stopped while starting; last log lines:\n%s", t.name, t.image, logs())
		}
		if !d.Now().Before(deadline) {
			return fmt.Errorf("Postgres in %s not ready after %.0f s; last log lines:\n%s", t.name, d.ReadyTimeout.Seconds(), logs())
		}
		select {
		case <-ctx.Done():
		case <-time.After(d.Poll):
		}
	}
}

func (r *dockerRuntime) remove(ctx context.Context, t *tempPG, res *Result, instance string) {
	rmCtx, cancel := cleanupCtx(ctx)
	defer cancel()
	if _, err := r.sh.Run(rmCtx, pg.Cmd{Name: "docker", Args: []string{"rm", "-f", "-v", t.name}}); err != nil {
		res.warn("%s: could not remove container %s: %v; run: docker rm -f -v %s", instance, t.name, err, t.name)
		return // the volumes are still attached to the container; remove them along with it
	}
	r.removeVolumes(rmCtx, t.volumes, res, instance)
}

// removeVolumes removes the drill's named volumes; on error it only warns, with the manual cleanup command.
func (r *dockerRuntime) removeVolumes(ctx context.Context, volumes []string, res *Result, instance string) {
	if len(volumes) == 0 {
		return
	}
	if _, err := r.sh.Run(ctx, pg.Cmd{Name: "docker", Args: append([]string{"volume", "rm"}, volumes...)}); err != nil {
		res.warn("%s: could not remove volume(s) %s: %v; run: docker volume rm %s", instance, strings.Join(volumes, " "), err, strings.Join(volumes, " "))
	}
}
