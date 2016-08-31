# minisv (mini supervisor)

Service starter, easy start multiply services (for example in one docker
container) with possibility to (gracefull) restart them via
http. Gracefull restart is used for services supporting SO_REUSERPORT:
new instance will started, waited X seconds to be sure that anything
is ok and only then SIGTERM will be send to old one; if old one
will not exit in Y time than SIGKILL also sent.

Possibility to run predefined tasks on webhooks is also planed
(primary for pulling new versions on github webhooks)

for restarting or sending signals next http schema used:

`http://[addr]:[port]/[taskname]/[restart|term|hup|kill]`

for exampe:
```
curl -i http://127.0.0.1:3443/sleep1800/kill
curl -i http://127.0.0.1:3443/sleep1800/restart
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
docker run -v /var/log/minisv:/var/log/minisv container1
```

using different *logfileprefix* prevents mixing of logfileprefix
