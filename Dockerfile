FROM alpine:3.10

WORKDIR /sms_gateway

ADD config/localtime /etc/
RUN echo 'Asia/Shanghai' >/etc/timezone

ADD bin/sms_gateway sms_gateway
RUN mkdir config
ADD config/channels.toml config

EXPOSE 7890

VOLUME /data

RUN mkdir /data/logs

ENTRYPOINT ["./sms_gateway"]
