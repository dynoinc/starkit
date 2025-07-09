default: test

gen:
	go tool sqlc generate -f ragstore/sqlc.yaml
	go tool sqlc generate -f workflow/sqlc.yaml

	go mod tidy
	go generate ./...
	go tool goimports -local=github.com/dynoinc/starkit -w .

lint: gen
	go vet ./...
	go tool staticcheck ./...
	go tool govulncheck ./...

test: lint
	go mod verify
	go build ./...
	go test -v -race ./...

pgshell:
	docker exec -it ratchet-db psql -U postgres -d termichat
        
pgreset:
	docker exec -it ratchet-db psql -U postgres -c "DROP DATABASE termichat"
	docker exec -it ratchet-db psql -U postgres -c "CREATE DATABASE termichat"