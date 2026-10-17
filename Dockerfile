FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o app .

FROM alpine:3.24

RUN apk add --no-cache ca-certificates libc6-compat curl bash mongodb-tools age

WORKDIR /root/

COPY --from=builder /src/app app

CMD ["/root/app"]
