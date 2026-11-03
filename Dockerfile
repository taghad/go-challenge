FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/server ./cmd/uss

FROM gcr.io/distroless/static-debian12:nonroot AS uss
COPY --from=build /out/uss /uss
ENTRYPOINT ["/uss"]

# A static binary needs no libc, and the image has no shell or package manager. This
# stage is last, so that a build without a target gives the server.
FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
