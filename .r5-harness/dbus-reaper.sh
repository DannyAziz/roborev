#!/bin/bash
# Kills orphaned session dbus-daemons (ppid 1) older than 120s; logs counts.
while true; do
  n=$(ps -o pid=,ppid=,etimes= -C dbus-daemon | awk '$2==1 && $3>120{print $1}' | tee >(xargs -r kill) | wc -l)
  echo "$(date +%T) reaped=$n live=$(pgrep -fc 'dbus-daemon --syslog --fork') inotify=$(ls -l /proc/*/fd 2>/dev/null | grep -c anon_inode:inotify)" >> /tmp/dbus-reaper.log
  sleep 30
done
