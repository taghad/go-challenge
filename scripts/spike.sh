#!/bin/sh
# Collects docker stats of one compose project as CSV, until it is stopped. The ramp and
# the saturation rule are in cmd/uss; this script only records resource use for the same
# period. The cmd/uss report has the start and end time of each step.
#
# Usage: scripts/spike.sh <compose project> > stats.csv

project=${1:?usage: scripts/spike.sh <compose project>}

echo "timestamp,container,cpu_percent,memory"
while :; do
	now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' |
		grep "^$project-" |
		sed "s|^|$now,|"
done
