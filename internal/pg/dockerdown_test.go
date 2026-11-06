package pg

import (
	"errors"
	"strings"
	"testing"
)

// BR-030: why docker info failed, told apart once for init's step 0 and
// the drill: the socket refusing the user, a rootless daemon that is not
// running (by its endpoint or by docker's message), or the system daemon.
func TestWhyDockerDown(t *testing.T) {
	for _, tc := range []struct {
		msg, endpoint string
		steps         []string
		sentence      string
	}{
		{"permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock", "",
			[]string{"sudo usermod -aG docker deploy   # the docker group is equivalent to root", "log out and back in (or run: newgrp docker)"},
			"let deploy use Docker: sudo usermod -aG docker deploy (the docker group is equivalent to root), then log out and back in"},
		{"Cannot connect to the Docker daemon at unix:///run/user/1000/docker.sock. Is the docker daemon running?", "",
			[]string{"systemctl --user enable --now docker   # as deploy"},
			"start the rootless Docker of deploy: systemctl --user enable --now docker, as deploy"},
		{"Cannot connect to the Docker daemon", "unix:///run/user/1000/docker.sock",
			[]string{"systemctl --user enable --now docker   # as deploy"},
			"start the rootless Docker of deploy: systemctl --user enable --now docker, as deploy"},
		{"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", "unix:///var/run/docker.sock",
			[]string{"sudo systemctl enable --now docker"},
			"start Docker: sudo systemctl enable --now docker"},
	} {
		d := WhyDockerDown(errors.New(tc.msg), "deploy", tc.endpoint)
		if strings.Join(d.Steps(), "\n") != strings.Join(tc.steps, "\n") || d.Sentence() != tc.sentence {
			t.Errorf("%q %q: %q / %q", tc.msg, tc.endpoint, d.Steps(), d.Sentence())
		}
		if d.NeedsLogin() != d.Denied || (d.Denied || d.Rootless) == (d.Detail() == "") {
			t.Errorf("%q: %+v detail %q", tc.msg, d, d.Detail())
		}
	}
}
