package pg

import "strings"

// DockerDown is why `docker info` failed for a user, told apart once for
// every place that says what to do about it: init's step 0 and restore
// (one step per line) and the drill's Fix line (one sentence).
type DockerDown struct {
	// Denied: the daemon runs but its socket refuses the user.
	Denied bool
	// Rootless: the daemon is the user's own (rootless Docker), not running.
	Rootless bool
	User     string
}

// WhyDockerDown classifies the error of `docker info` run as user against
// endpoint (DOCKER_HOST as the jobs use it; "" when not known). A rootless
// daemon is told by its socket under /run/user/, in endpoint or in docker's
// own message.
func WhyDockerDown(err error, user, endpoint string) DockerDown {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return DockerDown{
		Denied:   strings.Contains(msg, "permission denied"),
		Rootless: strings.HasPrefix(endpoint, "unix:///run/user/") || strings.Contains(msg, "unix:///run/user/"),
		User:     user,
	}
}

// Detail is the one line saying what is wrong; "" when only docker's own
// message says it.
func (d DockerDown) Detail() string {
	switch {
	case d.Denied:
		return d.User + " may not use Docker (permission denied on the Docker socket)"
	case d.Rootless:
		return "the rootless Docker daemon of " + d.User + " is not running"
	}
	return ""
}

// Steps are the commands that make docker info answer, in order.
func (d DockerDown) Steps() []string {
	switch {
	case d.Denied:
		return []string{
			"sudo usermod -aG docker " + d.User + "   # the docker group is equivalent to root",
			"log out and back in (or run: newgrp docker)",
		}
	case d.Rootless:
		return []string{"systemctl --user enable --now docker   # as " + d.User}
	}
	return []string{"sudo systemctl enable --now docker"}
}

// Sentence is Steps as one clause of a Fix line.
func (d DockerDown) Sentence() string {
	switch {
	case d.Denied:
		return "let " + d.User + " use Docker: sudo usermod -aG docker " + d.User + " (the docker group is equivalent to root), then log out and back in"
	case d.Rootless:
		return "start the rootless Docker of " + d.User + ": systemctl --user enable --now docker, as " + d.User
	}
	return "start Docker: sudo systemctl enable --now docker"
}

// NeedsLogin: the fix takes effect only in a new login session, not in the
// process that is running now.
func (d DockerDown) NeedsLogin() bool { return d.Denied }
