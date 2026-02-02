FROM alpine:3.23.3

RUN     mkdir /mimir
WORKDIR /mimir
ADD     ./mimir ./
