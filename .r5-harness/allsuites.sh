#!/bin/bash
for c in ${SUITES:-c1 c2 c3}; do
  d=/tmp/r5-$c
  (cd $d && { echo "commit $(git rev-parse --short HEAD)"; echo "gofmt: [$(gofmt -l . | tr '\n' ' ')]"; go vet ./... 2>&1 && echo "vet: clean" || echo "vet: FAILED"; } > /tmp/r5-final-$c-static.txt)
  bash /tmp/suite5.sh $d final-$c
done
touch /tmp/r5-allsuites.done
