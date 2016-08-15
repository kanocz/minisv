# dockStarter

Service starter, easy start multiply services in one docker
container with possibility to (gracefull) restart them via
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