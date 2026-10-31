FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/reelay .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/reelay /usr/local/bin/reelay
ENV DATA_DIR=/data WORK_DIR=/tmp/reelay-work
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["reelay"]
CMD ["serve"]
