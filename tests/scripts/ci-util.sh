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


# Install pytest requirements
function install_pytest_requirements(){
 echo "Install pytest requirements"
 cd ${ROOT}/tests/python_client
 python3 -m pip install --no-cache-dir -r requirements.txt
}

function docker_login_ci_registry(){

    if [[ -z "${CI_REGISTRY_USERNAME:-}" || -z "${CI_REGISTRY_PASSWORD:-}" ]]; then 
       echo "Please setup docker credential for ci registry-${HUB}"
    else
        echo "docker login ci registry"
        docker login  -u ${CI_REGISTRY_USERNAME} -p ${CI_REGISTRY_PASSWORD} ${HUB}
    fi
}

