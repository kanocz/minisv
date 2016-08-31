FROM ubuntu:16.04
MAINTAINER anton@nsl.cz

RUN apt-get update && apt-get install -y nginx-light redis-server
RUN mkdir -p /var/log/nginx /var/log/minisv /opt

COPY minisv /opt
COPY minisv.json /opt

EXPOSE 80 443 3443 6379
ENTRYPOINT ["/opt/minisv"]
