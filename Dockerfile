FROM docker.io/library/golang:1.19 as build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/vmware-populator

# TODO: switch to ubi9-minimal or distroless
FROM quay.io/centos/centos:stream9
LABEL maintainers="KubeV2V Authors"
LABEL description="VMware populator for Volume Populator service"

RUN dnf -y install nbdkit>=1.22 nbdkit-curl-plugin qemu-img libguestfs-tools-c
COPY --from=build /go/bin/vmware-populator /usr/local/bin/vmware-populator
ENTRYPOINT ["/usr/local/bin/vmware-populator"]
