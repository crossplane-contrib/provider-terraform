FROM BASEIMAGE

ARG ARCH
ARG TINI_VERSION
ENV USER_ID=1001

RUN \
  apk update && \
  apk add bash py3-pip && \
  apk add --virtual=build gcc libffi-dev musl-dev openssl-dev python3-dev make && \
  pip --no-cache-dir install -U pip && \
  pip --no-cache-dir install azure-cli && \
  apk del --purge build

ADD provider /usr/local/bin/crossplane-terraform-provider
ADD .gitconfig .gitconfig

RUN mkdir /.azure && \
    chgrp -R 0 /.azure && \
    chmod -R g=u /.azure

RUN mkdir /tf && \
    chgrp -R 0 /tf && \
    chmod -R g=u /tf

USER ${USER_ID}
EXPOSE 8080
ENTRYPOINT ["crossplane-terraform-provider"]
