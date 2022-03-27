FROM golang:1.18-alpine AS build

WORKDIR /app
COPY * /app/
RUN go get .
RUN CGO_ENABLED=0 GOOS=linux go build .

FROM alpine:3.14

WORKDIR /app

COPY --from=build /app/openfaas-scaler /app/openfaas-scaler
RUN chmod 700 /app/openfaas-scaler

RUN apk --no-cache add ca-certificates

ENTRYPOINT [ "/app/openfaas-scaler" ]
