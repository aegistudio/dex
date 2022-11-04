# Build DeX executable using golang container
FROM golang:1.18.8-bullseye as builder

RUN mkdir -p /build

COPY . /build

RUN cd /build && \
    CGO_ENABLED=0 go build -ldflags="-extldflags=-static"  -o dex ./cmd/dex/

# Release the DeX container with TeXLive and TiKZ depenencies.
FROM debian:bullseye-slim as runner

RUN mkdir -p /app

COPY ./preset/ /app

COPY --from=builder /build/dex /app/dex

RUN apt update -y && \
    apt install --no-install-recommends -y dvisvgm pdf2svg scour texlive-latex-base texlive-latex-extra texlive-pictures xz-utils curl ca-certificates && \
    apt autoremove -y && \
    apt clean && \
    rm -rf /var/lib/apt/lists/

RUN tlmgr init-usertree && \
    tlmgr option repository https://ftp.math.utah.edu/pub/tex/historic/systems/texlive/2020/tlnet-final/ && \
    tlmgr option docfiles 0 && \
    tlmgr install stix2-type1 filemod ucs currfile varwidth adjustbox standalone newtx kastrup && \
    updmap-sys

ENTRYPOINT ["/app/dex"]
