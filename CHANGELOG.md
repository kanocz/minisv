# Changelog

Versions follow the `AppVersion` constant in main.go and git tags. Dates are
commit dates.

## 1.3.0

Everything since v1.2.2, including the commits of 2024 and 2025 that were
never tagged.

### Breaking changes

- The HTTP API lives under `/api/`; the old paths without the prefix are
  rewritten to it and keep working.
- `started` and `finished` in the task status are omitted when zero
  instead of being sent as `0001-01-01T00:00:00Z`. Use the new `running`
  field to tell a running task from a stopped one.
- Task names created via the API may contain only letters, digits, `.`,
  `_` and `-`; `config` is reserved. Tasks with other names in an existing
  config file keep working.
- stop, term, hup and kill on an idle one-time task answer 409 instead of
  200. run on a permanent task and restart on a one-time task answer 406.
- Every task runs in its own process group. stop, term, kill and the
  shutdown of minisv signal the whole group, so helpers started by the
  task die with it; hup goes to the main process only. Ctrl+C in a
  terminal no longer reaches the tasks directly, they get SIGTERM from
  minisv.
- A task that dies within a second of starting is restarted no more than
  once a second, whatever `restartPause` says.
- Mutating requests marked by the browser as coming from another site
  (`Sec-Fetch-Site: cross-site` or `same-site`, or a foreign `Origin`
  without `Sec-Fetch-Site`) are rejected with 403. curl and scripts are
  not affected.
- Failed logins are limited per client address: 10 per minute by default,
  then 429. Configurable with `http.authfaillimit`, `-1` turns it off.
- The Dockerfile copies the config to `/etc/minisv.json`, the default of
  `-config`; the previous `/opt/minisv.json` was never read without the
  flag.
- `/favicon.svg` moved to `/static/favicon.svg`.

### Added

- Web UI on `/` built with htmx and Bootstrap: task cards with status and
  the last log lines, start/stop/restart/kill buttons, a form to create
  tasks (command, args, workdir, wait, restartPause, startTime, one-time).
  Assets are embedded into the binary, no CDN requests.
- `running` boolean in the task status.
- `logbufferlines` config option: number of recent log lines kept in
  memory for the UI.
- USR1 reloads the https server certificate and key.
- `http.authfaillimit` config option.
- Zombie reaping when minisv runs as PID 1 in a container, so `--init` or
  tini is not needed.
- One-time tasks are terminated when minisv exits, SIGTERM and then
  SIGKILL after `wait` seconds, and minisv waits for them like for the
  permanent tasks. The HTTP server stops first, and run or task creation
  requests still in flight answer 503.
- Request body limits: 1 MB for a task description, 64 MB for stdin of a
  one-time task, 413 above that.
- Go tests for the control loop, CSRF guard, login limiter and process
  helpers.

### Fixed

- Exit of minisv or deletion of a task waited the full `wait` seconds
  after a graceful restart, successful or failed, because the old
  instance was still marked as running.
- `restartPause: 0` with a command that fails at start spun in a busy
  loop writing to the log.
- A stop, restart or kill request arriving while the task was between
  restarts was silently dropped; the control channels are buffered now. A
  term or kill sent during a graceful restart or a stop is aimed at the
  instance being terminated and is dropped once it is gone; a hup queued
  during a graceful restart goes to the new instance, one queued during a
  stop is dropped.
- stop of a permanent task answers `ok` at once even while the loop is busy
  with a restart; before it waited 3 seconds and answered 503. The stop is
  executed when the loop gets to it, so the task may still be running when
  the answer arrives.
- `running` is true with the status `new instance failed` even when the
  old instance has already died and the task is in its restart pause; the
  status is corrected by the next automatic restart.
- The UI showed a task as stopped during restart validation and after a
  failed restart while the old instance was still running.
- A child process holding the task's stdout open blocked the restart
  forever; the pipe is closed 10 seconds after the main process exits.
- Config is saved through a temporary file and rename with mode 0600, so
  a crash during save does not destroy it and the password hash is not
  world readable.
- A client certificate file without a single certificate was accepted and
  every client was rejected with a TLS error; minisv now refuses to start
  and says why.
- Repeated requests with the same credentials skip bcrypt, the UI polling
  no longer costs a bcrypt round per request.
- Correct HTTP status codes on errors: 400 for bad JSON, 404 for an
  unknown task, 409 for an existing task, 503 while a task is busy.
- Removed the `chi` dependency; routing uses `net/http` patterns.

## 1.2.2 (2021-07-03)

### Added

- Log output to Graylog over UDP in GELF format (`graylog` config section).

## 1.2.1 (2021-05-27)

### Added

- Status of all tasks in one request.
- https and http basic auth with a bcrypt password hash; optional client
  certificate authentication.
- Stdin for one-time tasks: the request body is passed to the process.
- Resource limits (`limits` config section, setrlimit before starting
  tasks).

### Changed

- Go modules; staticcheck fixes.

### Fixed

- Log file name for tasks created over HTTP.
- No `: ` prefix on log lines when no time format is configured.

## 1.2.0 (2017-09-27)

### Added

- Create and delete tasks over HTTP (`POST /{task}`, `DELETE /{task}`).

## 1.1.1 (2017-06-25)

### Added

- Signals and termination for one-time tasks.
- The same one-time task cannot run twice in parallel.

## 1.1.0 (2017-06-25)

### Added

- Task status over HTTP.

## 1.0.0a (2017-04-23)

First tagged release.

- Supervised tasks with graceful restart: a new instance is started and
  validated before the old one is terminated.
- One-time tasks started over HTTP.
- Log files per task with rotation on HUP and on a timer.
- `/{task}/stop` handler.
- systemd unit file.
