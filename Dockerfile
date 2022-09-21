FROM golang:1.19.1-alpine3.16 AS builder
RUN mkdir /build
ADD . /build
WORKDIR /build
RUN go build -ldflags '-s' -o /bench main.go

FROM alpine:3.16.2
COPY --from=builder /bench .
ADD data/query_params.csv /data/query_params.csv
RUN apk update && apk add ca-certificates
ENTRYPOINT ["/bench"]