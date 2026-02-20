FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go build -o /bin/proxy .

FROM alpine:3.20
RUN adduser -D -H proxy
USER proxy
ENV PROXY_ADDR=:8090
EXPOSE 8090
COPY --from=build /bin/proxy /proxy
ENTRYPOINT ["/proxy"]

