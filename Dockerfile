## Build the app
FROM golang:alpine AS builder
RUN apk add --no-cache git
## Build fork of Reg with multi-manifest support
RUN cd /tmp && \
    git clone https://github.com/cquon/reg.git && \
    cd reg && go build -o /reg
WORKDIR /go/src/app
COPY . .
RUN go get -d -v ./...
RUN go build -o /go/bin/app -v ./...

## Get Helm
FROM alpine/helm:latest AS helm

## Get Docker CLI
FROM docker:git AS docker

## Install AWS CLI and add other components
## Copies the app, Reg, Helm and Docker to same image
FROM alpine:latest
RUN apk add --no-cache \
        python3 \
        py3-pip \
    && pip3 install --upgrade pip \
    && pip3 install \
        awscli \
    && rm -rf /var/cache/apk/*

## Installs GH CLI
RUN mkdir /usr/bin/gh && \
    wget https://github.com/cli/cli/releases/download/v2.7.0/gh_2.7.0_linux_amd64.tar.gz -O ghcli.tar.gz && \
    tar --strip-components=1 -xf ghcli.tar.gz -C /usr/bin/gh

## Copy executables from the other images
COPY --from=builder /go/bin/app /app
COPY --from=builder /reg /usr/bin/reg
COPY --from=helm /usr/bin/helm /usr/bin/helm
## Don't need Docker Daemon, just the CLI to login to registries
COPY --from=docker /usr/local/bin/docker /usr/bin/docker
ENV PATH="${PATH}:/usr/bin/gh/bin"
ENTRYPOINT /app
LABEL Name=terraformhelmdigests Version=0.0.1
