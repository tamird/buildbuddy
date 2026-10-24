#!/bin/busybox sh
set -eu
finish() {
    code=$?
    echo "NETEM_EXIT=$code"
    /bin/busybox poweroff -f
    exit "$code"
}
trap finish EXIT
/bin/busybox mkdir -p /dev /proc /sys /tmp
/bin/busybox mount -t devtmpfs devtmpfs /dev
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
test "$(/bin/busybox uname -r)" = 6.18.57
/bin/busybox uname -a
/usr/sbin/ip -Version
/usr/sbin/tc -Version
/usr/sbin/ip link add netem-left type veth peer name netem-right
/usr/sbin/ip link set netem-left up
/usr/sbin/ip link set netem-right up
/usr/sbin/tc qdisc add dev netem-left root netem limit 100 delay 50ms rate 20mbit
echo NETEM_ADDED
/usr/sbin/tc qdisc show dev netem-left
/usr/sbin/tc qdisc change dev netem-left root netem limit 100 delay 5ms rate 20mbit
echo NETEM_CHANGED
/usr/sbin/tc qdisc show dev netem-left
/usr/sbin/tc qdisc del dev netem-left root
/usr/sbin/ip link del netem-left
if /usr/sbin/ip link show netem-left; then exit 1; fi
if /usr/sbin/ip link show netem-right; then exit 1; fi
echo NETEM_CLEANED
