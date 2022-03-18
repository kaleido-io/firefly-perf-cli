#!/bin/bash
# Colors
GREEN='\033[1;32m'
PURPLE='\033[0;35m'
RED='\033[0;31m'
NC='\033[0m' # No Color

BASE_PATH=~/ffperf-testing

# Verify three arguments were given
if [ $# -ne 3 ]; then
    printf "${RED}Must provide exactly three arguments: \n1. Old stack name to remove \n2. New stack name to create \n3. Stack's blockchain type (ex. geth, besu, fabric, corda) \nex: ./prep.sh old_stack new_stack geth${NC}\n"
    exit 1
fi

OLD_STACK_NAME=$1
NEW_STACK_NAME=$2
BLOCKCHAIN_PROVIDER=$3

# Kill existing ffperf processes
printf "${PURPLE}Killing ffperf processes...\n${NC}"
pkill -f 'ffperf'
rm $BASE_PATH/ffperf.log

# Install local ffperf-cli
printf "${PURPLE}Installing local ffperf CLI...\n${NC}"
cd $BASE_PATH/firefly-perf-cli
make install

# Build firefly image
printf "${PURPLE}Building FireFly Image...\n${NC}"
cd $BASE_PATH/firefly
make docker

cd $BASE_PATH
# Remove old Firefly stack
printf "${PURPLE}Removing FireFly Stack: $OLD_STACK_NAME...\n${NC}"
ff remove -f $OLD_STACK_NAME

# Create new Firefly stack
printf "${PURPLE}Creating FireFly Stack: $NEW_STACK_NAME...\n${NC}"
ff init $NEW_STACK_NAME 2 --manifest $BASE_PATH/firefly/manifest.json -t erc1155 -d postgres -b $BLOCKCHAIN_PROVIDER --prometheus-enabled
cat ~/.firefly/stacks/$NEW_STACK_NAME/docker-compose.yml | yq '
  .services.firefly_core_0.logging.options.max-file = "250" |
  .services.firefly_core_0.logging.options.max-size = "500m"
  ' > /tmp/docker-compose.yml && cp /tmp/docker-compose.yml ~/.firefly/stacks/$NEW_STACK_NAME/docker-compose.yml

printf "${PURPLE}Starting FireFly Stack: $NEW_STACK_NAME...\n${NC}"
ff start $NEW_STACK_NAME

# Get org identity
ORG_IDENTITY=$(curl http://localhost:5000/api/v1/network/organizations | jq -r '.[0].did')
ORG_ADDRESS=$(cat ~/.firefly/stacks/$NEW_STACK_NAME/stack.json | jq -r '.members[0].address')
cd $BASE_PATH

printf ${PURPLE}"Deploying custom test contract...\n${NC}"

cat <<EOF > $BASE_PATH/instances.yaml
stackJSONPATH: ${HOME}/.firefly/stacks/$NEW_STACK_NAME/stack.json

wsConfig:
  wsPath: /ws
  readBufferSize: 16000
  writeBufferSize: 16000
  initialDelay: 250ms
  maximumDelay: 30s
  initialConnectAttempts: 5

instances:
  - name: ff0-ff1-msg-broadcast
    test: msg_broadcast
    length: 5m
    recipient: ${ORG_IDENTITY}
    recipientAddress: ${ORG_ADDRESS}
    workers: 10
    messageOptions:
      longMessage: true
  - name: ff0-ff1-msg-private
    test: msg_private
    length: 5m
    recipient: ${ORG_IDENTITY}
    recipientAddress: ${ORG_ADDRESS}
    workers: 10
    messageOptions:
      longMessage: true
  - name: ff0-ff1-blob-broadcast
    test: blob_broadcast
    length: 5m
    recipient: ${ORG_IDENTITY}
    recipientAddress: ${ORG_ADDRESS}
    workers: 10
    messageOptions:
      longMessage: true
  - name: ff0-ff1-blob-private
    test: blob_private
    length: 5m
    recipient: ${ORG_IDENTITY}
    recipientAddress: ${ORG_ADDRESS}
    workers: 10
    messageOptions:
      longMessage: true
EOF

if [ "$BLOCKCHAIN_PROVIDER" == "geth" ]; then
    output=$(ff deploy $NEW_STACK_NAME ./firefly/test/data/simplestorage/simple_storage.json | grep address)
    prefix='contract address: '
    CONTRACT_ADDRESS=${output#"$prefix"}
    FLAGS="$FLAGS -a $CONTRACT_ADDRESS"
    JOBS="$JOBS token_mint custom_ethereum_contract"
    cat <<EOF >> $BASE_PATH/instances.yaml
  - name: ff1-ff2-mint
    test: token_mint
    length: 5m
    recipient: ${ORG_IDENTITY}
    recipientAddress: ${ORG_ADDRESS}
    workers: 10
    tokenOptions:
      tokenType: fungible
  - name: ff1-ff2-contract
    test: custom_ethereum_contract
    length: 5m
    recipient: ${ORG_IDENTITY}
    recipientAddress: ${ORG_ADDRESS}
    workers: 10
    contractOptions:
      address: ${CONTRACT_ADDRESS}
EOF
fi

if [ "$BLOCKCHAIN_PROVIDER" == "fabric" ]; then
    docker run --rm -v $BASE_PATH/firefly/test/data/assetcreator:/chaincode-go hyperledger/fabric-tools:2.4 peer lifecycle chaincode package /chaincode-go/package.tar.gz --path /chaincode-go --lang golang --label assetcreator
    output=$(ff deploy $NEW_STACK_NAME ./firefly/test/data/assetcreator/package.tar.gz firefly assetcreator 1.0)
    cat <<EOF >> $BASE_PATH/instances.yaml
  - name: ff1-ff2-contract
    test: custom_fabric_contract
    length: 5m
    recipient: ${ORG_IDENTITY}
    recipientAddress: ${ORG_ADDRESS}
    workers: 10
    contractOptions:
      channel: firefly
      chaincode: assetcreator
EOF


EOF
fi

printf "${PURPLE}Modify $BASE_PATH/instances.yaml and the commnd below and run...\n${NC}"
printf "${GREEN}nohup ffperf run -c $BASE_PATH/instances.yaml -n ff0-ff1-broadcast &> ffperf.log &${NC}\n"

# Create markdown for Perf Test
printf "\n${RED}*** Before Starting Test ***${NC}\n"
printf "${PURPLE}*** Add the following entry to https://github.com/hyperledger/firefly/issues/519 ***${NC}\n"
printf "\n| $(uuidgen) | $(TZ=":US/Eastern" date +%m_%d_%Y_%I_%M_%p) | *Add Snapshot After Test* | $(TZ=":US/Eastern" date +%m_%d_%Y_%I_%M_%p) | *Add After Test* | *Add After Test* | $BLOCKCHAIN_PROVIDER | *Add Num Broadcasts* | *Add Num Private* | *Add Num Minters* | *Add Num On-Chain* | *Add related Github Issue* | $(cd $BASE_PATH/firefly;git rev-parse --short HEAD) | *Add After Test* | $(echo $(jq -r 'to_entries[] | "\(.key):\(.value .sha)"' $BASE_PATH/firefly/manifest.json)// )|\n"