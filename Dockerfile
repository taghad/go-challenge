FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# A static binary needs no libc, and the image has no shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
