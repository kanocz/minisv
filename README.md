# minisv (mini supervisor)

Service starter, easy way to start multiple services (for example in one
docker container or using systemd on a regular system) with the possibility
to (gracefully) restart them via HTTP. Graceful restart is intended for
services supporting SO_REUSEPORT: a new instance is started first, minisv
waits X seconds to be sure that everything is ok, and only then SIGTERM is
sent to the old one; if the old one does not exit in Y seconds, SIGKILL is
sent as well.

It's also possible to run "one-time" actions like a repository pull on
webhook and so on (set `oneTime` to `true` for the task).

Data can be passed to the standard input of "one-time" tasks (using the
*POST* method), for example, for creating configs before adding new tasks.

Logs can be "rotated" by sending the HUP signal to minisv, automatically
using the `logreopen` config option, or with the `rotate` command via HTTP.
Using `logdate` it's possible to prefix every log line with date/time, and
`logsuffixdate` adds the current date/time to the log file name (both use
classic golang time format). This affects only permanent tasks, not
`oneTime` ones.

Tasks can be added/removed online via HTTP requests; such changes are
saved back into the config file.

If a task's command fails to start (e.g. the binary does not exist yet),
minisv retries with the `restartPause` interval (at least 1 second)
instead of giving up.

## Web UI

Opening `http://[addr]:[port]/` in a browser shows a small built-in web
interface: live status of all tasks with buttons for start/stop/restart,
log rotation, viewing last log lines, and creating/deleting tasks.

## HTTP API

The API lives under the `/api` prefix:

- *GET* `/api` — status of all tasks
- *GET* `/api/config` — selected config values (e.g. `logBufferLines`)
- *POST* `/api/[taskname]` — add a new task (json description of the task as request body)
- *DELETE* `/api/[taskname]` — stop and remove a task
- *GET* `/api/[taskname]/status` — status of one task
- *GET* `/api/[taskname]/[stop|restart|term|hup|kill|run|rotate]` — control commands
- *POST* `/api/[taskname]/run` — run a one-time task with request body passed to its standard input
- *GET* `/api/[taskname]/logs` — last log lines of the task from the memory
  buffer (the number of lines is set by the `logbufferlines` config option,
  default 10)

The same paths without the `/api` prefix (e.g. `/[taskname]/restart`,
old-style scheme) are kept working for backward compatibility, as well as
*GET* `/status` for the status of all tasks.

Errors are reported with meaningful HTTP status codes: 400 (bad request
body/task description), 404 (unknown task), 406 (command not applicable to
this task type), 409 (task already exists / one-time task already running),
503 (task is busy, try again later).

Commands description:

* *stop* — stop the task until *restart* command
* *restart* — start after *stop* OR _graceful restart_ if running (start a
  new instance, wait to be sure it hasn't crashed and only then terminate
  the old one); not for _onetime_ tasks
* *term* — send SIGTERM to the process
* *hup* — send SIGHUP to the process
* *kill* — send SIGKILL to the process
* *run* — run a _onetime_ task (only for _onetime_ tasks; for stopped
  permanent tasks use *restart*)
* *rotate* — close the log file and reopen it (with a different name when
  _logsuffixdate_ is used); not for _onetime_ tasks
* *status* — return current process status

Examples:

```bash
curl -d '{"command": "/bin/sleep","args": ["1800"],"workdir": "/home"}' -H "Content-Type: application/json" -X 'POST' 'http://127.0.0.1:3443/api/sleep1800'
curl -i 'http://127.0.0.1:3443/api/sleep1800/kill'
curl -i 'http://127.0.0.1:3443/api/sleep1800/restart'
curl -X 'DELETE' 'http://127.0.0.1:3443/api/sleep1800'
curl -i 'http://127.0.0.1:3443/api/pull/run'
curl -i -X 'POST' -d '@/tmp/image.jpeg' 'http://127.0.0.1:3443/api/imgsave/run'
```

## Signals

minisv itself reacts to the following signals:

* *HUP* — rotate logs of all permanent tasks
* *USR1* — restart the HTTP server (mainly to reload changed TLS certificates)
* *TERM* / *INT* — terminate all tasks (SIGTERM, then SIGKILL after the
  task's `wait` timeout) and exit

## Configuration

Example *Dockerfile* for use with minisv:

```Dockerfile
FROM ubuntu:22.04

RUN apt-get update && apt-get install -y nginx-light redis-server
RUN mkdir -p /var/log/nginx /var/log/minisv /opt

COPY minisv /opt
COPY minisv.json /opt

EXPOSE 80 443 3443 6379
ENTRYPOINT ["/opt/minisv"]
```

while minisv.json contains

```json
{
    "logdir": "/var/log/minisv",
    "logfileprefix": "container1-",
    "logreopen": "1h",
    "logsuffixdate": "20060102.150405",
    "logdate": "2006/01/02 15:04:05",
    "logbufferlines": 10,
    "tasks": {
        "redis": {
            "command": "/usr/bin/redis-server",
            "args": ["--port", "6379"],
            "workdir": "/tmp",
            "wait": 60,
            "restartPause":1,
            "startTime": 10
        },
        "nginx": {
            "command": "/usr/sbin/nginx",
            "args": ["-g", "daemon off;"],
            "wait": 60,
            "restartPause":0,
            "startTime": 3
        },
        "pull": {
            "command": "/usr/bin/git",
            "args": ["pull", "-f"],
            "workdir": "/home/www/example.com",
            "oneTime": true
        }
    },
    "http": {
        "address": "127.0.0.1",
        "port": 3443
    }
}
```

starting with

```bash
docker run -v '/var/log/minisv:/var/log/minisv' 'container1'
```

using different *logfileprefix* prevents mixing of logs from different
containers.

Task options:

* *command*, *args*, *workdir* — what to run and where
* *wait* — seconds to wait after SIGTERM before sending SIGKILL
* *restartPause* — pause in seconds before restarting an exited task (and
  between attempts when the command fails to start)
* *startTime* — seconds a new instance must stay alive during graceful
  restart to be considered good
* *oneTime* — the task is started only on request, not supervised

## Graylog

In addition to log files, task output can be sent to a Graylog server
(GELF via UDP, chunking and compression supported):

```json
"graylog": {
    "remote": "graylog.example.com:12201",
    "level": 1,
    "addfields": {
        "_some_info": "foo",
        "_some_env_var": "bar"
    }
}
```

`addfields` values are added to every message as-is (custom field names
must start with an underscore).

## HTTPS & Auth

If no user, password and certificate parameters are specified in config,
_minisv_ will work with plain HTTP and no authentication — suitable only
for listening on localhost. Since the API allows starting arbitrary
commands, minisv logs a warning when it listens on a non-loopback address
without authentication.

If user and password (bcrypt hash) are specified in the http section,
http-basic auth is on:

```json
"http": {
        "address": "0.0.0.0",
        "port": 3443,
        "user": "admin",
        "password": "$2a$14$ajq8Q7fbtA0QvXpdCq7Jcuy.Rx1h/L4J60Otx.gyNLbAYctGMJ9tK"
    }
```

For more security it's possible to turn on HTTPS:

```json
"http": {
        "address": "0.0.0.0",
        "port": 3443,
        "user": "admin",
        "password": "$2a$14$ajq8Q7fbtA0QvXpdCq7Jcuy.Rx1h/L4J60Otx.gyNLbAYctGMJ9tK",
        "servercert": "server.crt",
        "serverkey": "server.key"
    }
```

And as most-secure it's possible to turn on verification of the client
certificate:

```json
"http": {
        "address": "0.0.0.0",
        "port": 3443,
        "user": "admin",
        "password": "$2a$14$ajq8Q7fbtA0QvXpdCq7Jcuy.Rx1h/L4J60Otx.gyNLbAYctGMJ9tK",
        "servercert": "server.crt",
        "serverkey": "server.key",
        "clientcert": "client.crt"
    }
```

and yes, it's true — it's possible to combine https-client-auth with
http-basic-auth at the same time and both will be required at once.

## Resource limits

It's possible to set global limits (like calling ulimit just before
minisv) by setting individual options in the `limits` array like this:

```json
{
    "logdir": "/var/log/minisv",
    "limits": [
        {
            "type": "nofile",
            "cur":  2048,
            "max":  4096
        },
        {
            "type": "nproc",
            "cur":  16384,
            "max":  16384
        }
    ],
    "tasks": {
        ......
    }
}
```

Supported limit types: `as`, `core`, `cpu`, `data`, `fsize`, `nofile`,
`nproc`, `stack`.

## Building

Go 1.26+ is required (the only external dependency is `golang.org/x/crypto`
for bcrypt):

```bash
go build
```
