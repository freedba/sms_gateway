FROM alpine:3.10

WORKDIR /sms_gateway

COPY config/zoneinfo /usr/share/zoneinfo
COPY config/zoneinfo/Asia/Shanghai /etc/localtime
RUN echo 'Asia/Shanghai' >/etc/timezone

ADD bin/sms_gateway sms_gateway
RUN mkdir config
ADD config/channels.toml config

EXPOSE 7890

VOLUME /data

RUN mkdir /data/log

ENTRYPOINT ["./sms_gateway"]
