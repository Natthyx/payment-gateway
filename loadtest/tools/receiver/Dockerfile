FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -o /receiver .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /receiver /receiver
EXPOSE 8443
ENTRYPOINT ["/receiver"]
