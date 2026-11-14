FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/smbfileserver .

FROM scratch
COPY --from=build /out/smbfileserver /smbfileserver
USER 65532:0
EXPOSE 3000
ENTRYPOINT ["/smbfileserver"]
