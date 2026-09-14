FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/upornot .

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/upornot /upornot
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/upornot"]
