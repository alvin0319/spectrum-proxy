FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git bash

WORKDIR /build

COPY . .

RUN chmod +x ./install-deps.sh

RUN bash ./install-deps.sh

RUN go mod tidy && go build -o spectrum-proxy .

FROM alpine:3.22

WORKDIR /spectrum-proxy

COPY --from=builder /build/spectrum-proxy /spectrum-proxy

EXPOSE 19132

CMD ["/spectrum-proxy/spectrum-proxy"]
