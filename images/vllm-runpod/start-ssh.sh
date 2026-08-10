#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${PUBLIC_KEY:-}" ]]; then
  touch /root/.ssh/authorized_keys
  if ! grep -Fqx -- "$PUBLIC_KEY" /root/.ssh/authorized_keys; then
    printf '%s\n' "$PUBLIC_KEY" >>/root/.ssh/authorized_keys
  fi
  chmod 600 /root/.ssh/authorized_keys
fi

exec /usr/sbin/sshd -D -e
