.PHONY: up down logs test test-integration smoke bench compare chaos-on chaos-off

STRATEGY ?= redis
BUYERS   ?= 5000
STOCK    ?= 500

up:                ## Nyalakan infra + app Go
	docker compose up -d --build

down:              ## Matikan semua container
	docker compose down

logs:
	docker compose logs -f app-go

test:              ## Unit test + vet
	cd impl/go && go vet ./... && go test ./...

test-integration:  ## Test race condition ke Postgres & Redis (jalankan `make up` dulu)
	cd impl/go && go test -count=1 -tags integration ./internal/integration_test/ -v

smoke:             ## Uji kontrak untuk semua strategi
	@for s in naive forupdate redis queue; do \
		echo "== $$s"; \
		docker compose --profile bench run --rm -e STRATEGY=$$s k6 run --quiet /bench/k6/smoke.js || exit 1; \
	done

bench:             ## Satu run: make bench STRATEGY=queue BUYERS=10000 STOCK=1000
	./bench/run.sh $(STRATEGY) $(BUYERS) $(STOCK)

compare:           ## Bandingkan keempat strategi dengan beban yang sama
	./bench/compare.sh $(BUYERS) $(STOCK)

chaos-on:          ## Bikin Postgres lambat (+40ms ±10ms) lewat Toxiproxy
	./bench/chaos.sh on 40 10

chaos-off:
	./bench/chaos.sh off
