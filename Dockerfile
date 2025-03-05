FROM golang:1.24 as build
COPY . .
RUN go get -d -v ./...
RUN go install -v ./...


FROM debian
COPY --from=build /go/bin/nvme_exporter /usr/local/bin/nvme_exporter
RUN apt-get update
RUN apt-get -y install nvme-cli

WORKDIR /go/src/nvme_exporter
EXPOSE 9998

CMD [ "nvme_exporter" ]
