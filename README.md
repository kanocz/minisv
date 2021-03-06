# minisv (mini supervisor)

Service starter, easy start multiply services (for example in one docker
container or using systemd on regular system) with possibility to (gracefull)
restart them via http. Gracefull restart is used for services supporting
SO_REUSERPORT: new instance will started, waited X seconds to be sure that
everything is ok and only then SIGTERM will be send to old one; if old one
will not exit in Y time than SIGKILL also sent.

It's also possible to run "one-time" actions like a repository pull one
webhook and so on (need to have `oneTime` setted to `true`).

In last version it's possible to pass data on standart input of "one-time"
tasks (using *POST* method), for example, for creating configs before
adding new tasks.

Possibility to "rotate" logs using HUP signal, using `logreload` variable
in config or `rotate` command via http. Using `logdate` it's possible to
add current date/time as log file name sufix (using class golang format).
This affects only permanent task, not `oneTime`.

Also is possible to add/remove tasks online via HTTP-requests.

For restarting, sending signals and task add/remove next http schema used:

- *GET* `http://[addr]:[port]/` (return basic status on all tasks)
- *GET* `http://[addr]:[port]/[taskname]/[stop|restart|term|hup|kill|run|rotate]`
- *POST* `http://[addr]:[port]/[taskname]/run` (run one-time task with data on standart input)
- *POST* `http://[addr]:[port]/[taskname]` (with json description of task as http body)  
- *DELETE* `http://[addr]:[port]/[taskname]`

for example:

```bash
curl -d '{"command": "/bin/sleep","args": ["1800"],"workdir": "/home"}' -H "Content-Type: application/json" -X 'POST' 'http://127.0.0.1:3443/sleep1800'
curl -i 'http://127.0.0.1:3443/sleep1800/kill'
curl -i 'http://127.0.0.1:3443/sleep1800/restart'
curl -X 'DELETE' 'http://127.0.0.1:3443/sleep3'
curl -i 'http://127.0.0.1:3443/pull/run'
curl -i -X 'POST' -d '@/tmp/image.jpeg' 'http://127.0.0.1:3443/imgsave/run'
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


## HTTPS & Auth

if no user, password and certificate parameters are specified in config _minisv_ will work with plain HTTP and no authentification (suitable for listening on localhost and no access of third people to server).

If user and password (bcrypted hash) specified in http session http-basic auth is on.
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

And as most-secure it's possible to turn on verifing of client certificate:

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

and yes, it's true - it's possible combinate https-client-auth with http-basic-auth in the same time and both will be required at once.

## Resource limits

it's possible to set global limits (like calling ulimit just before minisv) by setting individual options on `limits` array like this:
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
