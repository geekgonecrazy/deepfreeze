FROM golang:1.14 AS builder

WORKDIR /go/src/github.com/geekgonecrazy/deepfreeze

COPY . .


RUN GOOS=linux go build -o app .

FROM alpine:latest 

ENV MONGODB_TOOLS_VERSION 4.0.5-r0

RUN echo 'http://dl-cdn.alpinelinux.org/alpine/v3.9/main' >> /etc/apk/repositories && \
    echo 'http://dl-cdn.alpinelinux.org/alpine/v3.9/community' >> /etc/apk/repositories && \ 
    apk update && \
    apk --no-cache add ca-certificates libc6-compat curl bash mongodb-tools=${MONGODB_TOOLS_VERSION} && \
    wget https://github.com/FiloSottile/age/releases/download/v1.0.0-beta2/age-v1.0.0-beta2-linux-amd64.tar.gz && \
    tar xvf age-v1.0.0-beta2-linux-amd64.tar.gz && \
    mv age/age /usr/bin && \
    rm -rf age

ENV GIN_MODE=release

WORKDIR /root/

COPY --from=builder /go/src/github.com/geekgonecrazy/deepfreeze/app app

CMD ["/root/app"]