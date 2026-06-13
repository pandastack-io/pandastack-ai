SHELL := /bin/bash
.DEFAULT_GOAL := help

LIMA_NAME    ?= pandastack
LIMA_TEMPLATE := lima/microvm.yaml
AGENT_BIN    := bin/pandastack-agent
API_BIN      := bin/pandastack-api

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

setup: ## One-time: install Lima + boot the nested-virt VM
	@command -v limactl >/dev/null || { echo "Install Lima first: brew install lima"; exit 1; }
	limactl list --quiet | grep -qx "$(LIMA_NAME)" || \
		limactl start --tty=false --name=$(LIMA_NAME) $(LIMA_TEMPLATE)

smoke: ## Run a smoke test that boots a throw-away microVM inside Lima
	limactl shell $(LIMA_NAME) -- bash -lc 'firecracker --version && ls -l /dev/kvm'

agent: ## Build the agent for linux/arm64 (target = Lima VM)
	mkdir -p bin
	cd agent && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../$(AGENT_BIN) ./cmd/agent

deploy-agent: agent ## Copy the agent into Lima and (re)start it
	limactl copy $(AGENT_BIN) $(LIMA_NAME):/tmp/pandastack-agent
	limactl shell $(LIMA_NAME) -- sudo install -m 0755 /tmp/pandastack-agent /usr/local/bin/pandastack-agent
	limactl shell $(LIMA_NAME) -- sudo pkill -x pandastack-agent || true
	limactl shell $(LIMA_NAME) -- bash -lc 'nohup sudo /usr/local/bin/pandastack-agent >/tmp/pandastack-agent.log 2>&1 & disown; sleep 1; tail -n 20 /tmp/pandastack-agent.log'

api: ## Build & run the control-plane API on the Mac
	cd api && go build -o ../$(API_BIN) ./cmd/api
	./$(API_BIN)

dashboard: ## Run the Next.js dashboard
	cd dashboard && npm install && npm run dev

test-api: ## Curl harness covering all Phase 1+2 endpoints (api + agent must be running)
	./scripts/api-tests.sh

test-apps: ## Curl harness for git-driven apps (Postgres-backed API + agent must be running)
	./scripts/apps-tests.sh

tidy: ## go mod tidy in every Go module
	cd agent && go mod tidy
	cd api   && go mod tidy

pg-migrate-up: ## Apply Postgres migrations using .env.local
	set -a; [ ! -f .env.local ] || source .env.local; set +a; cd agent && PANDASTACK_DB_DRIVER=postgres PANDASTACK_DB_DSN="$${DATABASE_DIRECT_URL:-$${PANDASTACK_DB_DSN}}" go run ./cmd/migrate up

pg-migrate-down: ## Roll back all Postgres migrations using .env.local
	set -a; [ ! -f .env.local ] || source .env.local; set +a; cd agent && PANDASTACK_DB_DRIVER=postgres PANDASTACK_DB_DSN="$${DATABASE_DIRECT_URL:-$${PANDASTACK_DB_DSN}}" go run ./cmd/migrate down

pg-migrate-status: ## Show Postgres migration status using .env.local
	set -a; [ ! -f .env.local ] || source .env.local; set +a; cd agent && PANDASTACK_DB_DRIVER=postgres PANDASTACK_DB_DSN="$${DATABASE_DIRECT_URL:-$${PANDASTACK_DB_DSN}}" go run ./cmd/migrate status

clean: ## Remove build artefacts
	rm -rf bin

destroy: ## Tear down the Lima VM
	limactl delete -f $(LIMA_NAME)

# Multi-node Terraform envs (the OSS deployment targets):
#   infra/terraform/envs/dev-aws        — AWS multi-node (ASG + ALB + RDS)
#   infra/terraform/envs/dev-gcp-multi  — GCP multi-node (MIG + LB + Cloud SQL)

tf-aws-init: ## Initialize Terraform for AWS multi-node infra
	terraform -chdir=infra/terraform/envs/dev-aws init

tf-aws-plan: ## Plan Terraform changes for AWS multi-node infra
	terraform -chdir=infra/terraform/envs/dev-aws plan -var-file=terraform.tfvars

tf-aws-apply: ## Apply Terraform changes for AWS multi-node infra
	terraform -chdir=infra/terraform/envs/dev-aws apply -var-file=terraform.tfvars -auto-approve

tf-aws-destroy: ## Destroy AWS multi-node infra
	terraform -chdir=infra/terraform/envs/dev-aws destroy -var-file=terraform.tfvars -auto-approve

tf-aws-output: ## Show Terraform outputs for AWS multi-node infra
	terraform -chdir=infra/terraform/envs/dev-aws output

tf-gcp-init: ## Initialize Terraform for GCP multi-node infra
	terraform -chdir=infra/terraform/envs/dev-gcp-multi init

tf-gcp-plan: ## Plan Terraform changes for GCP multi-node infra
	terraform -chdir=infra/terraform/envs/dev-gcp-multi plan -var-file=terraform.tfvars

tf-gcp-apply: ## Apply Terraform changes for GCP multi-node infra
	terraform -chdir=infra/terraform/envs/dev-gcp-multi apply -var-file=terraform.tfvars -auto-approve

tf-gcp-destroy: ## Destroy GCP multi-node infra
	terraform -chdir=infra/terraform/envs/dev-gcp-multi destroy -var-file=terraform.tfvars -auto-approve

tf-gcp-output: ## Show Terraform outputs for GCP multi-node infra
	terraform -chdir=infra/terraform/envs/dev-gcp-multi output

.PHONY: help setup smoke agent deploy-agent api dashboard test-api test-apps tidy pg-migrate-up pg-migrate-down pg-migrate-status clean destroy tf-aws-init tf-aws-plan tf-aws-apply tf-aws-destroy tf-aws-output tf-gcp-init tf-gcp-plan tf-gcp-apply tf-gcp-destroy tf-gcp-output
