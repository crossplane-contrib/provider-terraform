FROM BASEIMAGE

ARG ARCH
ARG TINI_VERSION

ADD provider /usr/local/bin/crossplane-terraform-provider

# As of Crossplane v1.3.0 provider controllers run as UID 2000.
# https://github.com/crossplane/crossplane/blob/v1.3.0/internal/controller/pkg/revision/deployment.go#L32
RUN addgroup -g 2000 controller && \
    adduser -D -u 2000 -G controller  controller

ADD .gitconfig /home/controller/.gitconfig
ADD ssh_config /home/controller/.ssh/config

RUN mkdir /tf

RUN chown controller:controller /tf && \
    chown -R controller:controller /home/controller

EXPOSE 8080
USER 2000
ENTRYPOINT ["crossplane-terraform-provider"]
