.PHONY: test fmt vet check proto check-proto

PROTO_FILES := $(wildcard proto/le0x/v1/*.proto)
PROTO_FLAGS := --proto_path=. --go_opt=module=github.com/le0xdon/le0xfarm --go-grpc_opt=module=github.com/le0xdon/le0xfarm

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

check: check-proto test vet
	@files=$$(go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .CgoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .TestGoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}' ./...) || exit 1; \
	unformatted=$$(printf '%s\n' "$$files" | while IFS= read -r file; do if [ -n "$$file" ]; then gofmt -l "$$file" || exit 1; fi; done) || exit 1; \
	if [ -n "$$unformatted" ]; then printf '%s\n' "$$unformatted" 'Run make fmt'; exit 1; fi

proto:
	protoc $(PROTO_FLAGS) --go_out=. --go-grpc_out=. $(PROTO_FILES)

check-proto:
	@set -eu; \
	tmp=$$(mktemp -d .proto-check.XXXXXX); \
	trap 'rm -rf "$$tmp"' EXIT HUP INT TERM; \
	protoc $(PROTO_FLAGS) --go_out="$$tmp" --go-grpc_out="$$tmp" $(PROTO_FILES); \
	for file in proto/le0x/v1/*.pb.go "$$tmp"/proto/le0x/v1/*.pb.go; do \
	  relative=$${file#$$tmp/}; \
	  if ! cmp -s "$$relative" "$$tmp/$$relative"; then \
	    printf '%s\n' "Generated file missing, extra or stale: $$relative" 'Run make proto; remove obsolete generated files.'; \
	    exit 1; \
	  fi; \
	done
