#!/bin/sh
set -eu

containers_ca=/etc/cloudflare/certs/cloudflare-containers-ca.crt
if [ -r "$containers_ca" ]; then
    cp "$containers_ca" /usr/local/share/ca-certificates/cloudflare-containers-ca.crt
    /usr/sbin/update-ca-certificates
fi

exec /usr/bin/setpriv --reuid=streamline --regid=streamline --init-groups -- "$@"
