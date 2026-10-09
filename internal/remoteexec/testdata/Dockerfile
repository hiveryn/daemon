FROM alpine:3.22
RUN apk add --no-cache openssh tmux git nodejs bash && \
    adduser -D worker && passwd -u worker && \
    mkdir -p /home/worker/.ssh /home/worker/repo /run/sshd && \
    chown -R worker:worker /home/worker && ssh-keygen -A
COPY sshd_config /etc/ssh/sshd_config
CMD ["/usr/sbin/sshd", "-D", "-e"]
