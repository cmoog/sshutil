FROM ubuntu:20.04

RUN apt-get update && DEBIAN_FRONTEND="noninteractive" apt-get install -y \
  bash \
  openssh-server \
  && rm -rf /var/lib/apt/lists/*

ARG USER=testuser
ARG PASSWORD=testpassword
ARG PORT=2222

RUN ssh-keygen -A && mkdir -p /run/sshd

RUN useradd --create-home --shell /bin/bash ${USER}
RUN echo ${USER}:${PASSWORD} | chpasswd

RUN echo "Port ${PORT}" >> /etc/ssh/sshd_config

CMD [ "sh", "-c", "/usr/sbin/sshd && sleep infinity" ]
