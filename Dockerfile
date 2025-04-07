FROM golang:1.23.0-alpine AS build
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

WORKDIR /src
COPY . .
RUN go mod download
RUN go build -o /token-ext-proc

FROM registry.access.redhat.com/ubi8/ubi-minimal

WORKDIR /
COPY --from=build /token-ext-proc /token-ext-proc

ENTRYPOINT ["/token-ext-proc"]
