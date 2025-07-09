default: test

gen:
	sqlc generate -f ragstore/sqlc.yaml
	sqlc generate -f workflow/sqlc.yaml

	go mod tidy
	go generate ./...
	goimports -local=github.com/dynoinc/starkit -w .

lint: gen
	go vet ./...
	staticcheck ./...
	govulncheck ./...

test: lint
	go mod verify
	go build ./...
	go test -v -race ./...

pgshell:
	docker exec -it ratchet-db psql -U postgres -d termichat
        
pg-reset:
	docker exec -it ratchet-db psql -U postgres -c "DROP DATABASE termichat"
	docker exec -it ratchet-db psql -U postgres -c "CREATE DATABASE termichat"