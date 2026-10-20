//go:build linux

package app

import "testing"

func TestKnownSharedSSHMasterDaemonCommandRecognition(t *testing.T) {
	if !knownSharedSSHMasterDaemonCommand("ssh", []string{"ssh: /home/user/.ssh/cm-123 [mux]"}) {
		t.Fatal("OpenSSH's rewritten argv title was not recognized with comm=ssh")
	}
	if knownSharedSSHMasterDaemonCommand("ssh", []string{"ssh", "host", "true"}) {
		t.Fatal("ordinary SSH client was recognized as a shared master")
	}
}

func TestSharedTaskDaemonCommandRecognition(t *testing.T) {
	cases := []struct {
		name string
		comm string
		args []string
		want bool
	}{
		{name: "ssh ControlMaster", comm: "ssh", args: []string{"ssh", "-M", "-S", "/home/user/.ssh/control", "host"}, want: true},
		{name: "detached OpenSSH ControlPersist mux title", comm: "ssh", args: []string{"ssh: /home/user/.ssh/cm-123 [mux]"}, want: true},
		{name: "ssh ControlPersist", comm: "ssh", args: []string{"ssh", "-o", "ControlMaster=auto", "-o", "ControlPersist=10m", "host"}, want: true},
		{name: "Gradle daemon", comm: "java", args: []string{"java", "org.gradle.launcher.daemon.bootstrap.GradleDaemon", "9.0"}, want: true},
		{name: "Kotlin daemon", comm: "java", args: []string{"java", "org.jetbrains.kotlin.daemon.KotlinCompileDaemon", "--daemon"}, want: true},
		{name: "gopls remote daemon", comm: "gopls", args: []string{"gopls", "-remote=auto"}, want: true},
		{name: "sccache server startup", comm: "sccache", args: []string{"sccache", "--start-server"}, want: true},
		{name: "sccache daemon process", comm: "sccache", args: []string{"sccache"}, want: true},
		{name: "tmux server process title", comm: "tmux: server", args: []string{"tmux"}, want: true},
		{name: "tmux server argument", comm: "tmux", args: []string{"tmux", "server"}, want: true},
		{name: "tmux foreground server", comm: "tmux", args: []string{"tmux", "-D"}, want: true},
		{name: "screen server process title", comm: "SCREEN", args: []string{"SCREEN", "-D", "-m"}, want: true},
		{name: "ordinary ssh client", comm: "ssh", args: []string{"ssh", "host", "true"}},
		{name: "detached tmux client", comm: "tmux", args: []string{"tmux", "-d", "new-session"}},
		{name: "Worker Node server", comm: "node", args: []string{"node", "server.js"}},
		{name: "Worker Vite server", comm: "node", args: []string{"node", "vite"}},
		{name: "Worker browser", comm: "chrome", args: []string{"chrome", "--user-data-dir=/scratch/task"}},
		{name: "Worker test helper", comm: "sleep", args: []string{"sleep", "60"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSharedTaskDaemonCommand(tc.comm, tc.args); got != tc.want {
				t.Fatalf("isSharedTaskDaemonCommand(%q, %#v) = %t, want %t", tc.comm, tc.args, got, tc.want)
			}
		})
	}
}
