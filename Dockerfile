FROM ubuntu:22.04

RUN apt-get update && apt-get install -y nginx-light redis-server
RUN mkdir -p /var/log/nginx /var/log/minisv /opt

COPY minisv /opt
# default config path; or pass -config /opt/minisv.json in ENTRYPOINT
COPY minisv.json /etc/minisv.json

EXPOSE 80 443 3443 6379
ENTRYPOINT ["/opt/minisv"]
