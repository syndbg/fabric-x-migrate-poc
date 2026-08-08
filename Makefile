SHELL := /bin/sh

FABRIC_VERSION ?= 3.1.5
STATE_DATABASE ?= goleveldb
CHANNEL ?= migration
FABRIC_SAMPLES_COMMIT := $(shell cat fabric-samples.commit)
GOLANGCI_LINT_VERSION ?= v2.4.0

HACK_DIR := $(CURDIR)/hack
FABRIC_SAMPLES := $(HACK_DIR)/fabric-samples
FABRIC_RELEASE := $(HACK_DIR)/fabric/$(FABRIC_VERSION)
FABRIC_BIN := $(FABRIC_RELEASE)/bin
FABRIC_CONFIG := $(FABRIC_RELEASE)/config
FABRIC_ARCHIVE := $(HACK_DIR)/hyperledger-fabric-$(shell go env GOOS)-$(shell go env GOARCH)-$(FABRIC_VERSION).tar.gz
HACK_COMPOSE := docker compose -f $(HACK_DIR)/compose.yaml
HACK_BLOCK := $(HACK_DIR)/$(CHANNEL).block
ORDERER_CA := $(HACK_DIR)/crypto/ordererOrganizations/example.com/tlsca/tlsca.example.com-cert.pem
ORDERER_CERT := $(HACK_DIR)/crypto/ordererOrganizations/example.com/orderers/orderer.example.com/tls/server.crt
ORDERER_KEY := $(HACK_DIR)/crypto/ordererOrganizations/example.com/orderers/orderer.example.com/tls/server.key
PEER_CA := $(HACK_DIR)/crypto/peerOrganizations/org1.example.com/tlsca/tlsca.org1.example.com-cert.pem
PEER_MSP := $(HACK_DIR)/crypto/peerOrganizations/org1.example.com/users/Admin@org1.example.com/msp
ORDERER_ADMIN_ARGS := -o localhost:18053 --ca-file $(ORDERER_CA) --client-cert $(ORDERER_CERT) --client-key $(ORDERER_KEY)
PEER_ENV := FABRIC_CFG_PATH=$(FABRIC_CONFIG) CORE_PEER_TLS_ENABLED=true CORE_PEER_LOCALMSPID=Org1MSP CORE_PEER_MSPCONFIGPATH=$(PEER_MSP) CORE_PEER_ADDRESS=localhost:18051 CORE_PEER_TLS_ROOTCERT_FILE=$(PEER_CA)

.PHONY: help build test test-integration lint lint-fix regen-proto hack-samples hack-fabric run-hack stop-hack hack-status

help:
	@printf '%s\n' \
		'build             build artifacts/bin/fabric-x-migrate' \
		'test              run unit tests' \
		'test-integration  run Fabric export integration tests' \
		'lint              run golangci-lint' \
		'lint-fix          run golangci-lint with safe fixes' \
		'regen-proto       regenerate protobuf Go' \
		'hack-samples      checkout the pinned fabric-samples commit' \
		'hack-fabric       download the selected Fabric release' \
		'run-hack          start and join the local Fabric source network' \
		'stop-hack         stop the local network and delete its volumes' \
		'hack-status       show local network containers'

build:
	@mkdir -p artifacts/bin
	go build -o artifacts/bin/fabric-x-migrate ./cmd/fabric-x-migrate

test:
	go test ./... -v

test-integration:
	go test -tags=integration ./internal/cmd -v

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

lint-fix:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --fix

regen-proto:
	go generate ./...

hack-samples:
	@test -d $(FABRIC_SAMPLES)/.git || git clone --filter=blob:none --depth=1 https://github.com/hyperledger/fabric-samples.git $(FABRIC_SAMPLES)
	@if test "`git -C $(FABRIC_SAMPLES) rev-parse HEAD`" != "$(FABRIC_SAMPLES_COMMIT)"; then git -C $(FABRIC_SAMPLES) fetch --depth=1 origin $(FABRIC_SAMPLES_COMMIT) && git -C $(FABRIC_SAMPLES) checkout --detach $(FABRIC_SAMPLES_COMMIT); fi
	@test "`git -C $(FABRIC_SAMPLES) rev-parse HEAD`" = "$(FABRIC_SAMPLES_COMMIT)"

hack-fabric: $(FABRIC_BIN)/peer

$(FABRIC_BIN)/peer:
	@mkdir -p $(FABRIC_RELEASE)
	curl -fL https://github.com/hyperledger/fabric/releases/download/v$(FABRIC_VERSION)/$(notdir $(FABRIC_ARCHIVE)) -o $(FABRIC_ARCHIVE)
	tar -xzf $(FABRIC_ARCHIVE) -C $(FABRIC_RELEASE)
	rm $(FABRIC_ARCHIVE)

$(HACK_DIR)/crypto/peerOrganizations/org1.example.com/users/Admin@org1.example.com/msp: $(FABRIC_BIN)/peer
	cd $(HACK_DIR) && $(abspath $(FABRIC_BIN))/cryptogen generate --config=crypto-config.yaml --output=crypto

$(HACK_BLOCK): $(HACK_DIR)/configtx.yaml $(HACK_DIR)/crypto/peerOrganizations/org1.example.com/users/Admin@org1.example.com/msp
	@test -x $(FABRIC_BIN)/configtxgen || { echo 'set FABRIC_BIN to a Fabric release bin directory'; exit 1; }
	cd $(HACK_DIR) && FABRIC_CFG_PATH=$(HACK_DIR) $(abspath $(FABRIC_BIN))/configtxgen -profile MigrationChannel -channelID $(CHANNEL) -outputBlock $(HACK_BLOCK)

run-hack: hack-samples $(HACK_BLOCK)
	FABRIC_VERSION=$(FABRIC_VERSION) STATE_DATABASE=$(STATE_DATABASE) $(HACK_COMPOSE) up -d
	@until $(FABRIC_BIN)/osnadmin channel list $(ORDERER_ADMIN_ARGS) >/dev/null 2>&1; do sleep 1; done
	@$(FABRIC_BIN)/osnadmin channel list $(ORDERER_ADMIN_ARGS) | grep -q $(CHANNEL) || $(FABRIC_BIN)/osnadmin channel join --channelID $(CHANNEL) --config-block $(HACK_BLOCK) $(ORDERER_ADMIN_ARGS)
	@until $(PEER_ENV) $(FABRIC_BIN)/peer channel list >/dev/null 2>&1; do sleep 1; done
	@$(PEER_ENV) $(FABRIC_BIN)/peer channel list | grep -q $(CHANNEL) || $(PEER_ENV) $(FABRIC_BIN)/peer channel join -b $(HACK_BLOCK)

stop-hack:
	FABRIC_VERSION=$(FABRIC_VERSION) STATE_DATABASE=$(STATE_DATABASE) $(HACK_COMPOSE) down --volumes --remove-orphans

hack-status:
	FABRIC_VERSION=$(FABRIC_VERSION) STATE_DATABASE=$(STATE_DATABASE) $(HACK_COMPOSE) ps
