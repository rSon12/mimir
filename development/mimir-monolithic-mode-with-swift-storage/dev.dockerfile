FROM alpine:3.23.2

RUN     mkdir /mimir
WORKDIR /mimir
ADD     ./mimir ./
