# Testing Posse

## Run Lead-side commands in an isolated lab

Use an isolated Herdr server and `POSSE_HOME` when you need to exercise commands that require a Lead pane. The lab keeps pane operations and Posse state away from the User's live session. Use a disposable Git clone so Task and Project operations cannot change the checkout you are working in.

Choose a short temporary directory because Herdr's Unix socket path must fit the platform limit. Build Posse, clone the repository, and start Herdr with a clean environment:

```sh
LAB=$(mktemp -d)
mkdir -p "$LAB/home" "$LAB/xdg" "$LAB/posse" "$LAB/bin"
go build -o "$LAB/bin/posse" ./cmd/posse
git clone --no-hardlinks . "$LAB/project"

env -i HOME="$LAB/home" XDG_CONFIG_HOME="$LAB/xdg" POSSE_HOME="$LAB/posse" \
  PATH="$LAB/bin:$PATH" herdr server
```

In another terminal, set the same environment and create a workspace in the clone:

```sh
LAB=/path/from/the/first/terminal
env -i HOME="$LAB/home" XDG_CONFIG_HOME="$LAB/xdg" POSSE_HOME="$LAB/posse" \
  PATH="$LAB/bin:$PATH" herdr workspace create --cwd "$LAB/project"
```

Run commands from the shell pane Herdr opens. Start a Lead with `posse up --yes` and choose an available harness with `--claude`, `--codex`, or `--pi` when needed. You can then exercise Lead-side commands such as `posse lead` and `posse send` from the isolated Project.

Keep every Herdr command in the lab pointed at the isolated `XDG_CONFIG_HOME`. Do not inherit any `HERDR_*` variables. Configure the harness credentials under the isolated `HOME` before starting a Lead. Agent requests can still reach the provider. Run `herdr server stop` with the lab environment after your checks, then remove the lab directory.

The shell guard permits references to `HERDR_*` variable names when they are ordinary text. Code that reads those variables to connect to the Worker's Herdr remains blocked unless it proves that it targets a different socket.
