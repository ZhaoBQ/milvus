#!/bin/bash

# Copyright 2018 Istio Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e
set -u
set -x

SOURCE="${BASH_SOURCE[0]}"
while [ -h "$SOURCE" ]; do # resolve $SOURCE until the file is no longer a symlink
  DIR="$( cd -P "$( dirname "$SOURCE" )" && pwd )"
  SOURCE="$(readlink "$SOURCE")"
  [[ $SOURCE != /* ]] && SOURCE="$DIR/$SOURCE" # if $SOURCE was a relative symlink, we need to resolve it relative to the path where the symlink file was located
done
ROOT="$( cd -P "$( dirname "$SOURCE" )/../.." && pwd )"

while (( "$#" )); do
  case "$1" in

    --release-name)
      RELEASE_NAME=$2
      shift 2
    ;;

    --artifacts-name)
      ARTIFACTS_NAME=$2
      shift 2
    ;;

    --log-dir)
      LOG_DIR=$2
      shift 2
    ;;

    -h|--help)
      { set +x; } 2>/dev/null
      HELP="
Usage:
  $0 [flags] [Arguments]
    
    --log-dir                  Log Path

    --artifacts-name           Artifacts Name

    --release-name             Milvus helm release name

    -h or --help                Print help information


Use \"$0  --help\" for more information about a given command.
"
      echo -e "${HELP}" ; exit 0
    ;;
    -*)
      echo "Error: Unsupported flag $1" >&2
      exit 1
      ;;
    *) # preserve positional arguments
      PARAMS+=("$1")
      shift
      ;;
  esac
done


find ${LOG_DIR:-/ci-logs/} -type f  -name "*${RELEASE_NAME:-milvus-testing}*" \
| xargs tar -zcvf ${ARTIFACTS_NAME:-artifacts}.tar.gz --remove-files || true