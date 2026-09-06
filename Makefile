.PHONY: test fmt vet check

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

check: test vet
	@files=$$(go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .CgoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .TestGoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}}{{"\n"}}{{end}}' ./...) || exit 1; \
	unformatted=$$(printf '%s\n' "$$files" | while IFS= read -r file; do if [ -n "$$file" ]; then gofmt -l "$$file" || exit 1; fi; done) || exit 1; \
	if [ -n "$$unformatted" ]; then printf '%s\n' "$$unformatted" 'Run make fmt'; exit 1; fi
