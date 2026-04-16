#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="letshare.service"
ACTION="${1:-help}"

case "$ACTION" in
  start)
    systemctl start "$SERVICE_NAME"
    ;;
  stop)
    systemctl stop "$SERVICE_NAME"
    ;;
  restart)
    systemctl restart "$SERVICE_NAME"
    ;;
  status)
    systemctl status "$SERVICE_NAME" --no-pager
    ;;
  logs)
    journalctl -u "$SERVICE_NAME" -f
    ;;
  logs-tail)
    journalctl -u "$SERVICE_NAME" -n 50 --no-pager
    ;;
  help|-h|--help)
    cat <<'EOF'
Usage: ./scripts/command.sh <action>

Actions:
  start      Start letshare.service
  stop       Stop letshare.service
  restart    Restart letshare.service
  status     Show service status
  logs       Follow service logs
  logs-tail  Show recent service logs
EOF
    ;;
  *)
    echo "Unknown action: $ACTION" >&2
    exit 1
    ;;
esac
