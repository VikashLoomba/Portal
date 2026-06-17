#!/bin/bash
# Discover loopback dev ports on the remote dev box.
#
# Argv layout: <deny ports...> -- <allow ports...>
# We use the literal "--" sentinel because ssh joins all command words with
# spaces; sending DENY and ALLOW as quoted shell variables would be re-split
# by the remote shell and silently corrupted.
#
# Selection rules (mirror the bash original):
#   * loopback only (127.0.0.0/8 or ::1)
#   * allowlist OVERRIDES every exclusion
#   * exclude denylist (system services)
#   * exclude the kernel ephemeral range (read /proc; fallback 32768-60999)

DENY=""; ALLOW=""; seen_sep=0
for tok in "$@"; do
  if [ "$tok" = "--" ]; then seen_sep=1; continue; fi
  if [ "$seen_sep" -eq 0 ]; then DENY="$DENY $tok"; else ALLOW="$ALLOW $tok"; fi
done

EPHEM_MIN=$(cut -f1 /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null); [ -n "$EPHEM_MIN" ] || EPHEM_MIN=32768
EPHEM_MAX=$(cut -f2 /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null); [ -n "$EPHEM_MAX" ] || EPHEM_MAX=60999

ss -Htln | awk -v emin="$EPHEM_MIN" -v emax="$EPHEM_MAX" -v deny="$DENY" -v allow="$ALLOW" '
  BEGIN {
    n = split(deny, d, " ");  for (i = 1; i <= n; i++) bad[d[i]+0] = 1
    m = split(allow, a, " "); for (i = 1; i <= m; i++) ok[a[i]+0] = 1
  }
  {
    addr = $4                      # local address:port column
    sub(/%[^:]*:/, ":", addr)      # strip zone id: 127.0.0.53%lo -> 127.0.0.53
    p = addr; sub(/.*:/, "", p)    # port = after last colon
    h = addr; sub(/:[0-9]+$/, "", h)
    gsub(/[][]/, "", h)            # strip [ ] from IPv6 literal
    if (!(h ~ /^127\./ || h == "::1")) next       # loopback only
    if (p+0 in ok) { print p+0; next }            # allowlist overrides exclusions
    if (p+0 in bad) next                          # system service
    if (p+0 >= emin+0 && p+0 <= emax+0) next      # ephemeral range
    print p+0
  }' | sort -nu
