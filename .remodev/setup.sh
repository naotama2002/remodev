#!/bin/bash
# Runs once after the first clone (inside the sandbox, through the egress proxy).
set -euo pipefail
go version
go build ./... && echo "go build ok"
