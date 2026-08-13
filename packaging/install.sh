#!/bin/sh
# Install bdplay on a BirdDog PLAY. Run as root on the device, or from the
# .fw package's update script.
set -e

DEST=/userdata/bd-play
mkdir -p "$DEST"

# The binary is expected next to this script.
HERE=$(cd "$(dirname "$0")" && pwd)
install -m 0755 "$HERE/bdplay" "$DEST/bdplay"
[ -f "$HERE/mutool" ] && install -m 0755 "$HERE/mutool" "$DEST/mutool" || true
[ -f "$HERE/mount.exfat-fuse" ] && install -m 0755 "$HERE/mount.exfat-fuse" "$DEST/mount.exfat-fuse" || true

install -m 0644 "$HERE/bd-play.service" /etc/systemd/system/bd-play.service

systemctl daemon-reload
systemctl enable bd-play.service
systemctl restart bd-play.service

echo "bdplay installed; control UI on http://$(hostname):8091/"
