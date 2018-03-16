# minisv (mini supervisor)

Service starter, easy start multiply services (for example in one docker
container or using systemd on regular system) with possibility to (gracefull)
restart them via http. Gracefull restart is used for services supporting
SO_REUSERPORT: new instance will started, waited X seconds to be sure that
everything is ok and only then SIGTERM will be send to old one; if old one
will not exit in Y time than SIGKILL also sent.

It's also possible to run "one-time" actions like a repository pull one
webhook and so on (need to have `oneTime` setted to `true`).

Possibility to "rotate" logs using HUP signal, using `logreload` variable
in config or `rotate` command via http. Using `logdate` it's possible to
add current date/time as log file name sufix (using class golang format).
This affects only permanent task, not `oneTime`.

Also is possible to add/remove tasks online via HTTP-requests.

For restarting, sending signals and task add/remove next http schema used:

- *GET* `http://[addr]:[port]/` (return basic status on all tasks)
- *GET* `http://[addr]:[port]/[taskname]/[stop|restart|term|hup|kill|run|rotate]`
- *POST* `http://[addr]:[port]/[taskname]` (with json description of task as http body)  
- *DELETE* `http://[addr]:[port]/[taskname]`

for example:

```bash
curl -d '{"command": "/bin/sleep","args": ["1800"],"workdir": "/home"}' -H "Content-Type: application/json" -X 'POST' 'http://127.0.0.1:3443/sleep1800'
curl -i 'http://127.0.0.1:3443/sleep1800/kill'
curl -i 'http://127.0.0.1:3443/sleep1800/restart'
curl -X 'DELETE' 'http://127.0.0.1:3443/sleep3'
curl -i 'http://127.0.0.1:3443/pull/run'
```

Example *Dockerfile* for use with minisv:

```Dockerfile
FROM ubuntu:16.04
MAINTAINER somebody@service

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

using different *logfileprefix* prevents mixing of logs from different containers

commands description:

* *stop* - stop task until *restart* or *run* command
* *restart* - start after *stop* OR _graceful restart_ if running (start new instance, wait if not crashed and then terminate old one), not for _ontime_ tasks
* *term* - send SIGTERM to process
* *hup* - send SIGHUP to process
* *kill* - send SIGKILL to process
* *run* - run _onetime_ task
* *rotate* - close log and reopen (with different name while _logsuffixdate_ is used), not for _onetime_ tasks
* *status* - return current process status
