#!/bin/bash
export GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig
P=/tmp/probe5.sh
bash $P fin-ca-none neither codex "" neither check-agents
bash $P fin-ca-det-host detected host "" detected check-agents
bash $P fin-ca-host-neither neither codex "" neither check-agents-host
bash $P fin-ca-host-detected detected codex "" detected check-agents-host
bash $P fin-doctor-det detected host "" detected doctor
bash $P fin-doctor-det-nohost detected codex "" detected doctor
bash $P fin-doctor-none-host neither host "" neither doctor
bash $P fin-doctor-mismatch 0 host "" 1 doctor
bash $P fin-eq-none-auto neither auto "" neither enqueue
bash $P fin-eq-det-auto detected auto "" detected enqueue
bash $P fin-eq-1-auto 1 auto "" 1 enqueue
bash $P fin-eq-1-gemini-auto 1 gemini auto 1 enqueue
PROBE_STUBS=codex bash $P fin-fo-none-auto neither codex auto neither failover
PROBE_STUBS=codex bash $P fin-fo-det-auto detected codex auto detected failover
PROBE_STUBS=codex bash $P fin-fo-1-auto 1 codex auto 1 failover
PROBE_STUBS=codex bash $P fin-fo-none-hostbackup neither codex host neither failover
PROBE_STUBS=codex bash $P fin-fo-det-hostbackup detected codex host detected failover
PROBE_STUBS="codex claude" bash $P fin-fo-none-next neither codex auto neither failover
touch /tmp/r5-probes-final.done
