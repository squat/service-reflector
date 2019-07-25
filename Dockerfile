ARG FROM=alpine
FROM $FROM
LABEL maintainer="squat <lserven@gmail.com>"
ARG GOARCH
COPY bin/$GOARCH/service-reflector /opt/bin/
ENTRYPOINT ["/opt/bin/service-reflector"]
